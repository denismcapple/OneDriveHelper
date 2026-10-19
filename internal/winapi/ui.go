package winapi

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// Shared Win32 plumbing for the popups (internal/popup) and the settings
// window (internal/configui).

var (
	uiUser32 = windows.NewLazySystemDLL("user32.dll")
	uiGdi32  = windows.NewLazySystemDLL("gdi32.dll")

	procGetDpiForSystem               = uiUser32.NewProc("GetDpiForSystem")
	procSetProcessDpiAwarenessContext = uiUser32.NewProc("SetProcessDpiAwarenessContext")
	procSetProcessDPIAware            = uiUser32.NewProc("SetProcessDPIAware")
	procCreateFontW                   = uiGdi32.NewProc("CreateFontW")
)

// Call calls a Win32 procedure and returns its first result.
func Call(p *windows.LazyProc, a ...uintptr) uintptr {
	r, _, _ := p.Call(a...)
	return r
}

// U16 converts s to a NUL-terminated UTF-16 string.
func U16(s string) *uint16 {
	p, _ := windows.UTF16PtrFromString(s)
	return p
}

// Ptr passes a Go pointer to a Win32 call.
func Ptr[T any](v *T) uintptr { return uintptr(unsafe.Pointer(v)) }

// Rect is a Win32 RECT.
type Rect struct{ Left, Top, Right, Bottom int32 }

// Contains reports whether the point lies inside the rectangle.
func (r Rect) Contains(x, y int32) bool {
	return x >= r.Left && x < r.Right && y >= r.Top && y < r.Bottom
}

// Point is a Win32 POINT.
type Point struct{ X, Y int32 }

// Msg is a Win32 MSG.
type Msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      Point
	Private uint32
}

// WndClassEx is a Win32 WNDCLASSEXW.
type WndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

// SetDPIAware makes the process per-monitor DPI aware (V2), falling back to
// system DPI awareness on older Windows.
func SetDPIAware() {
	if procSetProcessDpiAwarenessContext.Find() == nil {
		if Call(procSetProcessDpiAwarenessContext, ^uintptr(3)) != 0 { // PER_MONITOR_AWARE_V2 (-4)
			return
		}
	}
	Call(procSetProcessDPIAware)
}

// SystemDPI returns the system DPI (96 if it can't be read).
func SystemDPI() int32 {
	if procGetDpiForSystem.Find() == nil {
		if d := int32(Call(procGetDpiForSystem)); d > 0 {
			return d
		}
	}
	return 96
}

// CreateFont creates a Segoe UI font of px pixels with ClearType.
func CreateFont(px int32, weight int32) uintptr {
	return Call(procCreateFontW, uintptr(-px), 0, 0, 0, uintptr(weight), 0, 0, 0,
		1 /*DEFAULT_CHARSET*/, 0, 0, 5 /*CLEARTYPE_QUALITY*/, 0, Ptr(U16("Segoe UI")))
}
