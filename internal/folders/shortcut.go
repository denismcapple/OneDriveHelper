package folders

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Shell link COM objects (shobjidl.h / objidl.h).
var (
	clsidShellLink  = windows.GUID{Data1: 0x00021401, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidShellLinkW   = windows.GUID{Data1: 0x000214F9, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidPersistFile  = windows.GUID{Data1: 0x0000010B, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	procCoCreateIns = windows.NewLazySystemDLL("ole32.dll").NewProc("CoCreateInstance")
)

// vtable slots.
const (
	slotQueryInterface = 0
	slotRelease        = 2
	slotSetDescription = 7  // IShellLinkW
	slotSetPath        = 20 // IShellLinkW
	slotSave           = 6  // IPersistFile
)

type comIface struct{ vtbl *[32]uintptr }

func (o *comIface) call(slot int, args ...uintptr) error {
	r, _, _ := syscall.SyscallN(o.vtbl[slot], append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)...)
	if int32(r) < 0 {
		return windows.Errno(r)
	}
	return nil
}

// ShortcutFile is the file name for a shortcut called name: characters
// Windows doesn't allow in file names are left out.
func ShortcutFile(name string) string {
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`\/:*?"<>|`, r) || r < 32 {
			return -1
		}
		return r
	}, strings.TrimSpace(name))
	name = strings.TrimRight(name, ". ")
	if name == "" {
		return ""
	}
	return name + ".lnk"
}

// ExpandTarget fills in variables such as %USERNAME% for the current user.
func ExpandTarget(target string) string {
	if s, err := registry.ExpandString(strings.TrimSpace(target)); err == nil {
		return s
	}
	return strings.TrimSpace(target)
}

// CreateShortcut writes a shortcut at lnk that opens target (a folder,
// drive or file; it doesn't have to be reachable now, e.g. a drive mapped
// at sign-in). An existing shortcut of the same name is replaced.
func CreateShortcut(lnk, target, description string) error {
	if target == "" {
		return errors.New("no target")
	}
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); err != nil && !errors.Is(err, syscall.Errno(1)) { // 1 = S_FALSE
			done <- err
			return
		}
		defer windows.CoUninitialize()
		done <- createShortcut(lnk, target, description)
	}()
	return <-done
}

func createShortcut(lnk, target, description string) error {
	var link *comIface
	r, _, _ := procCoCreateIns.Call(uintptr(unsafe.Pointer(&clsidShellLink)), 0, 1, // CLSCTX_INPROC_SERVER
		uintptr(unsafe.Pointer(&iidShellLinkW)), uintptr(unsafe.Pointer(&link)))
	if int32(r) < 0 || link == nil {
		return fmt.Errorf("create shell link: %w", windows.Errno(r))
	}
	defer link.call(slotRelease)
	t, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	if err := link.call(slotSetPath, uintptr(unsafe.Pointer(t))); err != nil {
		return fmt.Errorf("shortcut target %s: %w", target, err)
	}
	if description != "" {
		if d, err := windows.UTF16PtrFromString(description); err == nil {
			_ = link.call(slotSetDescription, uintptr(unsafe.Pointer(d)))
		}
	}
	var file *comIface
	if err := link.call(slotQueryInterface, uintptr(unsafe.Pointer(&iidPersistFile)), uintptr(unsafe.Pointer(&file))); err != nil || file == nil {
		return fmt.Errorf("shell link file: %v", err)
	}
	defer file.call(slotRelease)
	p, err := windows.UTF16PtrFromString(lnk)
	if err != nil {
		return err
	}
	if err := file.call(slotSave, uintptr(unsafe.Pointer(p)), 1); err != nil {
		return fmt.Errorf("save shortcut %s: %w", lnk, err)
	}
	return nil
}
