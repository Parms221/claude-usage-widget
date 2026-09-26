//go:build windows

// Package winutil contains the raw Win32 plumbing the widget needs:
// frameless window styling, DWM rounded corners / acrylic backdrop,
// taskbar geometry, wndproc subclassing, autostart and theme detection.
package winutil

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	dwmapi   = windows.NewLazySystemDLL("dwmapi.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")

	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procSetProcessDPIAware            = user32.NewProc("SetProcessDPIAware")
	procGetDpiForWindow               = user32.NewProc("GetDpiForWindow")
	procGetWindowLongPtrW             = user32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtrW             = user32.NewProc("SetWindowLongPtrW")
	procSetWindowPos                  = user32.NewProc("SetWindowPos")
	procCallWindowProcW               = user32.NewProc("CallWindowProcW")
	procSetWindowsHookExW             = user32.NewProc("SetWindowsHookExW")
	procUnhookWindowsHookEx           = user32.NewProc("UnhookWindowsHookEx")
	procCallNextHookEx                = user32.NewProc("CallNextHookEx")
	procDefWindowProcW                = user32.NewProc("DefWindowProcW")
	procSystemParametersInfoW         = user32.NewProc("SystemParametersInfoW")
	procSetForegroundWindow           = user32.NewProc("SetForegroundWindow")
	procShowWindow                    = user32.NewProc("ShowWindow")
	procDwmSetWindowAttribute         = dwmapi.NewProc("DwmSetWindowAttribute")
	procDwmExtendFrameIntoClientArea  = dwmapi.NewProc("DwmExtendFrameIntoClientArea")
	procSHAppBarMessage               = shell32.NewProc("SHAppBarMessage")
	procCreateMutexW                  = kernel32.NewProc("CreateMutexW")
	procGetDC                         = user32.NewProc("GetDC")
	procReleaseDC                     = user32.NewProc("ReleaseDC")
	procGetPixel                      = gdi32.NewProc("GetPixel")
	procGetWindowRect                 = user32.NewProc("GetWindowRect")
	procFindWindowW                   = user32.NewProc("FindWindowW")
	procSetParent                     = user32.NewProc("SetParent")
	procGetAncestor                   = user32.NewProc("GetAncestor")
	procPostMessageW                  = user32.NewProc("PostMessageW")
	procGetWindowThreadProcessId      = user32.NewProc("GetWindowThreadProcessId")
	procAllowSetForegroundWindow      = user32.NewProc("AllowSetForegroundWindow")
	procGetClientRect                 = user32.NewProc("GetClientRect")
	procRegisterWindowMessageW        = user32.NewProc("RegisterWindowMessageW")
	procRegisterClassExW              = user32.NewProc("RegisterClassExW")
	procCreateWindowExW               = user32.NewProc("CreateWindowExW")
	procDestroyWindow                 = user32.NewProc("DestroyWindow")
	procSetLayeredWindowAttributes    = user32.NewProc("SetLayeredWindowAttributes")
	procTrackMouseEvent               = user32.NewProc("TrackMouseEvent")
	procLoadCursorW                   = user32.NewProc("LoadCursorW")
	procSetCursor                     = user32.NewProc("SetCursor")
	procGetModuleHandleW              = kernel32.NewProc("GetModuleHandleW")
)

