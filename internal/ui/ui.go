//go:build windows

// Package ui owns the two WebView2 windows that make up the widget.
//
// The pill is a WS_CHILD of the taskbar (Shell_TrayWnd): it cannot be covered
// by the bar it lives in, it hides together with it when a full-screen app
// runs, and no topmost re-assertion is ever needed. If Explorer restarts, the
// taskbar (and with it our pill) is destroyed; the panel — a normal top-level
// window that also runs the message loop — receives the TaskbarCreated
// broadcast and recreates the pill. SetParent into the taskbar can also fail
// or be undone while the shell is still starting after logon, so the
// attachment is re-verified periodically; until it holds, the pill floats.
package ui

import (
	_ "embed"
	"fmt"
	"log"
	"math"
	"path/filepath"
	"reflect"
	"time"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	"github.com/jchv/go-webview2/pkg/edge"

	"github.com/Parms221/claude-usage-widget/internal/config"
	"github.com/Parms221/claude-usage-widget/internal/winutil"
)

//go:embed assets/widget.html
var widgetHTML string

// Callbacks lets main react to UI events. All of them are invoked on the UI
// thread; long work must be pushed to another goroutine by the receiver.
type Callbacks struct {
	OnReady        func()
	OnRefresh      func()
	OnQuit         func()
	OnSetTheme     func(mode string)
	OnSetAutostart func(enable bool)
	// OnEnvChanged fires for OS theme / display / DPI changes so the app can
	// rebuild and re-push the view-model.
	OnEnvChanged func()
}

type view struct {
	w    webview2.WebView
	hwnd uintptr
}

// UI wraps both windows.
type UI struct {
	pill  view
	panel view // stable anchor: runs the loop, receives broadcasts
	cfg   *config.Config
	cb    Callbacks

	attached    bool    // pill is parented into the taskbar
	attachNote  string  // last logged attach outcome (retries log changes only)
	pillDead    bool    // pill window destroyed (Explorer went away)
	overlay     uintptr // classic input relay over the pill (Win11 taskbar)
	recreating  bool
	taskbarMsg  uint32
	activateMsg uint32 // posted by a second launch of the exe
	debug       bool

	open        bool
	pendingOpen bool
	closedAt    time.Time // when the panel last auto-closed (race guard)
	panelW      int       // last measured panel size, physical px
	panelH      int
	acrylicOK   bool

	lastVM     string // last pushed view-model (to refill a recreated pill)
	lastVMDark bool
}

// New creates both windows; the pill is attached into the taskbar.
func New(cfg *config.Config, cb Callbacks, debug bool) (*UI, error) {
	u := &UI{
		cfg:         cfg,
		cb:          cb,
		taskbarMsg:  winutil.TaskbarCreatedMessage(),
		activateMsg: winutil.ActivateMessage(),
		debug:       debug,
	}

	var err error
	if u.panel, err = u.mkView("panel", true, 420, 560, debug); err != nil {
		return nil, err
	}
	winutil.MakeWidgetWindow(u.panel.hwnd)
	winutil.HideDWMBorder(u.panel.hwnd)
	winutil.SetRoundCorners(u.panel.hwnd, true)
	if cfg.Acrylic {
		if winutil.EnableAcrylic(u.panel.hwnd) && transparentBackground(u.panel.w) {
			u.acrylicOK = true
		}
	}
	if !u.acrylicOK {
		winutil.SetBackdrop(u.panel.hwnd, false)
	}
	u.bindPanel()
	winutil.Subclass(u.panel.hwnd, u.panelMsg)
	u.panel.w.SetHtml(widgetHTML)
	winutil.Hide(u.panel.hwnd) // hidden until the pill opens it

	if err := u.createPill(debug); err != nil {
		return nil, err
	}
	return u, nil
}

// createPill builds (or rebuilds) the pill window and attaches it to the
// taskbar. Must run on the UI thread.
func (u *UI) createPill(debug bool) error {
	v, err := u.mkView("pill", false, 180, 40, debug)
	if err != nil {
		return err
	}
	u.pill = v
	u.pillDead = false
	u.attached = false
	u.attachNote = ""

	winutil.MakeWidgetWindow(v.hwnd)
	winutil.HideDWMBorder(v.hwnd)
	winutil.SetRoundCorners(v.hwnd, false) // flat: no DWM rounding, no shadow
	winutil.SetNoActivate(v.hwnd)
	winutil.SetBackdrop(v.hwnd, false)
	transparentBackground(v.w) // avoids any white pre-paint flash

	u.bindPill()
	winutil.Subclass(v.hwnd, u.pillMsg)
	v.w.SetHtml(widgetHTML)

	u.tryAttach()
	return nil
}

