//go:build windows

// Claude Usage Widget — a Windows 11 taskbar pill + flyout panel showing the
// live Claude plan usage (5-hour window, weekly limit, per-model breakdown),
// implemented after the Claude Design mock "Claude Usage Widget.dc.html".
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"claude-usage-widget/internal/api"
	"claude-usage-widget/internal/auth"
	"claude-usage-widget/internal/config"
	"claude-usage-widget/internal/history"
	"claude-usage-widget/internal/ui"
	"claude-usage-widget/internal/vm"
	"claude-usage-widget/internal/winutil"
)

const appName = "ClaudeUsageWidget"

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
	lastCLIRefresh time.Time
	backoffUntil   time.Time // set after a 429; scheduled polls wait it out
	autostart      bool      // cached: the logon scheduled task exists
}

func main() {
	runtime.LockOSThread()

	debug := flag.Bool("debug", false, "habilita DevTools y log detallado")
	flag.Parse()

	if !winutil.EnsureSingleInstance(appName) {
		return // another copy is already running
	}
	winutil.SetDPIAware()

	logPath := filepath.Join(config.Dir(), "widget.log")
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
		log.SetOutput(f)
		defer f.Close()
	}
	log.Printf("claude-usage-widget iniciando (debug=%v)", *debug)

	a := &app{
		cfg:     config.Load(),
		scanner: history.NewScanner(),
		poke:    make(chan struct{}, 1),
		status:  "loading",
	}

	u, err := ui.New(a.cfg, ui.Callbacks{
		OnReady: func() {
			a.pushCurrent()
			a.requestPoll()
		},
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
	log.Printf("ventana creada, acrílico=%v", u.AcrylicOK())

	go a.pollLoop()
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

func (a *app) pollLoop() {
	ticker := time.NewTicker(time.Duration(a.cfg.PollSeconds) * time.Second)
	defer ticker.Stop()
	a.gather()
	for {
		select {
		case <-ticker.C:
			a.mu.Lock()
			wait := time.Now().Before(a.backoffUntil)
			a.mu.Unlock()
			if wait {
				continue // rate limited recently; let the endpoint breathe
			}
		case <-a.poke:
			// manual refresh always goes through
		}
		a.gather()
	}
}

// gather fetches credentials, live usage and local history, then pushes the
// refreshed view-model. Runs on the poll goroutine only.
func (a *app) gather() {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

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
	if err == api.ErrAuth {
		log.Printf("token rechazado; intentando refresh vía CLI")
		creds = a.tryCLIRefresh(ctx, creds)
		usage, err = api.FetchUsage(ctx, creds.AccessToken)
	}
	if err != nil {
		log.Printf("uso: %v", err)
		msg := "Sin conexión con el endpoint de uso"
		switch err {
		case api.ErrAuth:
			msg = "Sesión expirada · abre Claude Code"
		case api.ErrRateLimited:
			msg = "Límite de consultas · reintentando"
			a.mu.Lock()
			a.backoffUntil = time.Now().Add(5 * time.Minute)
			a.mu.Unlock()
		}
		a.setStatus(creds, "error", msg)
		return
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
	a.mu.Unlock()
	a.pushCurrent()
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
	in := vm.Inputs{
		Usage:     a.usage,
		Hist:      a.hist,
		Plan:      plan,
		ThemeMode: a.cfg.Theme,
		LightOS:   winutil.AppsUseLightTheme(),
		Acrylic:   a.ui.AcrylicOK(),
		Autostart: a.autostart,
		Status:    a.status,
		StatusMsg: a.statusMsg,
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