const (
	gwlStyle    = ^uintptr(15) // -16
	gwlExStyle  = ^uintptr(19) // -20
	gwlpWndProc = ^uintptr(3)  // -4

	wsPopup       = 0x80000000
	wsChild       = 0x40000000
	wsVisible     = 0x10000000
	wsCaption     = 0x00C00000
	wsThickFrame  = 0x00040000
	wsSysMenu     = 0x00080000
	wsMinimizeBox = 0x00020000
	wsMaximizeBox = 0x00010000

	wsExToolWindow = 0x00000080
	wsExTopmost    = 0x00000008
	wsExAppWindow  = 0x00040000

	swpNoSize       = 0x0001
	swpNoMove       = 0x0002
	swpNoZOrder     = 0x0004
	swpNoActivate   = 0x0010
	swpFrameChanged = 0x0020
	swpShowWindow   = 0x0040

	swShowNoActivate = 4

	// WM_* messages surfaced to the subclass callback.
	WMActivate      = 0x0006
	WMSettingChange = 0x001A
	WMDisplayChange = 0x007E
	WMDpiChanged    = 0x02E0

	WAInactive = 0

	whCBT           = 5
	hcbtCreateWnd   = 3
	dwmwaDarkMode   = 20
	dwmwaCorners    = 33
	dwmwaBorder     = 34
	dwmwaBackdrop   = 38
	dwmcpRound      = 2
	dwmColorNone    = 0xFFFFFFFE
	backdropAcrylic = 3 // DWMSBT_TRANSIENTWINDOW

	abmGetTaskbarPos = 5

	spiGetWorkArea = 0x0030
)

// Rect mirrors the Win32 RECT struct.
type Rect struct{ Left, Top, Right, Bottom int32 }

func (r Rect) Width() int32  { return r.Right - r.Left }
func (r Rect) Height() int32 { return r.Bottom - r.Top }

// SetDPIAware opts the process into per-monitor-v2 DPI awareness so window
// coordinates match physical pixels and WebView2 renders crisp.
func SetDPIAware() {
	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 == -4
	r, _, _ := procSetProcessDpiAwarenessContext.Call(^uintptr(3))
	if r == 0 {
		_, _, _ = procSetProcessDPIAware.Call()
	}
}

// DpiFor returns the DPI of the monitor the window lives on (96 = 100%).
func DpiFor(hwnd uintptr) int {
	if err := procGetDpiForWindow.Find(); err == nil {
		if dpi, _, _ := procGetDpiForWindow.Call(hwnd); dpi != 0 {
			return int(dpi)
		}
	}
	return 96
}

// EnsureSingleInstance returns false when another copy already holds the mutex.
func EnsureSingleInstance(name string) bool {
	n, _ := windows.UTF16PtrFromString("Local\\" + name)
	h, _, lastErr := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(n)))
	if h == 0 {
		return true // could not create: fail open
	}
	if errno, ok := lastErr.(syscall.Errno); ok && errno == windows.ERROR_ALREADY_EXISTS {
		return false
	}
	return true
}

// ---- frameless creation hook -------------------------------------------------

var (
	cbtHook     uintptr
	cbtCallback uintptr
	cbtClass    string
)

// HookFramelessCreation installs a thread-local CBT hook that rewrites the next
// window of class `className` created on this thread: it becomes a borderless
// WS_POPUP placed far offscreen, so the library's default framed window never
// flashes on screen. Call the returned func to remove the hook.
func HookFramelessCreation(className string) func() {
	cbtClass = strings.ToLower(className)
	if cbtCallback == 0 {
		cbtCallback = syscall.NewCallback(func(code uintptr, wp uintptr, lp uintptr) uintptr {
			if int32(code) == hcbtCreateWnd {
				cbt := (*cbtCreateWnd)(unsafe.Pointer(lp))
				if cbt != nil && cbt.lpcs != nil && cbt.lpcs.style&wsChild == 0 {
					if matchClassName(cbt.lpcs.lpszClass, cbtClass) {
						cbt.lpcs.style = int32(-1 << 31) // WS_POPUP, frameless
						cbt.lpcs.x = -32000
						cbt.lpcs.y = -32000
					}
				}
			}
			r, _, _ := procCallNextHookEx.Call(cbtHook, code, wp, lp)
			return r
		})
	}
	tid := windows.GetCurrentThreadId()
	h, _, _ := procSetWindowsHookExW.Call(whCBT, cbtCallback, 0, uintptr(tid))
	cbtHook = h
	return func() {
		if cbtHook != 0 {
			_, _, _ = procUnhookWindowsHookEx.Call(cbtHook)
			cbtHook = 0
		}
	}
}

