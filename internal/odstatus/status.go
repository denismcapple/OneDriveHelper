// Package odstatus reads OneDrive's own status for each of a user's sync
// roots, the way File Explorer shows it (Windows 11 / Server 2025: the
// storage provider status UI that OneDrive registers per sync root), and
// can run OneDrive's own command for it (e.g. "Sign in", "View sync issues").
//
// The calls are relayed to OneDrive.exe and block while OneDrive hangs, so
// they run on their own thread with a time limit.
package odstatus

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// State is Windows.Storage.Provider.StorageProviderState.
type State int

const (
	InSync State = iota
	Syncing
	Paused
	Error
	Warning
	Offline
)

func (s State) String() string {
	if s >= InSync && s <= Offline {
		return [...]string{"in sync", "syncing", "paused", "error", "warning", "offline"}[s]
	}
	return "unknown"
}

// Root is the status OneDrive reports for one sync root.
type Root struct {
	ID      string // SyncRootManager ID, "OneDrive!<SID>!Business1|..."
	Name    string // e.g. "OneDrive - Contoso"
	State   State
	Status  string // OneDrive's status text, e.g. "Backed up and synced"
	Message string // OneDrive's "more info" text, e.g. "You have 3 sync issues"
	Action  string // label of OneDrive's button for it, e.g. "View sync issues" ("" = none)
	Err     string // why this root's status couldn't be read
}

// Account is the account part of the ID ("Business1", "Personal").
func (r Root) Account() string {
	parts := strings.Split(r.ID, "!")
	if len(parts) < 3 {
		return ""
	}
	a, _, _ := strings.Cut(parts[2], "|")
	return a
}

// ErrUnsupported means Windows or OneDrive offers no status here (Windows 10,
// Server 2019/2022, or an old OneDrive).
var ErrUnsupported = errors.New("OneDrive status is not available on this version of Windows or OneDrive")

// ErrTimeout means OneDrive did not answer in time: it is hung.
var ErrTimeout = errors.New("OneDrive did not answer")

const syncRootPath = `SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\SyncRootManager`

// busy is held while a call into OneDrive is outstanding. A call that never
// returns keeps it, so a hung OneDrive costs one thread, not one per check.
var busy = make(chan struct{}, 1)

// Read returns the status of every OneDrive sync root of the user sid.
func Read(sid string, timeout time.Duration) ([]Root, error) {
	roots, err := statusRoots(sid)
	if err != nil || len(roots) == 0 {
		return nil, err
	}
	var out []Root
	err = call(timeout, func() {
		for _, r := range roots {
			out = append(out, readRoot(r))
		}
	})
	return out, err
}

// Invoke runs OneDrive's own command for a sync root (its "more info" button,
// else the status command), as if the user had clicked it in Explorer. It
// returns false if OneDrive offers no command.
func Invoke(sid, id string, timeout time.Duration) (bool, error) {
	roots, err := statusRoots(sid)
	if err != nil {
		return false, err
	}
	for _, r := range roots {
		if !strings.EqualFold(r.id, id) {
			continue
		}
		ran := false
		var ierr error
		err := call(timeout, func() { ran, ierr = invokeRoot(r) })
		if err == nil {
			err = ierr
		}
		return ran, err
	}
	return false, nil
}

type statusRoot struct {
	id, name string
	factory  windows.GUID
}

// statusRoots lists sid's OneDrive sync roots that registered a status source.
func statusRoots(sid string) ([]statusRoot, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, syncRootPath, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil, ErrUnsupported
	}
	defer k.Close()
	names, _ := k.ReadSubKeyNames(-1)
	prefix := strings.ToLower("OneDrive!" + sid + "!")
	var out []statusRoot
	for _, n := range names {
		if !strings.HasPrefix(strings.ToLower(n), prefix) {
			continue
		}
		sk, err := registry.OpenKey(k, n, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		clsid, _, _ := sk.GetStringValue("StorageProviderStatusUISourceFactory")
		name, _, _ := sk.GetStringValue("DisplayNameResource")
		sk.Close()
		g, err := windows.GUIDFromString(clsid)
		if err != nil {
			continue
		}
		out = append(out, statusRoot{id: n, name: name, factory: g})
	}
	if len(out) == 0 && len(names) > 0 && anyOwn(names, prefix) {
		return nil, ErrUnsupported // sync roots, but no status source: Windows 10 or an old OneDrive
	}
	return out, nil
}

