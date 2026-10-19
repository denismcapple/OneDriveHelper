package winapi

import (
	"sort"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	rstrtmgr                = windows.NewLazySystemDLL("rstrtmgr.dll")
	procRmStartSession      = rstrtmgr.NewProc("RmStartSession")
	procRmRegisterResources = rstrtmgr.NewProc("RmRegisterResources")
	procRmGetList           = rstrtmgr.NewProc("RmGetList")
	procRmEndSession        = rstrtmgr.NewProc("RmEndSession")
)

type rmUniqueProcess struct {
	PID       uint32
	StartTime windows.Filetime
}

type rmProcessInfo struct {
	Process          rmUniqueProcess
	AppName          [256]uint16
	ServiceShortName [64]uint16
	AppType          uint32
	AppStatus        uint32
	TSSessionID      uint32
	Restartable      int32
}

// ProgramsUsing returns the names of programs that have any of the given
// files open (e.g. "Microsoft Excel"), using the Windows Restart Manager.
// It only asks; nothing is closed or restarted. Errors give an empty list.
func ProgramsUsing(files []string) []string {
	if len(files) == 0 {
		return nil
	}
	var session uint32
	key := make([]uint16, 64) // CCH_RM_SESSION_KEY + 1
	if r, _, _ := procRmStartSession.Call(uintptr(unsafe.Pointer(&session)), 0, uintptr(unsafe.Pointer(&key[0]))); r != 0 {
		return nil
	}
	defer procRmEndSession.Call(uintptr(session))
	ptrs := make([]*uint16, 0, len(files))
	for _, f := range files {
		if p, err := windows.UTF16PtrFromString(f); err == nil {
			ptrs = append(ptrs, p)
		}
	}
	if len(ptrs) == 0 {
		return nil
	}
	if r, _, _ := procRmRegisterResources.Call(uintptr(session), uintptr(len(ptrs)), uintptr(unsafe.Pointer(&ptrs[0])), 0, 0, 0, 0); r != 0 {
		return nil
	}
	var needed, reasons uint32
	n := uint32(64)
	infos := make([]rmProcessInfo, n)
	if r, _, _ := procRmGetList.Call(uintptr(session), uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&n)),
		uintptr(unsafe.Pointer(&infos[0])), uintptr(unsafe.Pointer(&reasons))); r != 0 && r != 234 /*ERROR_MORE_DATA*/ {
		return nil
	}
	if n > uint32(len(infos)) {
		n = uint32(len(infos))
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range infos[:n] {
		name := windows.UTF16ToString(p.AppName[:])
		if name == "" {
			name = windows.UTF16ToString(p.ServiceShortName[:])
		}
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