type cbtCreateWnd struct {
	lpcs            *createStructW
	hwndInsertAfter uintptr
}

type createStructW struct {
	lpCreateParams uintptr
	hInstance      uintptr
	hMenu          uintptr
	hwndParent     uintptr
	cy             int32
	cx             int32
	y              int32
	x              int32
	style          int32
	lpszName       *uint16
	lpszClass      *uint16
	dwExStyle      uint32
}

func matchClassName(p *uint16, want string) bool {
	// The class can be an ATOM (numeric) instead of a string pointer.
	if uintptr(unsafe.Pointer(p)) <= 0xFFFF || p == nil {
		return false
	}
	return strings.ToLower(windows.UTF16PtrToString(p)) == want
}

// ---- window styling ------------------------------------------------------------

// MakeWidgetWindow strips any leftover frame styles and marks the window as a
// topmost tool window (no taskbar button, no alt-tab entry).
func MakeWidgetWindow(hwnd uintptr) {
	style, _, _ := procGetWindowLongPtrW.Call(hwnd, gwlStyle)
	style &^= uintptr(wsCaption | wsThickFrame | wsSysMenu | wsMinimizeBox | wsMaximizeBox)
	style |= uintptr(wsPopup)
	_, _, _ = procSetWindowLongPtrW.Call(hwnd, gwlStyle, style)

	ex, _, _ := procGetWindowLongPtrW.Call(hwnd, gwlExStyle)
	ex |= uintptr(wsExToolWindow | wsExTopmost)
	ex &^= uintptr(wsExAppWindow)
	_, _, _ = procSetWindowLongPtrW.Call(hwnd, gwlExStyle, ex)

	_, _, _ = procSetWindowPos.Call(hwnd, 0, 0, 0, 0, 0,
		swpNoMove|swpNoSize|swpNoZOrder|swpNoActivate|swpFrameChanged)
}

// SetRoundCorners toggles the Win11 rounded window corners. Rounded corners
// also bring the standard flyout shadow; DONOTROUND removes both, which is
// what the flat pill wants.
func SetRoundCorners(hwnd uintptr, round bool) {
	pref := int32(1) // DWMWCP_DONOTROUND
	if round {
		pref = dwmcpRound
	}
	_, _, _ = procDwmSetWindowAttribute.Call(hwnd, dwmwaCorners,
		uintptr(unsafe.Pointer(&pref)), unsafe.Sizeof(pref))
}

// SetNoActivate marks the window so clicks never steal focus (like taskbar
// buttons). Mouse input still reaches it.
func SetNoActivate(hwnd uintptr) {
	ex, _, _ := procGetWindowLongPtrW.Call(hwnd, gwlExStyle)
	ex |= 0x08000000 // WS_EX_NOACTIVATE
	_, _, _ = procSetWindowLongPtrW.Call(hwnd, gwlExStyle, ex)
}

// HideDWMBorder removes the thin outline DWM paints around rounded frameless
// windows, so the pill reads as flat content on the taskbar.
func HideDWMBorder(hwnd uintptr) {
	color := uint32(dwmColorNone)
	_, _, _ = procDwmSetWindowAttribute.Call(hwnd, dwmwaBorder,
		uintptr(unsafe.Pointer(&color)), unsafe.Sizeof(color))
}

// SetDarkFrame hints DWM about the theme so backdrop tinting matches.
func SetDarkFrame(hwnd uintptr, dark bool) {
	v := int32(0)
	if dark {
		v = 1
	}
	_, _, _ = procDwmSetWindowAttribute.Call(hwnd, dwmwaDarkMode,
		uintptr(unsafe.Pointer(&v)), unsafe.Sizeof(v))
}

