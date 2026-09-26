//go:build windows

// Claude Usage Widget — a Windows 11 taskbar pill + flyout panel showing the
// live Claude plan usage (5-hour window, weekly limit, per-model breakdown),
// implemented after the Claude Design mock "Claude Usage Widget.dc.html".
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/Parms221/claude-usage-widget/internal/api"
	"github.com/Parms221/claude-usage-widget/internal/auth"
	"github.com/Parms221/claude-usage-widget/internal/cache"
	"github.com/Parms221/claude-usage-widget/internal/config"
	"github.com/Parms221/claude-usage-widget/internal/history"
	"github.com/Parms221/claude-usage-widget/internal/ui"
	"github.com/Parms221/claude-usage-widget/internal/vm"
	"github.com/Parms221/claude-usage-widget/internal/winutil"
)

const appName = "ClaudeUsageWidget"

const (
	// The usage endpoint is stingy: polling it too often earns a 429 that keeps
	// renewing itself, so every retry is spaced out and grows on repeat.
	firstBackoff = 5 * time.Minute
	maxBackoff   = time.Hour
	// A manual "Actualizar ahora" skips the poll interval but not this floor.
	manualCooldown = 45 * time.Second
	// With an idle 5-hour window there is nothing new to read: poll 3x slower.
	idlePollFactor = 3
	// The view is rebuilt from memory this often so the countdown, the
	// "updated at" hint and the retry timer stay honest between fetches.
	renderInterval = 30 * time.Second
	maxLogBytes    = 512 * 1024
)

type app struct {
	cfg     *config.Config
	ui      *ui.UI
	scanner *history.Scanner

	poke chan struct{}

	mu             sync.Mutex
	usage          *api.Usage
	hist           *history.Stats
	creds          *auth.Credentials
	status         string
	statusMsg      string
	rateLimited    bool // last failure was a 429: the message is recomputed live
	lastCLIRefresh time.Time
	lastAttempt    time.Time // last request actually sent to the endpoint
	rlStrikes      int       // consecutive 429s, drives the exponential backoff
	backoffUntil   time.Time
	autostart      bool // cached: the logon scheduled task exists
}

func main() {
	runtime.LockOSThread()

	debug := flag.Bool("debug", false, "habilita DevTools y log detallado")
	flag.Parse()

	if !winutil.EnsureSingleInstance(appName) {
		// Another copy is already running. Starting the exe again usually
		// means the user can't see it: ask that copy to show itself instead
		// of exiting without a trace.
		ui.NotifyRunning()
		return
	}
	winutil.SetDPIAware()

	if f := openLog(config.Dir()); f != nil {
		log.SetOutput(f)
		defer f.Close()
	}
	log.Printf("claude-usage-widget iniciando (debug=%v, pid=%d)", *debug, os.Getpid())

	a := &app{
		cfg:     config.Load(),
		scanner: history.NewScanner(),
		poke:    make(chan struct{}, 1),
		status:  "loading",
	}
	// Show the last known reading right away: while the endpoint is rate
	// limited a cold start would otherwise sit at 0 % for minutes.
	if u := cache.Load(config.Dir()); u != nil {
		a.usage = u
		a.status = "stale"
		// Counts as the last attempt: restarting the widget several times in a
		// row used to fire one request each, which is how the 429 started.
		a.lastAttempt = u.FetchedAt
		log.Printf("caché: lectura de %s reutilizada al arrancar", u.FetchedAt.Format("2006-01-02 15:04"))
	}

	u, err := ui.New(a.cfg, ui.Callbacks{
		// Fires once per window (pill and panel) on every page load, including
		// a pill recreated by Explorer: repaint from memory, never poll — the
		// scheduler decides when the endpoint is due.
		OnReady:      a.pushCurrent,
		OnRefresh:    a.requestPoll,
		OnQuit:       func() { a.ui.Quit() },
		OnEnvChanged: a.pushCurrent,
		OnSetTheme: func(mode string) {
			a.cfg.Theme = mode
			if err := a.cfg.Save(); err != nil {
				log.Printf("guardando config: %v", err)
			}
			a.pushCurrent()
		},
		OnSetAutostart: func(enable bool) {
			go a.applyAutostart(enable)
		},
	}, *debug)
	if err != nil {
		winutil.MsgBox("Claude Usage Widget", err.Error()+
			"\n\nInstala el runtime de WebView2 (incluido en Windows 11) e inténtalo de nuevo.")
		return
	}
	a.ui = u
	log.Printf("ventana creada, acrílico=%v, poll=%ds", u.AcrylicOK(), a.cfg.PollSeconds)

	go a.pollLoop()
	go a.renderLoop()
	go a.initAutostart()
	go func() {
		// Right after logon the taskbar can still be settling; re-anchor the
		// windows a few times during the first two minutes.
		for i := 0; i < 12; i++ {
			time.Sleep(10 * time.Second)
			a.ui.Remeasure()
		}
	}()

	u.Run()
	log.Printf("saliendo")
}