func (u *UI) mkView(mode string, autofocus bool, width, height uint, debug bool) (view, error) {
	unhook := winutil.HookFramelessCreation("webview")
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug: debug,
		// The panel needs the webview child focused on activation so the
		// keyboard (Esc, menu) reaches the page; the pill must never grab
		// focus at all.
		AutoFocus: autofocus,
		DataPath:  filepath.Join(config.Dir(), "webview-data"),
		WindowOptions: webview2.WindowOptions{
			Title:  windowTitle(mode),
			Width:  width,
			Height: height,
		},
	})
	unhook()
	if w == nil {
		return view{}, fmt.Errorf("no se pudo iniciar WebView2 (¿runtime instalado?)")
	}
	w.Init("window.__MODE='" + mode + "'")
	return view{w: w, hwnd: uintptr(w.Window())}, nil
}

func windowTitle(mode string) string { return "Claude Usage " + mode }

// NotifyRunning asks an already running widget to re-check its pill and open
// the panel. Called by a second launch, which would otherwise exit silently
// and look like "the app doesn't start".
func NotifyRunning() bool {
	return winutil.NotifyRunningInstance("webview", windowTitle("panel"), winutil.ActivateMessage())
}

// tryAttach parents the pill and its input overlay into the taskbar. When
// that fails the pill stays a floating popup and ensureAttached retries.
func (u *UI) tryAttach() {
	if !u.cfg.OverlayTaskbar {
		return
	}
	tb := winutil.FindTaskbar()
	if tb == 0 {
		u.noteAttach("taskbar no encontrado; pastilla en modo flotante")
		return
	}
	if err := winutil.AttachToTaskbar(u.pill.hwnd, tb); err != nil {
		u.noteAttach(fmt.Sprintf("no se pudo integrar la pastilla al taskbar (%v); modo flotante, se reintentará", err))
		return
	}
	u.attached = true
	// The Win11 XAML taskbar swallows composition-pipeline input, so the
	// embedded webview renders but never sees the mouse; the classic overlay
	// relays clicks and hover to the page.
	winutil.DestroyOverlay(u.overlay)
	var err error
	if u.overlay, err = winutil.CreateInputOverlay(tb, u.onPillMouse); err != nil {
		u.noteAttach(fmt.Sprintf("pastilla integrada al taskbar, pero el overlay de input falló: %v", err))
		return
	}
	u.noteAttach("pastilla integrada al taskbar (overlay=true)")
}

// noteAttach logs attach outcomes only when they change: ensureAttached keeps
// retrying while the shell is unavailable and must not flood the log.
func (u *UI) noteAttach(msg string) {
	if msg != u.attachNote {
		u.attachNote = msg
		log.Printf("ui: %s", msg)
	}
}

// ensureAttached verifies that the pill and its input overlay really live
// inside the taskbar, and re-parents them when they don't. It reports whether
// the pill must be placed again: a fresh overlay sits at 0,0 until placed,
// and a pill that fell back to floating needs its popup position.
func (u *UI) ensureAttached() (replace bool) {
	if u.pillDead || u.recreating || !u.cfg.OverlayTaskbar {
		return false
	}
	tb := winutil.FindTaskbar()
	inside := u.attached && tb != 0 && winutil.ParentOf(u.pill.hwnd) == tb
	if inside && u.overlay != 0 && winutil.ParentOf(u.overlay) == tb {
		return false
	}
	wasAttached := u.attached
	if wasAttached && !inside {
		log.Printf("ui: la pastilla quedó fuera del taskbar (padre %#x, taskbar %#x); reintegrando",
			winutil.ParentOf(u.pill.hwnd), tb)
		u.attachNote = "" // log the outcome of the repair
	}
	u.attached = false
	u.tryAttach()
	// Still floating after another failed retry: already placed, nothing to do.
	return u.attached || wasAttached
}

