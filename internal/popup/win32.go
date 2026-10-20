package popup

import (
	"golang.org/x/sys/windows"

	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

var (
	user32 = windows.NewLazySystemDLL("user32.dll")
	gdi32  = windows.NewLazySystemDLL("gdi32.dll")
	dwmapi = windows.NewLazySystemDLL("dwmapi.dll")

	pRegisterClassExW       = user32.NewProc("RegisterClassExW")
	pCreateWindowExW        = user32.NewProc("CreateWindowExW")
	pDefWindowProcW         = user32.NewProc("DefWindowProcW")
	pDestroyWindow          = user32.NewProc("DestroyWindow")
	pShowWindow             = user32.NewProc("ShowWindow")
	pSetWindowPos           = user32.NewProc("SetWindowPos")
	pGetMessageW            = user32.NewProc("GetMessageW")
	pPeekMessageW           = user32.NewProc("PeekMessageW")
	pTranslateMessage       = user32.NewProc("TranslateMessage")
	pDispatchMessageW       = user32.NewProc("DispatchMessageW")
	pPostThreadMessageW     = user32.NewProc("PostThreadMessageW")
	pPostMessageW           = user32.NewProc("PostMessageW")
	pBeginPaint             = user32.NewProc("BeginPaint")
	pEndPaint               = user32.NewProc("EndPaint")
	pInvalidateRect         = user32.NewProc("InvalidateRect")
	pGetDC                  = user32.NewProc("GetDC")
	pReleaseDC              = user32.NewProc("ReleaseDC")
	pFillRect               = user32.NewProc("FillRect")
	pFrameRect              = user32.NewProc("FrameRect")
	pDrawTextW              = user32.NewProc("DrawTextW")
	pLoadCursorW            = user32.NewProc("LoadCursorW")
	pSetCursor              = user32.NewProc("SetCursor")
	pSystemParametersInfoW  = user32.NewProc("SystemParametersInfoW")
	pTrackMouseEvent        = user32.NewProc("TrackMouseEvent")
	pSetTimer               = user32.NewProc("SetTimer")
	pKillTimer              = user32.NewProc("KillTimer")
	pCreatePopupMenu        = user32.NewProc("CreatePopupMenu")
	pAppendMenuW            = user32.NewProc("AppendMenuW")
	pTrackPopupMenu         = user32.NewProc("TrackPopupMenu")
	pDestroyMenu            = user32.NewProc("DestroyMenu")
	pClientToScreen         = user32.NewProc("ClientToScreen")
	pSetForegroundWindow    = user32.NewProc("SetForegroundWindow")
	pAnimateWindow          = user32.NewProc("AnimateWindow")
	pMessageBeep            = user32.NewProc("MessageBeep")
	pPostQuitMessage        = user32.NewProc("PostQuitMessage")
	pSelectObject           = gdi32.NewProc("SelectObject")
	pDeleteObject           = gdi32.NewProc("DeleteObject")
	pDeleteDC               = gdi32.NewProc("DeleteDC")
	pSetBkMode              = gdi32.NewProc("SetBkMode")
	pSetTextColor           = gdi32.NewProc("SetTextColor")
	pCreateSolidBrush       = gdi32.NewProc("CreateSolidBrush")
	pCreatePen              = gdi32.NewProc("CreatePen")
	pRoundRect              = gdi32.NewProc("RoundRect")
	pEllipse                = gdi32.NewProc("Ellipse")
	pCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	pCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	pBitBlt                 = gdi32.NewProc("BitBlt")
	pSaveDC                 = gdi32.NewProc("SaveDC")
	pRestoreDC              = gdi32.NewProc("RestoreDC")
	pIntersectClipRect      = gdi32.NewProc("IntersectClipRect")
	pMsgWaitForMultipleObj  = user32.NewProc("MsgWaitForMultipleObjectsEx")
	pDwmSetWindowAttribute  = dwmapi.NewProc("DwmSetWindowAttribute")
	pGetModuleHandleW       = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetModuleHandleW")
)

const (
	wsPopup         = 0x80000000
	wsExTopmost     = 0x00000008
	wsExToolWindow  = 0x00000080
	wsExNoActivate  = 0x08000000
	csDropShadow    = 0x00020000
	wmNull          = 0x0000
	wmPaint         = 0x000F
	wmClose         = 0x0010
	wmEraseBkgnd    = 0x0014
	wmSetCursor     = 0x0020
	wmMouseActivate = 0x0021
	wmTimer         = 0x0113
	wmMouseMove     = 0x0200
	wmLButtonUp     = 0x0202
	wmMouseWheel    = 0x020A
	wmQuit          = 0x0012
	pmRemove        = 1
	qsAllInput      = 0x04FF
	mwmoInputAvail  = 0x0004
	wmMouseLeave    = 0x02A3
	wmPrintClient   = 0x0318
	wmApp           = 0x8000
	maNoActivate    = 3
	swHide          = 0
	swShowNoActive  = 4
	swpNoSize       = 0x1
	swpNoMove       = 0x2
	swpNoActivate   = 0x10
	spiGetWorkArea  = 0x30
	dtCenter        = 0x1
	dtVCenter       = 0x4
	dtWordBreak     = 0x10
	dtSingleLine    = 0x20
	dtCalcRect      = 0x400
	dtNoPrefix      = 0x800
	dtEditControl   = 0x2000 // break long words such as paths
	dtEndEllipsis   = 0x8000
	dtPathEllipsis  = 0x4000
	bkTransparent   = 1
	srcCopy         = 0x00CC0020
	tpmReturnCmd    = 0x0100
	tpmNoNotify     = 0x0080
	tpmBottomAlign  = 0x0020
	tmeLeave        = 0x2
	idcArrow        = 32512
	idcHand         = 32649
	awSlide         = 0x00040000
	awVerNegative   = 0x8
	pmNoRemove      = 0
	mbIconWarning   = 0x30
	topmostTimerID  = 1
)

var hwndTopmost = ^uintptr(0) // (HWND)-1

// Shared Win32 types and helpers live in internal/winapi.
type (
	rect       = winapi.Rect
	point      = winapi.Point
	msg        = winapi.Msg
	wndClassEx = winapi.WndClassEx
)

var (
	call        = winapi.Call
	u16         = winapi.U16
	createFont  = winapi.CreateFont
	setDPIAware = winapi.SetDPIAware
	systemDPI   = winapi.SystemDPI
)

func ptr[T any](v *T) uintptr { return winapi.Ptr(v) }

type paintStruct struct {
	Hdc       uintptr
	Erase     int32
	Paint     rect
	Restore   int32
	IncUpdate int32
	Reserved  [32]byte
}

type trackMouseEvent struct {
	Size      uint32
	Flags     uint32
	Hwnd      uintptr
	HoverTime uint32
}

// rgb builds a COLORREF.
func rgb(r, g, b byte) uintptr { return uintptr(r) | uintptr(g)<<8 | uintptr(b)<<16 }

func drawText(hdc uintptr, s string, r *rect, flags uint32) int32 {
	u, _ := windows.UTF16FromString(s)
	return int32(call(pDrawTextW, hdc, ptr(&u[0]), uintptr(len(u)-1), ptr(r), uintptr(flags)))
}

func fill(hdc uintptr, r rect, color uintptr) {
	b := call(pCreateSolidBrush, color)
	call(pFillRect, hdc, ptr(&r), b)
	call(pDeleteObject, b)
}

func roundRect(hdc uintptr, r rect, radius int32, fillColor, border uintptr) {
	b := call(pCreateSolidBrush, fillColor)
	p := call(pCreatePen, 0, 1, border)
	ob, op := call(pSelectObject, hdc, b), call(pSelectObject, hdc, p)
	call(pRoundRect, hdc, uintptr(r.Left), uintptr(r.Top), uintptr(r.Right), uintptr(r.Bottom), uintptr(radius), uintptr(radius))
	call(pSelectObject, hdc, ob)
	call(pSelectObject, hdc, op)
	call(pDeleteObject, b)
	call(pDeleteObject, p)
}

func ellipse(hdc uintptr, r rect, color uintptr) {
	b := call(pCreateSolidBrush, color)
	p := call(pCreatePen, 0, 1, color)
	ob, op := call(pSelectObject, hdc, b), call(pSelectObject, hdc, p)
	call(pEllipse, hdc, uintptr(r.Left), uintptr(r.Top), uintptr(r.Right), uintptr(r.Bottom))
	call(pSelectObject, hdc, ob)
	call(pSelectObject, hdc, op)
	call(pDeleteObject, b)
	call(pDeleteObject, p)
}

// circle draws an ellipse with a fill colour and an outline of the given width.
func circle(hdc uintptr, r rect, fillColor, border uintptr, width int32) {
	b := call(pCreateSolidBrush, fillColor)
	p := call(pCreatePen, 0, uintptr(width), border)
	ob, op := call(pSelectObject, hdc, b), call(pSelectObject, hdc, p)
	call(pEllipse, hdc, uintptr(r.Left+width/2), uintptr(r.Top+width/2), uintptr(r.Right-width/2), uintptr(r.Bottom-width/2))
	call(pSelectObject, hdc, ob)
	call(pSelectObject, hdc, op)
	call(pDeleteObject, b)
	call(pDeleteObject, p)
}

func workArea() rect {
	var r rect
	call(pSystemParametersInfoW, spiGetWorkArea, 0, ptr(&r), 0)
	return r
}
