package popup

import (
	"errors"
	"math"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	shell32                   = windows.NewLazySystemDLL("shell32.dll")
	pShellNotifyIconW         = shell32.NewProc("Shell_NotifyIconW")
	pRegisterWindowMessageW   = user32.NewProc("RegisterWindowMessageW")
	pGetCursorPos             = user32.NewProc("GetCursorPos")
	pGetSystemMetrics         = user32.NewProc("GetSystemMetrics")
	pCreateIconIndirect       = user32.NewProc("CreateIconIndirect")
	pLoadIconW                = user32.NewProc("LoadIconW")
	pDestroyIcon              = user32.NewProc("DestroyIcon")
	pOpenClipboard            = user32.NewProc("OpenClipboard")
	pEmptyClipboard           = user32.NewProc("EmptyClipboard")
	pSetClipboardData         = user32.NewProc("SetClipboardData")
	pCloseClipboard           = user32.NewProc("CloseClipboard")
	pCreateDIBSection         = gdi32.NewProc("CreateDIBSection")
	pCreateBitmap             = gdi32.NewProc("CreateBitmap")
	pGlobalAlloc              = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalAlloc")
	pGlobalLock               = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalLock")
	pGlobalUnlock             = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalUnlock")
	pGlobalFree               = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalFree")
	pAppendMenuSeparatorFlags = uintptr(0x800) // MF_SEPARATOR
)

const (
	trayCallback  = wmApp + 1
	nimAdd        = 0
	nimModify     = 1
	nimDelete     = 2
	nifMessage    = 0x1
	nifIcon       = 0x2
	nifTip        = 0x4
	wmLButtonUpT  = 0x0202
	wmRButtonUpT  = 0x0205
	mfGrayed      = 0x1
	tpmRightBtn   = 0x0002
	smCxSmIcon    = 49
	cfUnicodeText = 13
	gmemMoveable  = 0x2
)

type notifyIconData struct {
	Size            uint32
	Hwnd            uintptr
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            uintptr
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	Version         uint32
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GUID            windows.GUID
	BalloonIcon     uintptr
}

// TrayItem is an entry of the tray icon's right-click menu.
type TrayItem struct {
	Label     string
	Action    func()
	Disabled  bool
	Separator bool
}

// Tray describes the notification-area icon.
type Tray struct {
	Tooltip  string
	Severity Severity
	OnClick  func()            // left click
	Menu     func() []TrayItem // built when the menu opens
}

type trayState struct {
	hwnd           uintptr
	added          bool
	spec           Tray
	icons          map[Severity]uintptr
	taskbarCreated uint32
}

// SetTray adds or updates the tray icon.
func (m *Manager) SetTray(t Tray) {
	if m == nil {
		return
	}
	m.do(func() { m.setTray(t) })
}

// RemoveTray removes the tray icon and waits until it is gone.
func (m *Manager) RemoveTray() {
	if m == nil {
		return
	}
	m.doSync(func() {
		if m.tray.added {
			nid := m.nid()
			call(pShellNotifyIconW, nimDelete, ptr(&nid))
			m.tray.added = false
		}
	})
}

// CopyText puts text on the clipboard.
func (m *Manager) CopyText(s string) error {
	if m == nil {
		return errors.New("no UI")
	}
	var err error
	m.doSync(func() { err = m.copyText(s) })
	return err
}

func (m *Manager) doSync(f func()) {
	done := make(chan struct{})
	m.do(func() { f(); close(done) })
	<-done
}

func (m *Manager) ensureTrayWindow() {
	if m.tray.hwnd != 0 {
		return
	}
	m.tray.icons = map[Severity]uintptr{}
	m.tray.taskbarCreated = uint32(call(pRegisterWindowMessageW, ptr(u16("TaskbarCreated"))))
	m.tray.hwnd = call(pCreateWindowExW, wsExToolWindow, ptr(m.class), ptr(u16("OneDrive Helper tray")), wsPopup, 0, 0, 0, 0, 0, 0, m.inst, 0)
}