// EnsureAttached runs ensureAttached from any goroutine and re-places the
// pill when needed. Cheap (a FindWindow and two GetAncestor calls).
func (u *UI) EnsureAttached() {
	u.panel.w.Dispatch(func() {
		if u.ensureAttached() {
			u.remeasure()
		}
	})
}

func (u *UI) onPillMouse(evt int) {
	if u.pillDead {
		return
	}
	switch evt {
	case winutil.OverlayDown:
		u.pill.w.Eval("window.__pillPress && window.__pillPress()")
		u.toggle()
	case winutil.OverlayHoverOn:
		u.pill.w.Eval("window.__pillHover && window.__pillHover(true)")
	case winutil.OverlayHoverOff:
		u.pill.w.Eval("window.__pillHover && window.__pillHover(false)")
	}
}

// AcrylicOK reports whether the native blur backdrop is active on the panel.
func (u *UI) AcrylicOK() bool { return u.acrylicOK }

// Run pumps the shared message loop until Quit.
func (u *UI) Run() { u.panel.w.Run() }

// Quit can be called from any goroutine.
func (u *UI) Quit() {
	u.panel.w.Dispatch(func() { u.panel.w.Terminate() })
}

// Push sends a marshalled view-model to both pages. Safe from any goroutine.
func (u *UI) Push(vmJSON string, dark bool) {
	u.panel.w.Dispatch(func() {
		u.lastVM, u.lastVMDark = vmJSON, dark
		winutil.SetDarkFrame(u.panel.hwnd, dark)
		js := "window.__update && window.__update(" + vmJSON + ")"
		u.panel.w.Eval(js)
		if !u.pillDead {
			u.pill.w.Eval(js)
		}
	})
}

// Remeasure re-anchors both windows from any goroutine (used right after
// logon while the taskbar geometry is still settling).
func (u *UI) Remeasure() {
	u.panel.w.Dispatch(func() { u.remeasure() })
}

// ---- bindings ---------------------------------------------------------------

func must(err error) {
	if err != nil {
		log.Printf("bind: %v", err)
	}
}

func (u *UI) bindPill() {
	w := u.pill.w
	must(w.Bind("hostReady", func() {
		if u.cb.OnReady != nil {
			u.cb.OnReady()
		}
	}))
	must(w.Bind("hostToggle", func() { u.toggle() }))
	must(w.Bind("hostLog", func(msg string) { log.Printf("js: %s", msg) }))
	must(w.Bind("hostSetBounds", func(wCSS, hCSS float64, _ bool) {
		u.placePill(wCSS, hCSS)
	}))
}

func (u *UI) bindPanel() {
	w := u.panel.w
	must(w.Bind("hostReady", func() {
		if u.cb.OnReady != nil {
			u.cb.OnReady()
		}
	}))
	must(w.Bind("hostToggle", func() { u.toggle() }))
	must(w.Bind("hostLog", func(msg string) { log.Printf("js: %s", msg) }))
	must(w.Bind("hostSetBounds", func(wCSS, hCSS float64, _ bool) {
		u.sizePanel(wCSS, hCSS)
	}))
	must(w.Bind("hostPanelHidden", func() { u.parkPanel() }))
	must(w.Bind("hostPanelEsc", func() { u.closePanel() }))
	must(w.Bind("hostRefresh", func() {
		if u.cb.OnRefresh != nil {
			u.cb.OnRefresh()
		}
	}))
	must(w.Bind("hostQuit", func() {
		if u.cb.OnQuit != nil {
			u.cb.OnQuit()
		}
	}))
	must(w.Bind("hostSetTheme", func(mode string) {
		if u.cb.OnSetTheme != nil {
			u.cb.OnSetTheme(mode)
		}
	}))
	must(w.Bind("hostSetAutostart", func(enable bool) {
		if u.cb.OnSetAutostart != nil {
			u.cb.OnSetAutostart(enable)
		}
	}))
	must(w.Bind("hostOpenDetails", func() {
		winutil.OpenURL("https://claude.ai/settings/usage")
	}))
}

// ---- open / close -----------------------------------------------------------

func (u *UI) toggle() {
	log.Printf("ui: toggle (open=%v)", u.open)
	if u.open {
		u.closePanel()
		return
	}
	// If the same click already closed the panel via focus loss, the user's
	// intent was "close" — don't bounce it back open.
	if time.Since(u.closedAt) < 300*time.Millisecond {
		return
	}
	u.openPanel()
}