// openLog appends to widget.log, rotating it once it grows past maxLogBytes.
// Appending (instead of truncating on every start) keeps the history across
// restarts — the only way to diagnose something that happened hours ago.
func openLog(dir string) *os.File {
	p := filepath.Join(dir, "widget.log")
	if fi, err := os.Stat(p); err == nil && fi.Size() > maxLogBytes {
		_ = os.Remove(p + ".1")
		_ = os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil
	}
	return f
}

// initAutostart resolves the cached autostart state and migrates any old
// HKCU\...\Run entry (unreliable for unsigned exes at logon) to the
// scheduled-task mechanism.
func (a *app) initAutostart() {
	enabled := winutil.LogonTaskExists(appName)
	if !enabled && winutil.IsAutostartEnabled(appName) {
		if exe, err := os.Executable(); err == nil {
			if err := winutil.CreateLogonTask(appName, exe, config.Dir()); err != nil {
				log.Printf("autostart migración: %v", err)
			} else {
				enabled = true
				_ = winutil.SetAutostart(appName, false)
				log.Printf("autostart migrado de clave Run a tarea programada")
			}
		}
	}
	a.mu.Lock()
	a.autostart = enabled
	a.mu.Unlock()
	a.pushCurrent()
}

// applyAutostart creates or deletes the logon task off the UI thread.
func (a *app) applyAutostart(enable bool) {
	var err error
	if enable {
		var exe string
		if exe, err = os.Executable(); err == nil {
			err = winutil.CreateLogonTask(appName, exe, config.Dir())
		}
	} else {
		err = winutil.DeleteLogonTask(appName)
		_ = winutil.SetAutostart(appName, false) // clean any legacy Run entry
	}
	if err != nil {
		log.Printf("autostart: %v", err)
	} else {
		a.mu.Lock()
		a.autostart = enable
		a.mu.Unlock()
		log.Printf("autostart=%v (tarea programada)", enable)
	}
	a.pushCurrent()
}

func (a *app) requestPoll() {
	select {
	case a.poke <- struct{}{}:
	default:
	}
}

// pollLoop owns every call to the usage endpoint. The next attempt is always
// computed from the last one, so a rate-limit backoff cannot be shortened by a
// stray tick and the timer never fires uselessly.
func (a *app) pollLoop() {
	if a.pollIsDue() {
		a.gather(false)
	}
	for {
		t := time.NewTimer(a.untilNextPoll())
		select {
		case <-t.C:
			a.gather(false)
		case <-a.poke:
			t.Stop()
			if wait := a.manualWait(); wait > 0 {
				log.Printf("refresh manual ignorado (faltan %v de enfriamiento)", wait.Round(time.Second))
				continue
			}
			a.gather(true)
		}
	}
}

// renderLoop refreshes the view from memory (no network) so the pill countdown
// and the retry hint keep ticking between fetches. It also re-checks that the
// pill still lives inside the taskbar: Explorer can reject or undo the
// SetParent while it starts, leaving the pill invisible.
func (a *app) renderLoop() {
	for range time.Tick(renderInterval) {
		a.ui.EnsureAttached()
		a.pushCurrent()
	}
}