// EnableAcrylic turns on the Win11 "transient window" (acrylic) system
// backdrop and extends the frame so it shows through transparent web content.
// Returns false when the OS rejects it (e.g. Windows 10).
func EnableAcrylic(hwnd uintptr) bool {
	v := int32(backdropAcrylic)
	r, _, _ := procDwmSetWindowAttribute.Call(hwnd, dwmwaBackdrop,
		uintptr(unsafe.Pointer(&v)), unsafe.Sizeof(v))
	if r != 0 { // not S_OK
		return false
	}
	margins := struct{ L, R, T, B int32 }{-1, -1, -1, -1}
	_, _, _ = procDwmExtendFrameIntoClientArea.Call(hwnd, uintptr(unsafe.Pointer(&margins)))
	return true
}

// SetBackdrop switches between the acrylic backdrop (expanded panel) and none
// (collapsed pill, which paints an opaque taskbar-matched background instead).
func SetBackdrop(hwnd uintptr, acrylic bool) {
	v := int32(1) // DWMSBT_NONE
	if acrylic {
		v = backdropAcrylic
	}
	_, _, _ = procDwmSetWindowAttribute.Call(hwnd, dwmwaBackdrop,
		uintptr(unsafe.Pointer(&v)), unsafe.Sizeof(v))
}

// SampleScreenColor reads the physical screen pixel at (x, y). Used to match
// the pill background to the exact taskbar color under any theme/accent.
func SampleScreenColor(x, y int) (r, g, b uint8, ok bool) {
	dc, _, _ := procGetDC.Call(0)
	if dc == 0 {
		return 0, 0, 0, false
	}
	defer procReleaseDC.Call(0, dc)
	c, _, _ := procGetPixel.Call(dc, uintptr(x), uintptr(y))
	if c == 0xFFFFFFFF { // CLR_INVALID
		return 0, 0, 0, false
	}
	return uint8(c), uint8(c >> 8), uint8(c >> 16), true
}

// SetBounds moves/resizes the window keeping it in the topmost band.
func SetBounds(hwnd uintptr, x, y, w, h int, activate bool) {
	flags := uintptr(swpShowWindow)
	if !activate {
		flags |= swpNoActivate
	}
	hwndTopmost := ^uintptr(0) // -1
	_, _, _ = procSetWindowPos.Call(hwnd, hwndTopmost, uintptr(x), uintptr(y), uintptr(w), uintptr(h), flags)
}

// Hide hides the window (SW_HIDE). If it was foreground, Windows hands
// activation to the next window naturally.
func Hide(hwnd uintptr) {
	_, _, _ = procShowWindow.Call(hwnd, 0)
}

// Activate brings the window to the foreground (used when the panel expands so
// that clicking elsewhere deactivates it and we can auto-collapse).
func Activate(hwnd uintptr) {
	_, _, _ = procSetForegroundWindow.Call(hwnd)
}

// ---- geometry ------------------------------------------------------------------

type appBarData struct {
	cbSize           uint32
	hWnd             uintptr
	uCallbackMessage uint32
	uEdge            uint32
	rc               Rect
	lParam           int64
}

// TaskbarRect returns the primary taskbar rectangle in physical pixels.
func TaskbarRect() (Rect, bool) {
	var abd appBarData
	abd.cbSize = uint32(unsafe.Sizeof(abd))
	r, _, _ := procSHAppBarMessage.Call(abmGetTaskbarPos, uintptr(unsafe.Pointer(&abd)))
	if r == 0 {
		return Rect{}, false
	}
	return abd.rc, true
}

// WorkArea returns the primary monitor work area (desktop minus taskbar).
func WorkArea() Rect {
	var rc Rect
	_, _, _ = procSystemParametersInfoW.Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&rc)), 0)
	return rc
}

// ---- subclassing -----------------------------------------------------------------