func (m *Manager) nid() notifyIconData {
	n := notifyIconData{Hwnd: m.tray.hwnd, ID: 1, Flags: nifMessage | nifIcon | nifTip, CallbackMessage: trayCallback}
	n.Size = uint32(unsafe.Sizeof(n))
	n.Icon = m.icon(m.tray.spec.Severity)
	tip := windows.StringToUTF16(m.tray.spec.Tooltip)
	if len(tip) > len(n.Tip) {
		tip = append(tip[:len(n.Tip)-1], 0)
	}
	copy(n.Tip[:], tip)
	return n
}

func (m *Manager) setTray(t Tray) {
	m.ensureTrayWindow()
	if m.tray.hwnd == 0 {
		return
	}
	m.tray.spec = t
	nid := m.nid()
	if m.tray.added && call(pShellNotifyIconW, nimModify, ptr(&nid)) != 0 {
		return
	}
	m.tray.added = call(pShellNotifyIconW, nimAdd, ptr(&nid)) != 0
}

// trayProc handles messages for the hidden tray window. ok=false means "not handled".
func (m *Manager) trayProc(hwnd, message, wParam, lParam uintptr) (uintptr, bool) {
	if hwnd != m.tray.hwnd || hwnd == 0 {
		return 0, false
	}
	switch {
	case message == trayCallback:
		switch lParam & 0xFFFF {
		case wmLButtonUpT:
			if f := m.tray.spec.OnClick; f != nil {
				go f()
			}
		case wmRButtonUpT:
			m.trayMenu()
		}
		return 0, true
	case m.tray.taskbarCreated != 0 && uint32(message) == m.tray.taskbarCreated:
		m.tray.added = false // Explorer restarted: add the icon again
		m.setTray(m.tray.spec)
		return 0, true
	}
	return 0, false
}

func (m *Manager) trayMenu() {
	if m.tray.spec.Menu == nil {
		return
	}
	items := m.tray.spec.Menu()
	menu := call(pCreatePopupMenu)
	defer call(pDestroyMenu, menu)
	for i, it := range items {
		switch {
		case it.Separator:
			call(pAppendMenuW, menu, pAppendMenuSeparatorFlags, 0, 0)
		case it.Disabled:
			call(pAppendMenuW, menu, mfGrayed, uintptr(i+1), ptr(u16(it.Label)))
		default:
			call(pAppendMenuW, menu, 0, uintptr(i+1), ptr(u16(it.Label)))
		}
	}
	var pt point
	call(pGetCursorPos, ptr(&pt))
	call(pSetForegroundWindow, m.tray.hwnd) // so the menu closes when clicking elsewhere
	cmd := int(call(pTrackPopupMenu, menu, tpmReturnCmd|tpmNoNotify|tpmRightBtn|tpmBottomAlign, uintptr(pt.X), uintptr(pt.Y), 0, m.tray.hwnd, 0))
	call(pPostMessageW, m.tray.hwnd, wmNull, 0, 0)
	if cmd >= 1 && cmd <= len(items) && items[cmd-1].Action != nil {
		go items[cmd-1].Action()
	}
}

func (m *Manager) copyText(s string) error {
	runtime.LockOSThread() // already locked on the UI thread; harmless
	if call(pOpenClipboard, m.tray.hwnd) == 0 {
		return errors.New("the clipboard is in use by another program")
	}
	defer call(pCloseClipboard)
	call(pEmptyClipboard)
	u := windows.StringToUTF16(s)
	size := uintptr(len(u) * 2)
	h := call(pGlobalAlloc, gmemMoveable, size)
	if h == 0 {
		return errors.New("out of memory")
	}
	p := call(pGlobalLock, h)
	if p == 0 {
		call(pGlobalFree, h)
		return errors.New("out of memory")
	}
	mem := *(*unsafe.Pointer)(unsafe.Pointer(&p)) // OS-allocated memory, not Go memory
	copy(unsafe.Slice((*uint16)(mem), len(u)), u)
	call(pGlobalUnlock, h)
	if call(pSetClipboardData, cfUnicodeText, h) == 0 {
		call(pGlobalFree, h)
		return errors.New("could not set clipboard data")
	}
	return nil
}

