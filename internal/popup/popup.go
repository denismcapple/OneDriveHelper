// Package popup draws OneDrive Helper's own alert cards in the bottom-right
// corner of the screen. They are deliberately different from Windows toasts:
// a coloured header with the brand name, they stay on top, they don't steal
// keyboard focus, and they stay until the user picks an action.
package popup

import (
	"errors"
	"runtime"
	"sync"

	"golang.org/x/sys/windows"
)

// Severity selects the header colour.
type Severity int

const (
	Warning  Severity = iota // amber
	Critical                 // red
	Info                     // accent colour (progress / information)
	OK                       // green (all good)
)

// MenuItem is an entry of a button's drop-down menu.
type MenuItem struct {
	Label  string
	Action func()
}

// Button is an action on the card. Actions run on their own goroutine.
type Button struct {
	Label   string
	Primary bool
	Dismiss bool       // hide the card when clicked (for menus: when an entry is chosen)
	Menu    []MenuItem // if set, clicking opens this menu
	Action  func()
	Stay    bool // Ask/AskWithin: run Action but keep waiting (e.g. "Copy")
}

// Item is one listed problem: a bold title, an optional path on its own
// line in a smaller font, and an optional explanatory note.
type Item struct {
	Title, Path, Note string
}

// Spec describes a card. Showing a Spec with an existing ID updates that card.
type Spec struct {
	ID       string
	Severity Severity
	Brand    string
	Title    string
	Body     string
	Items    []Item
	Footer   string // text after the items
	Buttons  []Button
	Sound    bool
	Accent   string // "#RRGGBB" for primary buttons and Info headers; empty = default blue
	OnHelp   func() // if set, the header shows "What does this mean? (i)" which calls this
	Logo     string // PNG file shown above the title (skipped if missing or unusable)
}

// Manager owns the UI thread and all cards.
type Manager struct {
	tid     uint32
	reqs    chan func()
	mu      sync.Mutex
	visible map[string]bool

	// UI-thread only:
	cards  []*card
	byHwnd map[uintptr]*card
	dpi    int32
	fonts  fonts
	class  *uint16
	inst   uintptr
	tray   trayState
	logos  map[string]logoEntry
}

type fonts struct{ brand, title, body, button, small uintptr }

var (
	wndProcCB   uintptr
	cbOnce      sync.Once
	activeMgrMu sync.Mutex
	activeMgr   *Manager
)

// Start creates the UI thread. Only one Manager per process is supported;
// use Shared to get it from anywhere in the process.
func Start() (*Manager, error) {
	activeMgrMu.Lock()
	defer activeMgrMu.Unlock()
	if activeMgr != nil {
		return nil, errors.New("popup manager already running")
	}
	return start()
}

// Shared returns the process's Manager, starting it if needed.
func Shared() (*Manager, error) {
	activeMgrMu.Lock()
	defer activeMgrMu.Unlock()
	if activeMgr != nil {
		return activeMgr, nil
	}
	return start()
}

func start() (*Manager, error) {
	m := &Manager{reqs: make(chan func(), 64), visible: map[string]bool{}, byHwnd: map[uintptr]*card{}}
	ready := make(chan error, 1)
	go m.loop(ready)
	if err := <-ready; err != nil {
		return nil, err
	}
	activeMgr = m
	return m, nil
}

func (m *Manager) loop(ready chan<- error) {
	runtime.LockOSThread()
	setDPIAware()
	m.tid = windows.GetCurrentThreadId()
	var q msg
	call(pPeekMessageW, ptr(&q), 0, 0, 0, pmNoRemove) // create the thread's message queue

	cbOnce.Do(func() { wndProcCB = windows.NewCallback(wndProc) })
	m.class = u16("OneDriveHelperPopup")
	m.inst = call(pGetModuleHandleW, 0)
	wc := wndClassEx{Style: csDropShadow, WndProc: wndProcCB, Instance: m.inst, ClassName: m.class, Cursor: call(pLoadCursorW, 0, idcArrow)}
	wc.Size = uint32(unsafeSizeof(wc))
	if call(pRegisterClassExW, ptr(&wc)) == 0 {
		ready <- errors.New("RegisterClassEx failed")
		return
	}
	m.dpi = systemDPI()
	if ForceDPI > 0 {
		m.dpi = ForceDPI
	}
	m.fonts = fonts{
		brand:  createFont(m.s(12), 600),
		title:  createFont(m.s(17), 600),
		body:   createFont(m.s(14), 400),
		button: createFont(m.s(14), 600),
		small:  createFont(m.s(12), 400),
	}
	ready <- nil

	for {
		r := int32(call(pGetMessageW, ptr(&q), 0, 0, 0))
		if r <= 0 {
			return
		}
		if q.Hwnd == 0 && q.Message == wmApp {
			m.drain()
			continue
		}
		call(pTranslateMessage, ptr(&q))
		call(pDispatchMessageW, ptr(&q))
	}
}

func (m *Manager) drain() {
	for {
		select {
		case f := <-m.reqs:
			f()
		default:
			return
		}
	}
}

// do runs f on the UI thread.
func (m *Manager) do(f func()) {
	if m == nil {
		return
	}
	m.reqs <- f
	call(pPostThreadMessageW, uintptr(m.tid), wmApp, 0, 0)
}

// rewake re-posts the wake-up after a modal loop (a popup menu). Windows
// drops thread messages while one runs, so requests made meanwhile would
// otherwise wait for the next unrelated request. UI thread only.
func (m *Manager) rewake() {
	if len(m.reqs) > 0 {
		call(pPostThreadMessageW, uintptr(m.tid), wmApp, 0, 0)
	}
}

// s scales a 96-DPI value.
func (m *Manager) s(v int32) int32 { return v * m.dpi / 96 }