// MsgHandler receives window messages before the library wndproc. Return
// handled=true to swallow the message.
type MsgHandler func(msg uint32, wparam, lparam uintptr) (handled bool, result uintptr)

type subEntry struct {
	prev    uintptr
	handler MsgHandler
}

var (
	subclassMap = map[uintptr]*subEntry{} // touched only on the UI thread
	subclassCb  uintptr
)

// Subclass installs handler in front of the window's original wndproc.
// Call from the UI thread only (messages arrive there too).
func Subclass(hwnd uintptr, handler MsgHandler) {
	if subclassCb == 0 {
		subclassCb = syscall.NewCallback(func(h uintptr, msg uintptr, wp uintptr, lp uintptr) uintptr {
			if e, ok := subclassMap[h]; ok {
				if handled, res := e.handler(uint32(msg), wp, lp); handled {
					return res
				}
				r, _, _ := procCallWindowProcW.Call(e.prev, h, msg, wp, lp)
				return r
			}
			r, _, _ := procDefWindowProcW.Call(h, msg, wp, lp)
			return r
		})
	}
	prev, _, _ := procSetWindowLongPtrW.Call(hwnd, gwlpWndProc, subclassCb)
	subclassMap[hwnd] = &subEntry{prev: prev, handler: handler}
}

// Unsubclass forgets a window's handler. Call it on WM_NCDESTROY so a future
// window that reuses the HWND value doesn't hit a stale handler.
func Unsubclass(hwnd uintptr) {
	delete(subclassMap, hwnd)
}

// ---- taskbar integration -------------------------------------------------------------
//
// The pill lives INSIDE the taskbar as a child of Shell_TrayWnd (the approach
// tools like TrafficMonitor use). That removes the whole topmost arms race:
// the pill cannot be covered by the taskbar it belongs to, and when the shell
// hides the taskbar for full-screen apps the pill disappears with it.

// FindTaskbar returns the Shell_TrayWnd handle, or 0.
func FindTaskbar() uintptr {
	cls, _ := windows.UTF16PtrFromString("Shell_TrayWnd")
	h, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(cls)), 0)
	return h
}

func registerMessage(name string) uint32 {
	s, _ := windows.UTF16PtrFromString(name)
	m, _, _ := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(s)))
	return uint32(m)
}

// TaskbarCreatedMessage returns the broadcast message id Explorer sends to
// top-level windows after it (re)creates the taskbar.
func TaskbarCreatedMessage() uint32 { return registerMessage("TaskbarCreated") }

// ActivateMessage is what a second launch of the exe posts to the running
// instance: the user starting it again usually means "I can't see it".
func ActivateMessage() uint32 { return registerMessage("ClaudeUsageWidget.Activate") }

// NotifyRunningInstance posts msg to the top-level window with the given
// class and title, and lets its process take the foreground (only the
// process the user just launched holds that right). Reports whether the
// window was found.
func NotifyRunningInstance(class, title string, msg uint32) bool {
	c, _ := windows.UTF16PtrFromString(class)
	t, _ := windows.UTF16PtrFromString(title)
	h, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(c)), uintptr(unsafe.Pointer(t)))
	if h == 0 {
		return false
	}
	var pid uint32
	_, _, _ = procGetWindowThreadProcessId.Call(h, uintptr(unsafe.Pointer(&pid)))
	_, _, _ = procAllowSetForegroundWindow.Call(uintptr(pid))
	r, _, _ := procPostMessageW.Call(h, uintptr(msg), 0, 0)
	return r != 0
}