// untilNextPoll returns how long to wait before the next endpoint call.
func (a *app) untilNextPoll() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()

	base := time.Duration(a.cfg.PollSeconds) * time.Second
	if u := a.usage; u != nil && u.FiveHour != nil && u.FiveHour.Utilization <= 0 {
		base *= idlePollFactor // nothing burning in the current window
	}
	next := a.lastAttempt.Add(base)
	if a.backoffUntil.After(next) {
		next = a.backoffUntil
	}
	if d := time.Until(next); d > time.Second {
		return d
	}
	return time.Second
}

// pollIsDue reports whether the cached reading is already old enough to be
// worth a request at startup.
func (a *app) pollIsDue() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lastAttempt.IsZero() {
		return true
	}
	return time.Since(a.lastAttempt) >= time.Duration(a.cfg.PollSeconds)*time.Second
}

// manualWait reports how much of the manual cooldown is left.
func (a *app) manualWait() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return time.Until(a.lastAttempt.Add(manualCooldown))
}

// gather fetches credentials, live usage and local history, then pushes the
// refreshed view-model. Runs on the poll goroutine only.
func (a *app) gather(manual bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Stamped up front: the scheduler paces itself off this, and an attempt
	// that dies before the request (no credentials) must still count as one.
	a.mu.Lock()
	a.lastAttempt = time.Now()
	a.mu.Unlock()

	creds, err := auth.Load()
	if err != nil {
		log.Printf("credenciales: %v", err)
		a.setStatus(nil, "auth", "Inicia sesión con `claude login`")
		return
	}

	if creds.Expired() {
		creds = a.tryCLIRefresh(ctx, creds)
	}

	usage, err := api.FetchUsage(ctx, creds.AccessToken)
	if errors.Is(err, api.ErrAuth) {
		log.Printf("token rechazado; intentando refresh vía CLI")
		creds = a.tryCLIRefresh(ctx, creds)
		usage, err = api.FetchUsage(ctx, creds.AccessToken)
	}
	if err != nil {
		a.handleFetchError(err, creds, manual)
		return
	}

	if err := cache.Save(config.Dir(), usage); err != nil {
		log.Printf("caché: %v", err)
	}

	weekStart := time.Now().AddDate(0, 0, -7)
	if usage.SevenDay != nil && usage.SevenDay.ResetsAt != nil {
		weekStart = usage.SevenDay.ResetsAt.AddDate(0, 0, -7)
	}
	hist, err := a.scanner.Scan(weekStart, a.cfg.HistoryDays)
	if err != nil {
		log.Printf("historial: %v", err)
	}

	a.mu.Lock()
	a.creds = creds
	a.usage = usage
	if hist != nil {
		a.hist = hist
	}
	a.status = "ok"
	a.statusMsg = ""
	a.rateLimited = false
	a.rlStrikes = 0
	a.backoffUntil = time.Time{}
	a.mu.Unlock()
	log.Printf("uso ok: 5h=%s 7d=%s", pctOf(usage.FiveHour), pctOf(usage.SevenDay))
	a.pushCurrent()
}

// handleFetchError turns a failed fetch into a status the panel can show
// without throwing away the numbers we already have.
func (a *app) handleFetchError(err error, creds *auth.Credentials, manual bool) {
	var rl *api.RateLimitError
	switch {
	case errors.As(err, &rl):
		wait := a.noteRateLimited(rl.RetryAfter, manual)
		a.mu.Lock()
		strikes := a.rlStrikes
		a.mu.Unlock()
		log.Printf("uso: 429 (%d seguidos, manual=%v, Retry-After=%v); próximo intento en %v",
			strikes, manual, rl.RetryAfter, wait.Round(time.Second))
		a.setStatus(creds, a.degradedStatus(), "")
	case errors.Is(err, api.ErrAuth):
		log.Printf("uso: %v", err)
		a.clearRateLimit()
		a.setStatus(creds, "auth", "Sesión expirada · abre Claude Code")
	default:
		log.Printf("uso: %v", err)
		a.clearRateLimit()
		a.setStatus(creds, a.degradedStatus(), "Sin conexión con el endpoint de uso")
	}
}

// degradedStatus keeps showing the cached numbers ("stale") whenever we have
// any, and only falls back to the bare error state on a cold start.
func (a *app) degradedStatus() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.usage != nil {
		return "stale"
	}
	return "error"
}

