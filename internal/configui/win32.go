package configui

import (
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	comdlg32 = windows.NewLazySystemDLL("comdlg32.dll")

	pRegisterClassExW              = user32.NewProc("RegisterClassExW")
	pCreateWindowExW               = user32.NewProc("CreateWindowExW")
	pDefWindowProcW                = user32.NewProc("DefWindowProcW")
	pDestroyWindow                 = user32.NewProc("DestroyWindow")
	pShowWindow                    = user32.NewProc("ShowWindow")
	pUpdateWindow                  = user32.NewProc("UpdateWindow")
	pGetMessageW                   = user32.NewProc("GetMessageW")
	pIsDialogMessageW              = user32.NewProc("IsDialogMessageW")
	pTranslateMessage              = user32.NewProc("TranslateMessage")
	pDispatchMessageW              = user32.NewProc("DispatchMessageW")
	pPostQuitMessage               = user32.NewProc("PostQuitMessage")
	pSendMessageW                  = user32.NewProc("SendMessageW")
	pSetWindowTextW                = user32.NewProc("SetWindowTextW")
	pGetWindowTextW                = user32.NewProc("GetWindowTextW")
	pGetWindowTextLengthW          = user32.NewProc("GetWindowTextLengthW")
	pLoadCursorW                   = user32.NewProc("LoadCursorW")
	pLoadIconW                     = user32.NewProc("LoadIconW")
	pAdjustWindowRectEx            = user32.NewProc("AdjustWindowRectEx")
	pGetSystemMetrics              = user32.NewProc("GetSystemMetrics")
	pSetFocus                      = user32.NewProc("SetFocus")
	pEnableWindow                  = user32.NewProc("EnableWindow")
	pGetDpiForSystem               = user32.NewProc("GetDpiForSystem")
	pSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	pSetProcessDPIAware            = user32.NewProc("SetProcessDPIAware")
	pGetOpenFileNameW              = comdlg32.NewProc("GetOpenFileNameW")
	pGetSaveFileNameW              = comdlg32.NewProc("GetSaveFileNameW")
	pGetModuleHandleW              = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetModuleHandleW")
)

const (
	wsOverlapped    = 0x00000000
	wsCaption       = 0x00C00000
	wsSysMenu       = 0x00080000
	wsMinimizeBox   = 0x00020000
	wsChild         = 0x40000000
	wsVisible       = 0x10000000
	wsTabStop       = 0x00010000
	wsGroup         = 0x00020000
	wsVScroll       = 0x00200000
	wsExControlPar  = 0x00010000
	wsExClientEdge  = 0x00000200
	esMultiline     = 0x0004
	esAutoVScroll   = 0x0040
	esAutoHScroll   = 0x0080
	esWantReturn    = 0x1000
	esNumber        = 0x2000
	bsPushButton    = 0x0000
	bsDefPushButton = 0x0001
	bsAutoCheckBox  = 0x0003
	bsGroupBox      = 0x0007
	bsNotify        = 0x4000
	ssLeft          = 0x0000
	ssLeftNoWrap    = 0x000C
	wmCreate        = 0x0001
	wmDestroy       = 0x0002
	wmClose         = 0x0010
	wmSetFont       = 0x0030
	wmCommand       = 0x0111
	bmGetCheck      = 0x00F0
	bmSetCheck      = 0x00F1
	emLimitText     = 0x00C5
	bcmSetShield    = 0x160C
	enSetFocus      = 0x0100
	enChange        = 0x0300
	bnClicked       = 0
	bnSetFocus      = 6
	swShow          = 5
	smCxScreen      = 0
	smCyScreen      = 1
	colorBtnFace    = 15
	idcArrow        = 32512
	idiApplication  = 32512
	ofnOverwrite    = 0x00000002
	ofnNoChangeDir  = 0x00000008
	ofnPathMust     = 0x00000800
	ofnFileMust     = 0x00001000
	ofnExplorer     = 0x00080000
)

// Shared Win32 types and helpers live in internal/winapi.
type (
	rect       = winapi.Rect
	msg        = winapi.Msg
	wndClassEx = winapi.WndClassEx
)

var (
	call = winapi.Call
	u16  = winapi.U16
)

func ptr[T any](v *T) uintptr { return winapi.Ptr(v) }

type openFileName struct {
	StructSize    uint32
	Owner         uintptr
	Instance      uintptr
	Filter        *uint16
	CustomFilter  *uint16
	MaxCustFilter uint32
	FilterIndex   uint32
	File          *uint16
	MaxFile       uint32
	FileTitle     *uint16
	MaxFileTitle  uint32
	InitialDir    *uint16
	Title         *uint16
	Flags         uint32
	FileOffset    uint16
	FileExtension uint16
	DefExt        *uint16
	CustData      uintptr
	FnHook        uintptr
	TemplateName  *uint16
	Reserved      uintptr
	Reserved2     uint32
	FlagsEx       uint32
}

func sizeOf[T any](v T) uintptr { return unsafe.Sizeof(v) }

func setText(h uintptr, s string) { call(pSetWindowTextW, h, ptr(u16(s))) }

func getText(h uintptr) string {
	n := call(pGetWindowTextLengthW, h)
	buf := make([]uint16, n+1)
	call(pGetWindowTextW, h, ptr(&buf[0]), n+1)
	return windows.UTF16ToString(buf)
}

// fileDialog shows the standard Open/Save dialog for JSON files.
func fileDialog(owner uintptr, save bool, title, initial string) (string, bool) {
	filter, _ := windows.UTF16FromString("Settings files (*.json)|*.json|All files (*.*)|*.*|")
	for i, c := range filter {
		if c == '|' {
			filter[i] = 0
		}
	}
	buf := make([]uint16, 1024)
	copy(buf, windows.StringToUTF16(initial))
	ofn := openFileName{
		Owner: owner, Filter: &filter[0], FilterIndex: 1, File: &buf[0], MaxFile: uint32(len(buf)),
		Title: u16(title), DefExt: u16("json"), Flags: ofnExplorer | ofnNoChangeDir | ofnPathMust,
	}
	ofn.StructSize = uint32(unsafe.Sizeof(ofn))
	p := pGetOpenFileNameW
	if save {
		ofn.Flags |= ofnOverwrite
		p = pGetSaveFileNameW
	} else {
		ofn.Flags |= ofnFileMust
	}
	if call(p, ptr(&ofn)) == 0 {
		return "", false
	}
	return windows.UTF16ToString(buf), true
}
