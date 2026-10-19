// Package configui is the settings editor window of OneDriveHelperConfig.exe.
// The form is generated from the config.Config field definitions.
package configui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/denismcapple/OneDriveHelper/internal/agent"
	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/popup"
	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

const windowTitle = "OneDrive Helper - Settings"

// Layout: two columns of grouped settings.
var layout = [][]struct {
	title  string
	fields []string
}{
	{
		{"Branding && support", []string{"CompanyName", "SupportName", "SupportEmail", "SupportPhone", "SupportURL", "SupportEmailMethod", "AccentColor"}},
		{"Alert text (leave blank for the built-in wording)", []string{"NotRunningMessage", "OrphanMessage"}},
	},
	{
		{"OneDrive running check", []string{"OneDriveCheckEnabled", "OneDriveMissingGraceSeconds", "AutoRestartEnabled", "AutoRestartAttempts", "IgnoreOptions"}},
		{"Library sync check", []string{"OrphanCheckEnabled", "RemediationEnabled", "BackupRoot", "FreeSpaceMarginMB"}},
		{"General", []string{"TrayIconEnabled", "PopupSound", "CheckIntervalSeconds", "AgentWatchdogSeconds"}},
	},
}

// Command IDs.
const (
	idImport = 1001 + iota
	idExport
	idDefaults
	idPreview
	idSave
	idClose
)

type form struct {
	hwnd    uintptr
	inst    uintptr
	font    uintptr
	bold    uintptr
	dpi     int32
	fields  map[string]config.Field
	ctrls   map[string]uintptr // field name -> control
	byHwnd  map[uintptr]string // control -> field name
	help    uintptr
	status  uintptr
	dirty   bool
	loading bool
	preview *popup.Manager
}

var current *form

// Run shows the settings window until it is closed. If file is set, the
// window starts with that settings file loaded instead of this PC's settings.
func Run(file string) error {
	runtime.LockOSThread()
	if pSetProcessDpiAwarenessContext.Find() == nil {
		if call(pSetProcessDpiAwarenessContext, ^uintptr(3)) == 0 { // PER_MONITOR_AWARE_V2
			call(pSetProcessDPIAware)
		}
	}
	winapi.EnableVisualStyles()
	f := &form{fields: map[string]config.Field{}, ctrls: map[string]uintptr{}, byHwnd: map[uintptr]string{}, dpi: 96}
	if pGetDpiForSystem.Find() == nil {
		f.dpi = int32(call(pGetDpiForSystem))
	}
	for _, fd := range config.Fields() {
		f.fields[fd.Name] = fd
	}
	current = f
	f.inst = call(pGetModuleHandleW, 0)
	f.font = call(pCreateFontW, uintptr(-f.s(15)), 0, 0, 0, 400, 0, 0, 0, 1, 0, 0, 5, 0, ptr(u16("Segoe UI")))
	f.bold = call(pCreateFontW, uintptr(-f.s(15)), 0, 0, 0, 600, 0, 0, 0, 1, 0, 0, 5, 0, ptr(u16("Segoe UI")))

	class := u16("OneDriveHelperConfig")
	wc := wndClassEx{
		WndProc: windows.NewCallback(wndProc), Instance: f.inst, ClassName: class,
		Cursor: call(pLoadCursorW, 0, idcArrow), Icon: call(pLoadIconW, 0, idiApplication), Background: colorBtnFace + 1,
	}
	wc.Size = uint32(sizeOf(wc))
	if call(pRegisterClassExW, ptr(&wc)) == 0 {
		return errors.New("RegisterClassEx failed")
	}

	w, h := f.s(1000), f.s(660)
	if len(config.PolicyOverrides()) > 0 {
		h += f.s(40)
	}
	style := uintptr(wsOverlapped | wsCaption | wsSysMenu | wsMinimizeBox)
	r := rect{0, 0, w, h}
	call(pAdjustWindowRectEx, ptr(&r), style, 0, wsExControlPar)
	ww, wh := r.Right-r.Left, r.Bottom-r.Top
	x := (int32(call(pGetSystemMetrics, smCxScreen)) - ww) / 2
	y := (int32(call(pGetSystemMetrics, smCyScreen)) - wh) / 2
	f.hwnd = call(pCreateWindowExW, wsExControlPar, ptr(class), ptr(u16(windowTitle)), style,
		uintptr(x), uintptr(y), uintptr(ww), uintptr(wh), 0, 0, f.inst, 0)
	if f.hwnd == 0 {
		return errors.New("CreateWindowEx failed")
	}
	f.build(h)
	f.load(config.LoadStored(), "Showing the settings saved on this PC.")
	if file != "" {
		c, _, err := config.ParseFile(file, config.Defaults())
		if err != nil {
			winapi.Error(windowTitle, "Could not read "+file+":\n\n"+err.Error())
		} else {
			f.load(c, "Opened "+filepath.Base(file)+" - not saved to this PC.")
		}
	}
	call(pShowWindow, f.hwnd, swShow)
	call(pUpdateWindow, f.hwnd)
	call(pSetFocus, f.ctrls["CompanyName"])

	var m msg
	for int32(call(pGetMessageW, ptr(&m), 0, 0, 0)) > 0 {
		if call(pIsDialogMessageW, f.hwnd, ptr(&m)) == 0 {
			call(pTranslateMessage, ptr(&m))
			call(pDispatchMessageW, ptr(&m))
		}
	}
	return nil
}