// noteRateLimited grows the backoff (5 → 10 → 20 → 40 → 60 min, jittered) and
// returns how long the next attempt is away. A manual retry never inflates the
// counter — the user pressing refresh should not push the widget into an hour
// long silence — but it does not shorten the wait either.
func (a *app) noteRateLimited(retryAfter time.Duration, manual bool) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !manual {
		a.rlStrikes++
	} else if a.rlStrikes == 0 {
		a.rlStrikes = 1
	}
	d := jitter(backoffFor(a.rlStrikes))
	if retryAfter > d {
		d = retryAfter
	}
	if until := time.Now().Add(d); until.After(a.backoffUntil) {
		a.backoffUntil = until
	}
	a.rateLimited = true
	return time.Until(a.backoffUntil)
}

func (a *app) clearRateLimit() {
	a.mu.Lock()
	a.rateLimited = false
	a.rlStrikes = 0
	a.backoffUntil = time.Time{}
	a.mu.Unlock()
}

// backoffFor doubles the wait per consecutive 429, capped at maxBackoff.
func backoffFor(strikes int) time.Duration {
	d := firstBackoff
	for i := 1; i < strikes; i++ {
		if d >= maxBackoff {
			break
		}
		d *= 2
	}
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

// jitter spreads retries by ±1/6 so several widgets (or restarts) do not line
// up on the same second.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	delta := int64(d) / 6
	return d + time.Duration(rand.Int63n(2*delta+1)-delta)
}

func pctOf(b *api.Bucket) string {
	if b == nil {
		return "—"
	}
	return vm.FmtPct(b.Utilization)
}

// tryCLIRefresh runs `claude -p .` (throttled) so the CLI renews the token,
// then reloads the credentials file. Returns the freshest credentials.
func (a *app) tryCLIRefresh(ctx context.Context, creds *auth.Credentials) *auth.Credentials {
	a.mu.Lock()
	recent := time.Since(a.lastCLIRefresh) < 10*time.Minute
	if !recent {
		a.lastCLIRefresh = time.Now()
	}
	a.mu.Unlock()
	if recent || !a.cfg.AutoRefreshCLI {
		return creds
	}
	log.Printf("token expirado; ejecutando claude -p . para refrescar")
	if err := auth.RefreshViaCLI(ctx); err != nil {
		log.Printf("refresh CLI: %v", err)
	}
	if fresh, err := auth.Load(); err == nil {
		return fresh
	}
	return creds
}

func (a *app) setStatus(creds *auth.Credentials, status, msg string) {
	a.mu.Lock()
	if creds != nil {
		a.creds = creds
	}
	a.status = status
	a.statusMsg = msg
	a.mu.Unlock()
	a.pushCurrent()
}

// pushCurrent rebuilds the view-model from the last known state and sends it
// to the page. Safe from any goroutine.
func (a *app) pushCurrent() {
	if a.ui == nil {
		return
	}
	a.mu.Lock()
	plan := a.cfg.PlanLabel
	if plan == "" && a.creds != nil {
		plan = a.creds.PlanLabel()
	}
	if plan == "" {
		plan = "Claude"
	}
	msg := a.statusMsg
	if a.rateLimited {
		// Recomputed on every push so the countdown actually counts down.
		if d := time.Until(a.backoffUntil); d > 0 {
			msg = "Límite de consultas · reintento en " + vm.FmtDurShort(d)
		} else {
			msg = "Límite de consultas · reintentando"
		}
	}
	in := vm.Inputs{
		Usage:     a.usage,
		Hist:      a.hist,
		Plan:      plan,
		ThemeMode: a.cfg.Theme,
		LightOS:   winutil.AppsUseLightTheme(),
		Acrylic:   a.ui.AcrylicOK(),
		Autostart: a.autostart,
		Status:    a.status,
		StatusMsg: msg,
	}
	a.mu.Unlock()

	model := vm.Build(in)
	data, err := json.Marshal(model)
	if err != nil {
		log.Printf("vm: %v", err)
		return
	}
	a.ui.Push(string(data), model.Theme == "dark")
}
