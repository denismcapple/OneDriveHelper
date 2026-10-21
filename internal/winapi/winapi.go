// Package winapi contains small Win32 helpers not provided by golang.org/x/sys/windows.
package winapi

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32            = windows.NewLazySystemDLL("kernel32.dll")
	shell32             = windows.NewLazySystemDLL("shell32.dll")
	procGetTickCount64  = kernel32.NewProc("GetTickCount64")
	procShellExecuteExW = shell32.NewProc("ShellExecuteExW")
)

const (
	MB_OK              = 0x0
	MB_OKCANCEL        = 0x1
	MB_ICONERROR       = 0x10
	MB_ICONWARNING     = 0x30
	MB_ICONINFORMATION = 0x40
	MB_DEFBUTTON2      = 0x100
	MB_SETFOREGROUND   = 0x10000
	MB_TOPMOST         = 0x40000
	IDOK               = 1

	seeMaskNoCloseProcess = 0x40
	seeMaskNoAsync        = 0x100
	seeMaskFlagNoUI       = 0x400
)

var (
	procCreateActCtxW  = kernel32.NewProc("CreateActCtxW")
	procAttachConsole  = kernel32.NewProc("AttachConsole")
	procGetConsoleWnd  = kernel32.NewProc("GetConsoleWindow")
	procInitCommonCtls = windows.NewLazySystemDLL("comctl32.dll").NewProc("InitCommonControls")
)

type actCtx struct {
	cbSize                 uint32
	dwFlags                uint32
	lpSource               *uint16
	wProcessorArchitecture uint16
	wLangId                uint16
	lpAssemblyDirectory    *uint16
	lpResourceName         uintptr
	lpApplicationName      *uint16
	hModule                uintptr
}

// EnableVisualStyles makes standard controls and message boxes use the
// modern (Common Controls v6) look without an embedded manifest, by
// activating the manifest that ships inside shell32.dll as the process default.
// Call once at start-up, before creating any window.
func EnableVisualStyles() {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return
	}
	src, _ := windows.UTF16PtrFromString(sys + `\shell32.dll`)
	dir, _ := windows.UTF16PtrFromString(sys)
	ctx := actCtx{
		dwFlags:             0x4 | 0x8 | 0x10, // ASSEMBLY_DIRECTORY_VALID | RESOURCE_NAME_VALID | SET_PROCESS_DEFAULT
		lpSource:            src,
		lpAssemblyDirectory: dir,
		lpResourceName:      124,
	}
	ctx.cbSize = uint32(unsafe.Sizeof(ctx))
	procCreateActCtxW.Call(uintptr(unsafe.Pointer(&ctx)))
	procInitCommonCtls.Call()
}

// AttachParentConsole connects a GUI-subsystem program's stdout/stderr to
// the console it was started from (if any) so CLI options can print output.
func AttachParentConsole() bool {
	if h, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE); err == nil && h != 0 && h != windows.InvalidHandle {
		if t, _ := windows.GetFileType(h); t != windows.FILE_TYPE_UNKNOWN {
			return true // output is redirected to a file or pipe; keep it
		}
	}
	if w, _, _ := procGetConsoleWnd.Call(); w != 0 {
		return true
	}
	if r, _, _ := procAttachConsole.Call(uintptr(^uint32(0))); r == 0 { // ATTACH_PARENT_PROCESS
		return false
	}
	name, _ := windows.UTF16PtrFromString("CONOUT$")
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return false
	}
	f := os.NewFile(uintptr(h), "CONOUT$")
	os.Stdout, os.Stderr = f, f
	fmt.Fprintln(f) // the shell has already printed its prompt
	return true
}

var procAssocQueryStringW = windows.NewLazySystemDLL("shlwapi.dll").NewProc("AssocQueryStringW")

// HasFileHandler reports whether a program is registered to open files with
// the given extension (e.g. ".eml").
func HasFileHandler(ext string) bool {
	e, _ := windows.UTF16PtrFromString(ext)
	verb, _ := windows.UTF16PtrFromString("open")
	buf := make([]uint16, windows.MAX_PATH)
	n := uint32(len(buf))
	// ASSOCF_NOTRUNCATE=0x20, ASSOCSTR_EXECUTABLE=2
	hr, _, _ := procAssocQueryStringW.Call(0x20, 2, uintptr(unsafe.Pointer(e)), uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)))
	return int32(hr) >= 0 && windows.UTF16ToString(buf) != ""
}

// ErrCancelled is returned when the user declines the UAC prompt.
var ErrCancelled = errors.New("the operation was cancelled by the user")

// BootTime returns the (approximate) time the system was last started.
func BootTime() time.Time {
	ms, _, _ := procGetTickCount64.Call()
	return time.Now().Add(-time.Duration(ms) * time.Millisecond)
}

func box(title, text string, flags uint32) int32 {
	t, _ := windows.UTF16PtrFromString(text)
	c, _ := windows.UTF16PtrFromString(title)
	r, _ := windows.MessageBox(0, t, c, flags|MB_SETFOREGROUND|MB_TOPMOST)
	return r
}

func Info(title, text string)  { box(title, text, MB_OK|MB_ICONINFORMATION) }
func Warn(title, text string)  { box(title, text, MB_OK|MB_ICONWARNING) }
func Error(title, text string) { box(title, text, MB_OK|MB_ICONERROR) }

// Confirm shows an OK/Cancel warning dialog with Cancel as the default button.
func Confirm(title, text string) bool {
	return box(title, text, MB_OKCANCEL|MB_ICONWARNING|MB_DEFBUTTON2) == IDOK
}