func anyOwn(names []string, prefix string) bool {
	for _, n := range names {
		if strings.HasPrefix(strings.ToLower(n), prefix) {
			return true
		}
	}
	return false
}

// call runs f on a COM thread of its own and waits at most timeout.
func call(timeout time.Duration, f func()) error {
	select {
	case busy <- struct{}{}:
	default:
		return ErrTimeout // an earlier call is still stuck in OneDrive
	}
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // the thread is discarded with its apartment if the call never returns
		defer func() { <-busy }()
		if err := windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED); err != nil && !errors.Is(err, syscall.Errno(1)) { // 1 = S_FALSE: already initialised
			done <- err
			return
		}
		defer windows.CoUninitialize()
		defer runtime.UnlockOSThread()
		f()
		done <- nil
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return ErrTimeout
	}
}

// Interface IDs and vtable slots, from Windows.Storage.winmd. Slots 0-5 are
// IUnknown and IInspectable.
var (
	iidFactory = windows.GUID{Data1: 0x12e46b74, Data2: 0x4e5a, Data3: 0x58d1, Data4: [8]byte{0xa6, 0x2f, 0x03, 0x76, 0xe8, 0xee, 0x7d, 0xd8}}
)

const (
	slotRelease        = 2
	slotGetSource      = 6  // IStorageProviderStatusUISourceFactory.GetStatusUISource(HSTRING)
	slotGetStatusUI    = 6  // IStorageProviderStatusUISource.GetStatusUI
	slotState          = 6  // IStorageProviderStatusUI.get_ProviderState
	slotSyncCommand    = 12 // .get_SyncStatusCommand
	slotMoreInfo       = 16 // .get_MoreInfoUI
	slotCmdLabel       = 6  // IStorageProviderUICommand.get_Label
	slotCmdState       = 9  // .get_State (0 enabled, 1 disabled, 2 hidden)
	slotCmdInvoke      = 10 // .Invoke
	slotMoreInfoText   = 6  // IStorageProviderMoreInfoUI.get_Message
	slotMoreInfoCmd    = 8  // .get_Command
	clsctxLocalOrInner = 0x1 | 0x4
)

var (
	ole32                       = windows.NewLazySystemDLL("ole32.dll")
	procCoCreateInstance        = ole32.NewProc("CoCreateInstance")
	combase                     = windows.NewLazySystemDLL("combase.dll")
	procWindowsCreateString     = combase.NewProc("WindowsCreateString")
	procWindowsDeleteString     = combase.NewProc("WindowsDeleteString")
	procWindowsGetStringRawBuff = combase.NewProc("WindowsGetStringRawBuffer")
	procLstrcpynW               = windows.NewLazySystemDLL("kernel32.dll").NewProc("lstrcpynW")
)

// iface is a COM interface pointer: its first word points at the vtable.
type iface struct{ vtbl *[32]uintptr }

type comObj = *iface

func call0(o comObj, slot int, args ...uintptr) uintptr {
	r, _, _ := syscall.SyscallN(o.vtbl[slot], append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)...)
	return r
}

func (o *iface) method(slot int, args ...uintptr) uintptr { return call0(o, slot, args...) }

func (o *iface) release() {
	if o != nil {
		o.method(slotRelease)
	}
}

func hr(r uintptr) error {
	if int32(r) < 0 {
		return windows.Errno(r)
	}
	return nil
}

// obj calls a method that returns an interface pointer (nil if none).
func (o *iface) obj(slot int, args ...uintptr) (comObj, error) {
	var out comObj
	err := hr(o.method(slot, append(args, uintptr(unsafe.Pointer(&out)))...))
	return out, err
}