func (f *form) s(v int32) int32 { return v * f.dpi / 96 }

func (f *form) child(class, text string, style uintptr, exStyle uintptr, x, y, w, h int32, id int) uintptr {
	hwnd := call(pCreateWindowExW, exStyle, ptr(u16(class)), ptr(u16(text)), wsChild|wsVisible|style,
		uintptr(x), uintptr(y), uintptr(w), uintptr(h), f.hwnd, uintptr(id), f.inst, 0)
	call(pSendMessageW, hwnd, wmSetFont, f.font, 1)
	return hwnd
}

// build creates all controls.
func (f *form) build(clientH int32) {
	margin, colW, gap := f.s(16), f.s(476), f.s(16)
	labelW := f.s(222)
	rowH, editH, checkH, multiH := f.s(31), f.s(24), f.s(26), f.s(64)

	y0 := margin
	if p := config.PolicyOverrides(); len(p) > 0 {
		note := f.child("STATIC", fmt.Sprintf("Note: %d setting(s) on this PC are enforced by Group Policy (HKLM\\SOFTWARE\\Policies\\OneDriveHelper) and override what is saved here: %s",
			len(p), strings.Join(p, ", ")), ssLeft, 0, margin, y0, 2*colW+gap, f.s(34), 0)
		_ = note
		y0 += f.s(40)
	}
	for col, groups := range layout {
		x := margin + int32(col)*(colW+gap)
		y := y0
		for _, g := range groups {
			top := y
			inner := y + f.s(24)
			for _, name := range g.fields {
				fd := f.fields[name]
				cx, cw := x+f.s(12)+labelW, colW-f.s(24)-labelW
				var ctl uintptr
				switch {
				case fd.Type == reflect.Bool:
					ctl = f.child("BUTTON", fd.Label, wsTabStop|bsAutoCheckBox|bsNotify, 0, x+f.s(12), inner, colW-f.s(24), checkH, 0)
					inner += checkH + f.s(2)
				case fd.Kind == "multiline":
					f.child("STATIC", fd.Label, ssLeft, 0, x+f.s(12), inner+f.s(3), labelW-f.s(8), multiH, 0)
					ctl = f.child("EDIT", "", wsTabStop|wsVScroll|esMultiline|esAutoVScroll|esWantReturn, wsExClientEdge, cx, inner, cw, multiH, 0)
					call(pSendMessageW, ctl, emLimitText, 1000, 0)
					inner += multiH + f.s(8)
				default:
					f.child("STATIC", fd.Label, ssLeftNoWrap, 0, x+f.s(12), inner+f.s(4), labelW-f.s(8), editH, 0)
					style := uintptr(wsTabStop | esAutoHScroll)
					if fd.Type == reflect.Int {
						style |= esNumber
					}
					ctl = f.child("EDIT", "", style, wsExClientEdge, cx, inner, cw, editH, 0)
					call(pSendMessageW, ctl, emLimitText, 260, 0)
					inner += rowH
				}
				f.ctrls[name], f.byHwnd[ctl] = ctl, name
			}
			gb := f.child("BUTTON", g.title, bsGroupBox, 0, x, top, colW, inner-top+f.s(4), 0)
			call(pSendMessageW, gb, wmSetFont, f.bold, 1)
			y = inner + f.s(16)
		}
	}

	// Help line, status line and buttons along the bottom.
	btnH, btnW := f.s(32), f.s(130)
	by := clientH - margin - btnH
	f.help = f.child("STATIC", "Click a setting to see what it does.", ssLeft, 0, margin, by-f.s(52), 2*colW+gap, f.s(20), 0)
	f.status = f.child("STATIC", "", ssLeftNoWrap, 0, margin, by-f.s(28), 2*colW+gap, f.s(20), 0)
	call(pSendMessageW, f.status, wmSetFont, f.bold, 1)
	left := []struct {
		id    int
		label string
	}{{idImport, "Import..."}, {idExport, "Export..."}, {idDefaults, "Reset to defaults"}, {idPreview, "Preview alerts"}}
	bx := margin
	for _, b := range left {
		f.child("BUTTON", b.label, wsTabStop|bsPushButton, 0, bx, by, btnW, btnH, b.id)
		bx += btnW + f.s(8)
	}
	right := 2*colW + gap + margin
	f.child("BUTTON", "Close", wsTabStop|bsPushButton, 0, right-btnW, by, btnW, btnH, idClose)
	save := f.child("BUTTON", "Save to this PC", wsTabStop|bsDefPushButton, 0, right-2*btnW-f.s(8), by, btnW, btnH, idSave)
	if !winapi.IsElevated() {
		call(pSendMessageW, save, bcmSetShield, 0, 1) // UAC shield icon: saving prompts for admin
	}
}