// AttachToTaskbar turns the window into a WS_CHILD of the taskbar and reports
// whether it really got there. SetParent into Explorer's window can fail (or
// be undone) while the shell is still starting after logon, and a window left
// with WS_CHILD under the desktop is an orphan: drawn at the screen's top-left
// corner, behind every other window. On failure the window goes back to being
// a plain top-level popup so the caller can float it and retry later.
func AttachToTaskbar(hwnd, taskbar uintptr) error {
	style, _, _ := procGetWindowLongPtrW.Call(hwnd, gwlStyle)
	style &^= uintptr(wsPopup | wsCaption | wsThickFrame | wsSysMenu)
	style |= uintptr(wsChild | wsVisible)
	_, _, _ = procSetWindowLongPtrW.Call(hwnd, gwlStyle, style)

	ex, _, _ := procGetWindowLongPtrW.Call(hwnd, gwlExStyle)
	ex &^= uintptr(wsExTopmost | wsExAppWindow)
	_, _, _ = procSetWindowLongPtrW.Call(hwnd, gwlExStyle, ex)

	r, _, err := procSetParent.Call(hwnd, taskbar)
	if ParentOf(hwnd) != taskbar {
		detach(hwnd)
		if r == 0 {
			return errors.New("SetParent: " + err.Error())
		}
		return errors.New("SetParent no tuvo efecto")
	}
	_, _, _ = procSetWindowPos.Call(hwnd, 0 /* HWND_TOP */, 0, 0, 0, 0,
		swpNoMove|swpNoSize|swpNoActivate|swpFrameChanged)
	return nil
}

// detach turns hwnd back into a top-level popup. SetParent keeps WS_CHILD
// when the new parent is the desktop, so the style is fixed by hand.
func detach(hwnd uintptr) {
	_, _, _ = procSetParent.Call(hwnd, 0)
	style, _, _ := procGetWindowLongPtrW.Call(hwnd, gwlStyle)
	style = style&^uintptr(wsChild) | uintptr(wsPopup)
	_, _, _ = procSetWindowLongPtrW.Call(hwnd, gwlStyle, style)
	_, _, _ = procSetWindowPos.Call(hwnd, 0, 0, 0, 0, 0,
		swpNoMove|swpNoSize|swpNoZOrder|swpNoActivate|swpFrameChanged)
}

// ParentOf returns hwnd's real parent (GA_PARENT): the desktop window for a
// top-level window, never its owner.
func ParentOf(hwnd uintptr) uintptr {
	p, _, _ := procGetAncestor.Call(hwnd, 1 /* GA_PARENT */)
	return p
}

// SetChildBounds positions a child window (client coordinates of its parent)
// and keeps it above its siblings.
func SetChildBounds(hwnd uintptr, x, y, w, h int) {
	_, _, _ = procSetWindowPos.Call(hwnd, 0 /* HWND_TOP */, uintptr(x), uintptr(y),
		uintptr(w), uintptr(h), swpNoActivate|swpShowWindow)
}

// ClientRect returns the client-area rectangle of hwnd (origin 0,0).
func ClientRect(hwnd uintptr) Rect {
	var rc Rect
	_, _, _ = procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	return rc
}

// WindowRect returns the screen rectangle of hwnd.
func WindowRect(hwnd uintptr) Rect {
	var rc Rect
	_, _, _ = procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	return rc
}

// ---- input overlay -------------------------------------------------------------------
//
// WebView2 receives pointer input through the composition pipeline anchored
// at its top-level window; parented into the Win11 XAML taskbar that pipeline
// belongs to Explorer, so the embedded pill renders but never sees clicks.
// Classic Win32 children DO get WM_* mouse input there, so a tiny invisible
// (alpha=1) layered sibling sits exactly over the pill and relays the mouse.

// Overlay mouse events.
const (
	OverlayDown = iota
	OverlayHoverOn
	OverlayHoverOff
)

var (
	overlayClassRegistered bool
	overlayCb              uintptr
	overlayHandlers        = map[uintptr]func(int){} // UI thread only
	overlayHovering        = map[uintptr]bool{}
)