// Show displays (or updates) a card.
func (m *Manager) Show(s Spec) {
	if m == nil {
		return
	}
	m.setVisible(s.ID, true)
	m.do(func() { m.show(s) })
}

// SetButtonLabel changes the label of button i of a shown card.
func (m *Manager) SetButtonLabel(id string, i int, label string) {
	if m == nil {
		return
	}
	m.do(func() {
		for _, c := range m.cards {
			if c.spec.ID == id && i >= 0 && i < len(c.spec.Buttons) {
				c.spec.Buttons[i].Label = label
				c.layout()
				m.arrange()
				call(pInvalidateRect, c.hwnd, 0, 0)
			}
		}
	})
}

// Hide removes a card if shown.
func (m *Manager) Hide(id string) {
	if m == nil {
		return
	}
	m.setVisible(id, false)
	m.do(func() { m.hide(id) })
}

// Visible reports whether a card is currently shown (or about to be).
func (m *Manager) Visible(id string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.visible[id]
}

// AnyVisible reports whether any card is shown.
func (m *Manager) AnyVisible() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.visible {
		if v {
			return true
		}
	}
	return false
}

func (m *Manager) setVisible(id string, v bool) {
	m.mu.Lock()
	m.visible[id] = v
	m.mu.Unlock()
}

func (m *Manager) show(s Spec) {
	for _, c := range m.cards {
		if c.spec.ID == s.ID {
			c.spec = s
			c.hover = -1
			c.layout()
			m.arrange()
			call(pInvalidateRect, c.hwnd, 0, 0)
			return
		}
	}
	hwnd := call(pCreateWindowExW, wsExTopmost|wsExToolWindow|wsExNoActivate, ptr(m.class), ptr(u16(s.Brand)),
		wsPopup, 0, 0, 10, 10, 0, 0, m.inst, 0)
	if hwnd == 0 {
		m.setVisible(s.ID, false)
		return
	}
	c := &card{m: m, hwnd: hwnd, spec: s, hover: -1}
	m.byHwnd[hwnd] = c
	m.cards = append(m.cards, c)
	c.layout()
	// Windows 11: rounded corners (ignored on Windows 10).
	pref := int32(2) // DWMWCP_ROUND
	if pDwmSetWindowAttribute.Find() == nil {
		call(pDwmSetWindowAttribute, hwnd, 33, ptr(&pref), 4)
	}
	m.arrange()
	if call(pAnimateWindow, hwnd, 220, awSlide|awVerNegative) == 0 {
		call(pShowWindow, hwnd, swShowNoActive)
	}
	call(pSetTimer, hwnd, topmostTimerID, 5000, 0)
	if s.Sound {
		call(pMessageBeep, mbIconWarning)
	}
}

func (m *Manager) hide(id string) {
	for i, c := range m.cards {
		if c.spec.ID == id {
			call(pKillTimer, c.hwnd, topmostTimerID)
			call(pDestroyWindow, c.hwnd)
			delete(m.byHwnd, c.hwnd)
			m.cards = append(m.cards[:i], m.cards[i+1:]...)
			m.arrange()
			return
		}
	}
}

// arrange stacks the cards upwards from the bottom-right of the work area.
func (m *Manager) arrange() {
	wa := workArea()
	margin, gap := m.s(16), m.s(10)
	y := wa.Bottom - margin
	for _, c := range m.cards {
		y -= c.h
		call(pSetWindowPos, c.hwnd, hwndTopmost, uintptr(wa.Right-margin-c.w), uintptr(y), uintptr(c.w), uintptr(c.h), swpNoActivate)
		y -= gap
	}
}

func wndProc(hwnd, message, wParam, lParam uintptr) uintptr {
	activeMgrMu.Lock()
	m := activeMgr
	activeMgrMu.Unlock()
	var c *card
	if m != nil {
		if r, ok := m.trayProc(hwnd, message, wParam, lParam); ok {
			return r
		}
		c = m.byHwnd[hwnd]
	}
	if c == nil {
		return call(pDefWindowProcW, hwnd, message, wParam, lParam)
	}
	switch message {
	case wmPaint:
		var ps paintStruct
		hdc := call(pBeginPaint, hwnd, ptr(&ps))
		c.paint(hdc)
		call(pEndPaint, hwnd, ptr(&ps))
		return 0
	case wmPrintClient:
		c.paint(wParam)
		return 0
	case wmEraseBkgnd:
		return 1
	case wmMouseActivate:
		return maNoActivate
	case wmClose:
		return 0 // only the buttons close a card
	case wmTimer:
		call(pSetWindowPos, hwnd, hwndTopmost, 0, 0, 0, 0, swpNoMove|swpNoSize|swpNoActivate)
		return 0
	case wmSetCursor:
		cur := uintptr(idcArrow)
		if c.hover != -1 {
			cur = idcHand
		}
		call(pSetCursor, call(pLoadCursorW, 0, cur))
		return 1
	case wmMouseMove:
		if !c.tracking {
			tme := trackMouseEvent{Flags: tmeLeave, Hwnd: hwnd}
			tme.Size = uint32(unsafeSizeof(tme))
			call(pTrackMouseEvent, ptr(&tme))
			c.tracking = true
		}
		c.setHover(c.hit(lParam))
		return 0
	case wmMouseLeave:
		c.tracking = false
		c.setHover(-1)
		return 0
	case wmLButtonUp:
		if i := c.hit(lParam); i != -1 {
			c.click(i)
		}
		return 0
	case wmMouseWheel:
		c.scrollBy(-int32(int16(wParam>>16)) * m.s(48) / 120)
		return 0
	}
	return call(pDefWindowProcW, hwnd, message, wParam, lParam)
}