// load puts c into the controls.
func (f *form) load(c config.Config, status string) {
	f.loading = true
	for name, ctl := range f.ctrls {
		v := c.Get(name)
		if f.fields[name].Type == reflect.Bool {
			chk := uintptr(0)
			if v == "1" {
				chk = 1
			}
			call(pSendMessageW, ctl, bmSetCheck, chk, 0)
			continue
		}
		if f.fields[name].Kind == "multiline" {
			v = strings.ReplaceAll(strings.ReplaceAll(v, "\r\n", "\n"), "\n", "\r\n")
		}
		setText(ctl, v)
	}
	f.loading = false
	f.setDirty(false)
	setText(f.status, status)
}

// read builds a Config from the controls (not yet validated).
func (f *form) read() (config.Config, error) {
	c := config.Defaults()
	var errs []error
	for name, ctl := range f.ctrls {
		var v string
		if f.fields[name].Type == reflect.Bool {
			v = "0"
			if call(pSendMessageW, ctl, bmGetCheck, 0, 0) == 1 {
				v = "1"
			}
		} else {
			v = getText(ctl)
		}
		if err := c.Set(name, v); err != nil {
			errs = append(errs, fmt.Errorf("%s: please enter a whole number", f.fields[name].Label))
		}
	}
	return c, errors.Join(errs...)
}

// readValid reads and validates, explaining problems to the user.
func (f *form) readValid() (config.Config, bool) {
	c, err := f.read()
	if err == nil {
		err = c.Validate()
	}
	if err != nil {
		winapi.Warn(windowTitle, "Please correct these settings:\n\n"+err.Error())
		return c, false
	}
	return c, true
}

func (f *form) setDirty(d bool) {
	f.dirty = d
	t := windowTitle
	if d {
		t += " *"
	}
	setText(f.hwnd, t)
}