// icon renders (and caches) a round status icon: green with a tick when OK,
// amber/red with "!" when there are problems.
func (m *Manager) icon(sev Severity) uintptr {
	if h, ok := m.tray.icons[sev]; ok {
		return h
	}
	n := int(call(pGetSystemMetrics, smCxSmIcon))
	if n <= 0 {
		n = 16
	}
	h := makeIcon(iconPixels(sev, n), n)
	m.tray.icons[sev] = h
	return h
}

// iconPixels draws the status icon as n*n straight-alpha ARGB pixels.
func iconPixels(sev Severity, n int) []uint32 {
	col := map[Severity][3]float64{
		OK: {0x10, 0x7C, 0x10}, Warning: {0xD9, 0x7A, 0x00}, Critical: {0xC4, 0x2B, 0x1C}, Info: {0x0F, 0x6C, 0xBD},
	}[sev]
	px := make([]uint32, n*n)
	fn, c := float64(n), float64(n)/2
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			disc := clamp01(c - 0.5 - math.Hypot(fx-c, fy-c) + 0.5) // anti-aliased edge
			if disc == 0 {
				continue
			}
			var glyph float64
			if sev == OK { // tick
				t := 0.075 * fn
				glyph = math.Max(seg(fx, fy, 0.28*fn, 0.52*fn, 0.44*fn, 0.68*fn, t), seg(fx, fy, 0.44*fn, 0.68*fn, 0.73*fn, 0.34*fn, t))
			} else { // exclamation mark
				t := 0.075 * fn
				glyph = math.Max(seg(fx, fy, c, 0.24*fn, c, 0.56*fn, t), seg(fx, fy, c, 0.74*fn, c, 0.745*fn, t*1.1))
			}
			r := col[0] + (255-col[0])*glyph
			g := col[1] + (255-col[1])*glyph
			b := col[2] + (255-col[2])*glyph
			px[y*n+x] = uint32(disc*255)<<24 | uint32(r)<<16 | uint32(g)<<8 | uint32(b)
		}
	}
	return px
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

// seg returns coverage of a round-capped line segment of half-width t.
func seg(px, py, ax, ay, bx, by, t float64) float64 {
	dx, dy := bx-ax, by-ay
	l := dx*dx + dy*dy
	u := 0.0
	if l > 0 {
		u = clamp01(((px-ax)*dx + (py-ay)*dy) / l)
	}
	return clamp01(t - math.Hypot(px-(ax+u*dx), py-(ay+u*dy)) + 0.5)
}

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type iconInfo struct {
	FIcon    int32
	XHotspot uint32
	YHotspot uint32
	Mask     uintptr
	Color    uintptr
}

func makeIcon(px []uint32, n int) uintptr {
	bih := bitmapInfoHeader{Width: int32(n), Height: -int32(n), Planes: 1, BitCount: 32}
	bih.Size = uint32(unsafe.Sizeof(bih))
	var bits unsafe.Pointer
	color := call(pCreateDIBSection, 0, ptr(&bih), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if color == 0 || bits == nil {
		return call(pLoadIconW, 0, 32515) // IDI_WARNING fallback
	}
	copy(unsafe.Slice((*uint32)(bits), len(px)), px)
	mask := call(pCreateBitmap, uintptr(n), uintptr(n), 1, 1, 0)
	ii := iconInfo{FIcon: 1, Mask: mask, Color: color}
	h := call(pCreateIconIndirect, ptr(&ii))
	call(pDeleteObject, color)
	call(pDeleteObject, mask)
	return h
}