func (u *UI) openPanel() {
	u.open = true
	if u.panelW == 0 || u.panelH == 0 {
		u.pendingOpen = true // first measurement not in yet; sizePanel will place it
		return
	}
	u.placePanel()
	u.panel.w.Eval("window.__panelShow && window.__panelShow()")
	winutil.Activate(u.panel.hwnd)
}

// closePanel plays the fly-out animation; the page calls hostPanelHidden when
// it finishes and the window hides.
func (u *UI) closePanel() {
	if !u.open {
		return
	}
	u.open = false
	u.pendingOpen = false
	u.closedAt = time.Now()
	u.panel.w.Eval("window.__panelHide && window.__panelHide()")
}

func (u *UI) parkPanel() {
	if u.open {
		return // reopened while the closing animation ran
	}
	winutil.Hide(u.panel.hwnd)
}

// ---- geometry ---------------------------------------------------------------

// placePill positions the pill inside the taskbar (client coordinates) or, in
// floating fallback mode, just above the work area.
func (u *UI) placePill(wCSS, hCSS float64) {
	if u.pillDead {
		return
	}
	dpi := winutil.DpiFor(u.pill.hwnd)
	scale := float64(dpi) / 96.0
	w := int(math.Ceil(wCSS * scale))
	h := int(math.Ceil(hCSS * scale))
	margin := int(math.Round(float64(u.cfg.MarginX) * scale))

	u.ensureAttached() // never trust a stale "attached": an orphan is invisible
	if u.attached {
		tb := winutil.FindTaskbar()
		if tb == 0 {
			u.attached = false
			return
		}
		client := winutil.ClientRect(tb)
		x := margin
		y := (int(client.Height()) - h) / 2
		winutil.SetChildBounds(u.pill.hwnd, x, y, w, h)
		if u.overlay != 0 {
			winutil.SetChildBounds(u.overlay, x, y, w, h) // placed after → above
		}

		screen := winutil.WindowRect(tb)
		u.matchTaskbarColor(int(screen.Left)+x+w, int(screen.Top)+y+h/2)
		return
	}

	// Floating fallback: bottom-left just above the work area.
	wa := winutil.WorkArea()
	gap := int(math.Round(12 * scale))
	x := int(wa.Left) + margin
	y := int(wa.Bottom) - gap/2 - h
	winutil.SetBounds(u.pill.hwnd, x, y, w, h, false)
	pr := winutil.WindowRect(u.pill.hwnd)
	u.matchTaskbarColor(int(pr.Right), int(pr.Top)+h/2)
}

// sizePanel records the measured panel size and (re)positions it if visible.
func (u *UI) sizePanel(wCSS, hCSS float64) {
	dpi := winutil.DpiFor(u.panel.hwnd)
	scale := float64(dpi) / 96.0
	u.panelW = int(math.Ceil(wCSS * scale))
	u.panelH = int(math.Ceil(hCSS * scale))
	if u.open {
		first := u.pendingOpen
		u.pendingOpen = false
		u.placePanel()
		if first {
			u.panel.w.Eval("window.__panelShow && window.__panelShow()")
			winutil.Activate(u.panel.hwnd)
		}
	}
}

func (u *UI) placePanel() {
	dpi := winutil.DpiFor(u.panel.hwnd)
	scale := float64(dpi) / 96.0
	margin := int(math.Round(float64(u.cfg.MarginX) * scale))
	gap := int(math.Round(12 * scale))

	wa := winutil.WorkArea()
	tb, hasTb := winutil.TaskbarRect()
	bottom := int(wa.Bottom) - gap
	if hasTb && tb.Top >= wa.Bottom-2 {
		bottom = int(tb.Top) - gap
	}
	y := bottom - u.panelH
	if y < int(wa.Top) {
		y = int(wa.Top)
	}
	winutil.SetBounds(u.panel.hwnd, int(wa.Left)+margin, y, u.panelW, u.panelH, true)
}