func (f *form) command(id int, code uint16, ctl uintptr) {
	if name, ok := f.byHwnd[ctl]; ok {
		switch code {
		case enSetFocus, bnSetFocus:
			setText(f.help, f.fields[name].Label+": "+f.fields[name].Help)
		case enChange, bnClicked:
			if !f.loading {
				f.setDirty(true)
			}
		}
		return
	}
	switch id {
	case idImport:
		path, ok := fileDialog(f.hwnd, false, "Import settings", "")
		if !ok {
			return
		}
		c, _, err := config.ParseFile(path, config.Defaults())
		if err != nil {
			winapi.Error(windowTitle, "Could not read "+path+":\n\n"+err.Error())
			return
		}
		f.load(c, "Imported "+filepath.Base(path)+" - not saved to this PC yet.")
		f.setDirty(true)
		if err := c.Validate(); err != nil {
			winapi.Warn(windowTitle, "The file was loaded, but some settings need correcting:\n\n"+err.Error())
		}
	case idExport:
		c, ok := f.readValid()
		if !ok {
			return
		}
		path, ok := fileDialog(f.hwnd, true, "Export settings", "OneDriveHelper-settings.json")
		if !ok {
			return
		}
		if err := config.WriteFile(c, path); err != nil {
			winapi.Error(windowTitle, "Could not write "+path+":\n\n"+err.Error())
			return
		}
		setText(f.status, "Exported to "+path)
		winapi.Info(windowTitle, "Settings exported to:\n"+path+"\n\nDeploy with:\nOneDriveHelper.exe install --config \""+path+"\"")
	case idDefaults:
		if winapi.Confirm(windowTitle, "Replace all values in this window with the built-in defaults?\n\n(Nothing is saved until you click Save or Export.)") {
			f.load(config.Defaults(), "Defaults loaded - not saved to this PC yet.")
			f.setDirty(true)
		}
	case idPreview:
		c, ok := f.readValid()
		if !ok {
			return
		}
		if f.preview == nil {
			p, err := popup.Start()
			if err != nil {
				winapi.Error(windowTitle, "Preview unavailable: "+err.Error())
				return
			}
			f.preview = p
		}
		agent.ShowPreview(f.preview, c, nil)
	case idSave:
		f.save()
	case idClose:
		f.close()
	}
}

func (f *form) save() {
	c, ok := f.readValid()
	if !ok {
		return
	}
	if winapi.IsElevated() {
		if err := config.Save(c); err != nil {
			winapi.Error(windowTitle, "Could not save the settings:\n\n"+err.Error())
			return
		}
	} else {
		// Hand the settings to an elevated copy of this tool (UAC prompt).
		tmp, err := os.CreateTemp("", "OneDriveHelper-settings-*.json")
		if err != nil {
			winapi.Error(windowTitle, err.Error())
			return
		}
		tmp.Close()
		defer os.Remove(tmp.Name())
		if err := config.WriteFile(c, tmp.Name()); err != nil {
			winapi.Error(windowTitle, err.Error())
			return
		}
		exe, _ := os.Executable()
		deadline := time.Now().Add(15 * time.Minute)
		code, err, returned := winapi.RunElevatedWithDeadline(exe,
			[]string{"--import", tmp.Name(), "--deadline", strconv.FormatInt(deadline.Unix(), 10)}, deadline)
		switch {
		case errors.Is(err, winapi.ErrTimedOut):
			winapi.Warn(windowTitle, "Nobody approved saving the settings within 15 minutes, so nothing was saved.")
			<-returned // the temp file must stay until the stale prompt is gone
			return
		case errors.Is(err, winapi.ErrCancelled):
			return
		case err != nil:
			winapi.Error(windowTitle, "Could not save the settings: "+err.Error())
			return
		case code != 0:
			winapi.Error(windowTitle, fmt.Sprintf("Saving the settings failed (exit code %d).", code))
			return
		}
	}
	f.setDirty(false)
	setText(f.status, "Saved to this PC. Running agents pick up the changes within a minute.")
}

func (f *form) close() {
	if f.dirty && !winapi.Confirm(windowTitle, "You have changes that have not been saved or exported. Close anyway?") {
		return
	}
	call(pDestroyWindow, f.hwnd)
}

func wndProc(hwnd, message, wParam, lParam uintptr) uintptr {
	f := current
	if f == nil || f.hwnd == 0 {
		return call(pDefWindowProcW, hwnd, message, wParam, lParam)
	}
	switch message {
	case wmCommand:
		f.command(int(wParam&0xFFFF), uint16(wParam>>16), lParam)
		return 0
	case wmClose:
		f.close()
		return 0
	case wmDestroy:
		call(pPostQuitMessage, 0)
		return 0
	}
	return call(pDefWindowProcW, hwnd, message, wParam, lParam)
}