// CreateInputOverlay creates the invisible input relay and parents it into
// the taskbar. Must be called on the UI thread; onEvent fires there too.
// The window is created parentless first: CreateWindowEx refuses WS_CHILD
// with a foreign-process parent, but SetParent afterwards is allowed.
func CreateInputOverlay(parent uintptr, onEvent func(int)) (uintptr, error) {
	const (
		wmMouseMove   = 0x0200
		wmLButtonDown = 0x0201
		wmMouseLeave  = 0x02A3
		wmSetCursor   = 0x0020
		wmNCDestroy   = 0x0082
		tmeLeave      = 0x0002
		lwaAlpha      = 0x0002
		wsExLayered   = 0x00080000
		idcHand       = 32649
	)
	if overlayCb == 0 {
		overlayCb = syscall.NewCallback(func(h uintptr, msg uintptr, wp uintptr, lp uintptr) uintptr {
			handler := overlayHandlers[h]
			switch uint32(msg) {
			case wmLButtonDown:
				if handler != nil {
					handler(OverlayDown)
				}
				return 0
			case wmMouseMove:
				if handler != nil && !overlayHovering[h] {
					overlayHovering[h] = true
					var tme struct {
						CbSize      uint32
						DwFlags     uint32
						HwndTrack   uintptr
						DwHoverTime uint32
					}
					tme.CbSize = uint32(unsafe.Sizeof(tme))
					tme.DwFlags = tmeLeave
					tme.HwndTrack = h
					_, _, _ = procTrackMouseEvent.Call(uintptr(unsafe.Pointer(&tme)))
					handler(OverlayHoverOn)
				}
				return 0
			case wmMouseLeave:
				overlayHovering[h] = false
				if handler != nil {
					handler(OverlayHoverOff)
				}
				return 0
			case wmSetCursor:
				cur, _, _ := procLoadCursorW.Call(0, idcHand)
				_, _, _ = procSetCursor.Call(cur)
				return 1
			case wmNCDestroy:
				delete(overlayHandlers, h)
				delete(overlayHovering, h)
			}
			r, _, _ := procDefWindowProcW.Call(h, msg, wp, lp)
			return r
		})
	}
	hinst, _, _ := procGetModuleHandleW.Call(0)
	clsName, _ := windows.UTF16PtrFromString("ClaudeUsageInput")
	if !overlayClassRegistered {
		var wc struct {
			CbSize        uint32
			Style         uint32
			LpfnWndProc   uintptr
			CbClsExtra    int32
			CbWndExtra    int32
			HInstance     uintptr
			HIcon         uintptr
			HCursor       uintptr
			HbrBackground uintptr
			LpszMenuName  *uint16
			LpszClassName *uint16
			HIconSm       uintptr
		}
		wc.CbSize = uint32(unsafe.Sizeof(wc))
		wc.LpfnWndProc = overlayCb
		wc.HInstance = hinst
		wc.LpszClassName = clsName
		if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
			return 0, errors.New("RegisterClassEx: " + err.Error())
		}
		overlayClassRegistered = true
	}
	h, _, err := procCreateWindowExW.Call(
		wsExLayered,
		uintptr(unsafe.Pointer(clsName)), 0,
		uintptr(uint32(wsPopup)),
		0, 0, 10, 10,
		0, 0, hinst, 0)
	if h == 0 {
		return 0, errors.New("CreateWindowEx: " + err.Error())
	}
	if err := AttachToTaskbar(h, parent); err != nil {
		// An orphaned overlay would sit invisible at the screen's corner,
		// still eating clicks there.
		_, _, _ = procDestroyWindow.Call(h)
		return 0, err
	}
	// Alpha 1/255: imperceptible but still hit-testable (alpha 0 would be
	// click-through).
	_, _, _ = procSetLayeredWindowAttributes.Call(h, 0, 1, lwaAlpha)
	overlayHandlers[h] = onEvent
	return h, nil
}

// DestroyOverlay tears the relay down (no-op for 0).
func DestroyOverlay(hwnd uintptr) {
	if hwnd != 0 {
		_, _, _ = procDestroyWindow.Call(hwnd)
	}
}