// str calls a method that returns an HSTRING.
func (o *iface) str(slot int) string {
	var h uintptr
	if o == nil || hr(o.method(slot, uintptr(unsafe.Pointer(&h)))) != nil || h == 0 {
		return ""
	}
	defer procWindowsDeleteString.Call(h)
	var n uint32
	p, _, _ := procWindowsGetStringRawBuff.Call(h, uintptr(unsafe.Pointer(&n)))
	if p == 0 || n == 0 {
		return ""
	}
	buf := make([]uint16, n+1) // lstrcpynW copies n characters and a terminating NUL
	procLstrcpynW.Call(uintptr(unsafe.Pointer(&buf[0])), p, uintptr(len(buf)))
	return windows.UTF16ToString(buf)
}

func (o *iface) u32(slot int) (uint32, error) {
	var v uint32
	err := hr(o.method(slot, uintptr(unsafe.Pointer(&v))))
	return v, err
}

func hstring(s string) (uintptr, error) {
	u, err := windows.UTF16FromString(s)
	if err != nil {
		return 0, err
	}
	var h uintptr
	r, _, _ := procWindowsCreateString.Call(uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1), uintptr(unsafe.Pointer(&h)))
	return h, hr(r)
}

// statusUI returns the IStorageProviderStatusUI of a sync root.
func statusUI(r statusRoot) (comObj, error) {
	if err := procCoCreateInstance.Find(); err != nil {
		return nil, ErrUnsupported
	}
	var factory comObj
	v, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&r.factory)), 0, clsctxLocalOrInner,
		uintptr(unsafe.Pointer(&iidFactory)), uintptr(unsafe.Pointer(&factory)))
	if err := hr(v); err != nil {
		return nil, err
	}
	defer factory.release()
	id, err := hstring(r.id)
	if err != nil {
		return nil, err
	}
	defer procWindowsDeleteString.Call(id)
	source, err := factory.obj(slotGetSource, id)
	if err != nil || source == nil {
		return nil, err
	}
	defer source.release()
	return source.obj(slotGetStatusUI)
}

func readRoot(r statusRoot) Root {
	out := Root{ID: r.id, Name: r.name, State: -1}
	ui, err := statusUI(r)
	if err != nil || ui == nil {
		out.Err = fmt.Sprint("no status: ", err)
		return out
	}
	defer ui.release()
	if s, err := ui.u32(slotState); err == nil {
		out.State = State(s)
	}
	if c, err := ui.obj(slotSyncCommand); err == nil && c != nil {
		out.Status = c.str(slotCmdLabel)
		c.release()
	}
	if mi, err := ui.obj(slotMoreInfo); err == nil && mi != nil {
		out.Message = mi.str(slotMoreInfoText)
		if c, err := mi.obj(slotMoreInfoCmd); err == nil && c != nil {
			if usable(c) {
				out.Action = c.str(slotCmdLabel)
			}
			c.release()
		}
		mi.release()
	}
	return out
}

func usable(c comObj) bool {
	s, err := c.u32(slotCmdState)
	return err == nil && s == 0
}

// invokeRoot runs the "more info" button, else the status command.
func invokeRoot(r statusRoot) (bool, error) {
	ui, err := statusUI(r)
	if err != nil || ui == nil {
		return false, err
	}
	defer ui.release()
	var cmds []comObj
	if mi, err := ui.obj(slotMoreInfo); err == nil && mi != nil {
		if c, err := mi.obj(slotMoreInfoCmd); err == nil && c != nil {
			cmds = append(cmds, c)
		}
		mi.release()
	}
	if c, err := ui.obj(slotSyncCommand); err == nil && c != nil {
		cmds = append(cmds, c)
	}
	defer func() {
		for _, c := range cmds {
			c.release()
		}
	}()
	for _, c := range cmds {
		if usable(c) {
			return true, hr(c.method(slotCmdInvoke))
		}
	}
	return false, nil
}