// ShellOpen opens a URL, protocol link or file with its registered handler.
func ShellOpen(target string) error {
	verb, _ := windows.UTF16PtrFromString("open")
	file, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, verb, file, nil, nil, windows.SW_SHOWNORMAL)
}

// OpenFolder opens an existing folder in Explorer. Anything that isn't a
// plain folder (a file, a missing path, a link) is refused, so a path can
// never start a program.
func OpenFolder(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a folder", path)
	}
	return ShellOpen(path)
}

type shellExecuteInfo struct {
	cbSize       uint32
	fMask        uint32
	hwnd         windows.HWND
	lpVerb       *uint16
	lpFile       *uint16
	lpParameters *uint16
	lpDirectory  *uint16
	nShow        int32
	hInstApp     windows.Handle
	lpIDList     uintptr
	lpClass      *uint16
	hkeyClass    windows.Handle
	dwHotKey     uint32
	hIcon        windows.Handle
	hProcess     windows.Handle
}

// RunElevatedAndWait starts exe with the "runas" verb (UAC prompt), waits for it
// to exit and returns its exit code.
func RunElevatedAndWait(exe string, args []string) (uint32, error) {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = windows.EscapeArg(a)
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(exe)
	params, _ := windows.UTF16PtrFromString(strings.Join(quoted, " "))
	sei := shellExecuteInfo{
		fMask:        seeMaskNoCloseProcess | seeMaskNoAsync,
		lpVerb:       verb,
		lpFile:       file,
		lpParameters: params,
		nShow:        windows.SW_SHOWNORMAL,
	}
	sei.cbSize = uint32(unsafe.Sizeof(sei))
	if r, _, err := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&sei))); r == 0 {
		if errors.Is(err, windows.ERROR_CANCELLED) {
			return 0, ErrCancelled
		}
		return 0, fmt.Errorf("ShellExecuteEx: %w", err)
	}
	if sei.hProcess == 0 {
		return 0, errors.New("elevated process handle not returned")
	}
	defer windows.CloseHandle(sei.hProcess)
	if _, err := windows.WaitForSingleObject(sei.hProcess, windows.INFINITE); err != nil {
		return 0, err
	}
	var code uint32
	err := windows.GetExitCodeProcess(sei.hProcess, &code)
	return code, err
}

// ErrTimedOut is returned when an elevated step was not answered in time.
var ErrTimedOut = errors.New("nobody answered the administrator approval in time")

// RunElevatedWithDeadline is RunElevatedAndWait with a deadline. While the
// UAC prompt is up ShellExecuteEx blocks, and a standard-user process cannot
// reliably end the elevated one, so the call runs on its own goroutine. If it
// has not returned by the deadline the result is ErrTimedOut; whatever the
// call returns later is discarded. The returned channel closes when the call
// has really returned (callers wait on it before allowing another attempt).
func RunElevatedWithDeadline(exe string, args []string, deadline time.Time) (uint32, error, <-chan struct{}) {
	return WithDeadline(func() (uint32, error) { return RunElevatedAndWait(exe, args) }, deadline)
}

// WithDeadline runs fn and returns its result, or ErrTimedOut at deadline.
func WithDeadline(fn func() (uint32, error), deadline time.Time) (uint32, error, <-chan struct{}) {
	type result struct {
		code uint32
		err  error
	}
	ch, done := make(chan result, 1), make(chan struct{})
	go func() {
		defer close(done)
		c, err := fn()
		ch <- result{c, err}
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.code, r.err, done
	case <-timer.C:
		return 0, ErrTimedOut, done
	}
}

var procMessageBoxTimeoutW = windows.NewLazySystemDLL("user32.dll").NewProc("MessageBoxTimeoutW")

const mbTimedOut = 32000

// ConfirmWithin is Confirm, but the dialog closes itself after limit
// (counting as "not confirmed"). timedOut reports that it did.
func ConfirmWithin(title, text string, limit time.Duration) (ok, timedOut bool) {
	if limit <= 0 {
		return false, true
	}
	t, _ := windows.UTF16PtrFromString(text)
	c, _ := windows.UTF16PtrFromString(title)
	ms := uint32(limit / time.Millisecond)
	r, _, _ := procMessageBoxTimeoutW.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)),
		uintptr(MB_OKCANCEL|MB_ICONWARNING|MB_DEFBUTTON2|MB_SETFOREGROUND|MB_TOPMOST), 0, uintptr(ms))
	switch int32(r) {
	case mbTimedOut:
		return false, true
	case IDOK:
		return true, false
	}
	return false, false
}

// IsElevated reports whether the current process token is elevated.
func IsElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// CurrentUserSID returns the SID string of the user running this process.
func CurrentUserSID() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
}

// CurrentUserName returns DOMAIN\user for the account running this process.
func CurrentUserName() string {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "unknown"
	}
	if acct, dom, _, err := u.User.Sid.LookupAccount(""); err == nil {
		return dom + `\` + acct
	}
	return u.User.Sid.String()
}

// CurrentSessionID returns the Terminal Services session of this process.
func CurrentSessionID() uint32 {
	var s uint32
	_ = windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &s)
	return s
}

// LongPath converts an absolute path to the \\?\ form so Win32 calls are not
// limited to MAX_PATH.
func LongPath(p string) string {
	if strings.HasPrefix(p, `\\?\`) || len(p) < 3 {
		return p
	}
	if strings.HasPrefix(p, `\\`) {
		return `\\?\UNC\` + p[2:]
	}
	return `\\?\` + p
}