// ---- shell / registry helpers ------------------------------------------------------

// OpenURL opens the URL in the default browser.
func OpenURL(url string) {
	u, _ := windows.UTF16PtrFromString(url)
	verb, _ := windows.UTF16PtrFromString("open")
	_ = windows.ShellExecute(0, verb, u, nil, nil, windows.SW_SHOWNORMAL)
}

// AppsUseLightTheme reads the Windows personalization setting.
func AppsUseLightTheme() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("AppsUseLightTheme")
	if err != nil {
		return false
	}
	return v == 1
}

const autostartKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// SetAutostart registers/unregisters the current executable in HKCU\...\Run.
func SetAutostart(name string, enable bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartKey, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !enable {
		err := k.DeleteValue(name)
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return k.SetStringValue(name, `"`+exe+`"`)
}

// IsAutostartEnabled reports whether the Run entry exists.
func IsAutostartEnabled(name string) bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(name)
	return err == nil
}

// ---- logon scheduled task ------------------------------------------------------
//
// The HKCU Run key is unreliable for freshly built unsigned executables
// (SmartScreen/Defender can silently skip them at logon). A per-user
// scheduled task with a short delay is the robust way to autostart, and the
// delay also dodges the "taskbar not created yet" race.

func hiddenCmd(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
	return cmd
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}

// CreateLogonTask registers (or replaces) a scheduled task that starts exe
// 10 seconds after the current user logs on.
func CreateLogonTask(name, exe, tmpDir string) error {
	user := os.Getenv("USERDOMAIN") + `\` + os.Getenv("USERNAME")
	xml := `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Inicia el widget de uso de Claude al iniciar sesión.</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>` + xmlEscape(user) + `</UserId>
      <Delay>PT10S</Delay>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>` + xmlEscape(user) + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <StartWhenAvailable>true</StartWhenAvailable>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Enabled>true</Enabled>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + xmlEscape(exe) + `</Command>
    </Exec>
  </Actions>
</Task>`

	path := filepath.Join(tmpDir, "logon-task.xml")
	if err := writeUTF16File(path, xml); err != nil {
		return err
	}
	defer os.Remove(path)
	out, err := hiddenCmd("schtasks", "/Create", "/TN", name, "/XML", path, "/F").CombinedOutput()
	if err != nil {
		return errors.New("schtasks: " + strings.TrimSpace(string(out)))
	}
	return nil
}

// DeleteLogonTask removes the task; a missing task is not an error.
func DeleteLogonTask(name string) error {
	out, err := hiddenCmd("schtasks", "/Delete", "/TN", name, "/F").CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.ToLower(string(out))
	if strings.Contains(msg, "cannot find") || strings.Contains(msg, "no encuentra") ||
		strings.Contains(msg, "no existe") {
		return nil
	}
	return errors.New("schtasks: " + strings.TrimSpace(string(out)))
}

// LogonTaskExists reports whether the scheduled task is registered.
func LogonTaskExists(name string) bool {
	return hiddenCmd("schtasks", "/Query", "/TN", name).Run() == nil
}

func writeUTF16File(path, s string) error {
	u := utf16.Encode([]rune(s))
	buf := make([]byte, 0, 2+len(u)*2)
	buf = append(buf, 0xFF, 0xFE) // UTF-16LE BOM
	for _, c := range u {
		buf = append(buf, byte(c), byte(c>>8))
	}
	return os.WriteFile(path, buf, 0o644)
}

// MsgBox shows a native message box (the app has no console to print to).
func MsgBox(title, text string) {
	t, _ := windows.UTF16PtrFromString(title)
	m, _ := windows.UTF16PtrFromString(text)
	_, _ = windows.MessageBox(0, m, t, windows.MB_OK|windows.MB_ICONWARNING)
}