// matchTaskbarColor samples the pixels just right of the pill and hands the
// color to the page, which uses it as the flat background.
func (u *UI) matchTaskbarColor(rightX, midY int) {
	var rs, gs, bs, n int
	for _, dx := range []int{16, 30, 44} {
		if r, g, b, ok := winutil.SampleScreenColor(rightX+dx, midY); ok {
			rs += int(r)
			gs += int(g)
			bs += int(b)
			n++
		}
	}
	if n == 0 {
		return
	}
	u.pill.w.Eval(fmt.Sprintf(
		"window.__setTaskbarColor && window.__setTaskbarColor('rgb(%d,%d,%d)')",
		rs/n, gs/n, bs/n))
}

// ---- window messages ----------------------------------------------------------

const (
	wmDestroy   = 0x0002
	wmNCDestroy = 0x0082
)

func (u *UI) pillMsg(msg uint32, wparam, lparam uintptr) (bool, uintptr) {
	switch msg {
	case wmDestroy:
		// Explorer (our window's ancestor) is going away and takes the pill
		// with it. Swallow the message so the library doesn't quit the app;
		// the TaskbarCreated broadcast will trigger a rebuild.
		u.pillDead = true
		u.attached = false
		u.overlay = 0 // died with its taskbar parent too
		log.Printf("ui: pastilla destruida (explorer se reinicia)")
		return true, 0
	case wmNCDestroy:
		winutil.Unsubclass(u.pill.hwnd)
	}
	return false, 0
}

func (u *UI) panelMsg(msg uint32, wparam, lparam uintptr) (bool, uintptr) {
	switch msg {
	case winutil.WMActivate:
		if wparam&0xFFFF == winutil.WAInactive && u.open {
			// Clicking anywhere else closes the flyout, like native ones.
			u.closePanel()
		}
	case winutil.WMSettingChange, winutil.WMDisplayChange:
		if u.cb.OnEnvChanged != nil {
			u.cb.OnEnvChanged()
		}
		u.remeasure()
	case winutil.WMDpiChanged:
		u.remeasure()
	case u.taskbarMsg:
		if u.taskbarMsg != 0 {
			log.Printf("ui: TaskbarCreated recibido")
			u.panel.w.Dispatch(u.rebuildPill)
		}
	case u.activateMsg:
		if u.activateMsg != 0 {
			log.Printf("ui: otra ejecución pidió mostrar el widget")
			u.panel.w.Dispatch(u.showFromRelaunch)
		}
	}
	return false, 0
}

// showFromRelaunch answers a second launch of the exe: make sure the pill is
// back in the taskbar and open the panel as visible proof the widget runs.
func (u *UI) showFromRelaunch() {
	u.ensureAttached()
	u.remeasure()
	if !u.open {
		u.openPanel()
	}
}

// rebuildPill recreates the pill after Explorer restarted (or just re-attaches
// it when the window survived).
func (u *UI) rebuildPill() {
	if u.recreating {
		return
	}
	u.recreating = true
	defer func() { u.recreating = false }()

	if u.pillDead {
		if err := u.createPill(u.debug); err != nil {
			log.Printf("ui: recreando pastilla: %v", err)
			return
		}
		// The fresh page reports hostReady → main re-pushes; also replay the
		// last view-model right away to avoid a blank pill.
		if u.lastVM != "" {
			u.pill.w.Eval("window.__update && window.__update(" + u.lastVM + ")")
		}
		log.Printf("ui: pastilla recreada")
		return
	}
	u.attached = false
	u.attachNote = ""
	u.tryAttach()
	u.remeasure()
}

func (u *UI) remeasure() {
	js := "window.__remeasure && window.__remeasure()"
	if !u.pillDead {
		u.pill.w.Eval(js)
	}
	u.panel.w.Eval(js)
}

// transparentBackground reaches into the library to set the WebView2 default
// background to fully transparent so the DWM backdrop shows through. Uses
// reflection on an unexported field; any failure just means solid mode.
func transparentBackground(w webview2.WebView) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	rv := reflect.ValueOf(w)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return false
	}
	f := rv.Elem().FieldByName("browser")
	if !f.IsValid() || !f.CanAddr() {
		return false
	}
	f = reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem()
	chr, good := f.Interface().(*edge.Chromium)
	if !good || chr == nil {
		return false
	}
	ctl := chr.GetController()
	if ctl == nil {
		return false
	}
	c2 := ctl.GetICoreWebView2Controller2()
	if c2 == nil {
		return false
	}
	return c2.PutDefaultBackgroundColor(edge.COREWEBVIEW2_COLOR{A: 0, R: 0, G: 0, B: 0}) == nil
}
