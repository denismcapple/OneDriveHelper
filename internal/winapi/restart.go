package winapi

import (
	"errors"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// RestartBlockers lists why this PC must not be restarted by OneDrive Helper.
// It is empty only on a single-user physical-style workstation: not a server
// or multi-session host (RDS, AVD), not a Citrix, VMware Horizon or FSLogix
// environment, and with nobody else signed in.
func RestartBlockers() []string {
	var why []string
	if v := windows.RtlGetVersion(); v == nil || v.ProductType != 1 { // VER_NT_WORKSTATION
		why = append(why, "this is a server, not a workstation")
	}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE); err == nil {
		ed, _, _ := k.GetStringValue("EditionID")
		name, _, _ := k.GetStringValue("ProductName")
		k.Close()
		if strings.EqualFold(ed, "ServerRdsh") || strings.Contains(strings.ToLower(name), "multi-session") {
			why = append(why, "this is a multi-session (Remote Desktop / Azure Virtual Desktop) host")
		}
	}
	keyExists := func(path string) bool {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE)
		if err == nil {
			k.Close()
		}
		return err == nil
	}
	enabled := func(path string) bool {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE)
		if err != nil {
			return false
		}
		defer k.Close()
		v, _, err := k.GetIntegerValue("Enabled")
		return err == nil && v != 0
	}
	if keyExists(`SOFTWARE\Citrix\VirtualDesktopAgent`) || serviceExists("BrokerAgent") || serviceExists("picaSvc2") {
		why = append(why, "Citrix is installed")
	}
	if keyExists(`SOFTWARE\VMware, Inc.\VMware VDM\Agent`) {
		why = append(why, "this is a VMware Horizon virtual desktop")
	}
	if serviceExists("RDAgentBootLoader") {
		why = append(why, "this is an Azure Virtual Desktop session host")
	}
	for _, p := range []string{`SOFTWARE\FSLogix\Profiles`, `SOFTWARE\Policies\FSLogix\Profiles`, `SOFTWARE\FSLogix\ODFC`, `SOFTWARE\Policies\FSLogix\ODFC`} {
		if enabled(p) {
			why = append(why, "FSLogix profile containers are in use")
			break
		}
	}
	if n := otherSessions(); n > 0 {
		why = append(why, "someone else is signed in to this PC")
	}
	return why
}

func serviceExists(name string) bool {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return false
	}
	defer windows.CloseServiceHandle(scm)
	n, _ := windows.UTF16PtrFromString(name)
	s, err := windows.OpenService(scm, n, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST)
	}
	windows.CloseServiceHandle(s)
	return true
}

// otherSessions counts user sessions (active or disconnected) other than
// this process's own. An error counts as "someone else", to be safe.
func otherSessions() int {
	var p *windows.WTS_SESSION_INFO
	var n uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &p, &n); err != nil {
		return 1
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(p)))
	mine, others := CurrentSessionID(), 0
	for _, s := range unsafe.Slice(p, n) {
		if s.SessionID != 0 && s.SessionID != mine && (s.State == windows.WTSActive || s.State == windows.WTSDisconnected) {
			others++
		}
	}
	return others
}

// LogOff signs the current user out of this session. Programs are asked to
// close normally, so one with unsaved work can still hold it up.
func LogOff() error {
	// SHTDN_REASON_MAJOR_APPLICATION | SHTDN_REASON_MINOR_MAINTENANCE | SHTDN_REASON_FLAG_PLANNED
	return windows.ExitWindowsEx(windows.EWX_LOGOFF, 0x00040000|0x00000001|0x80000000)
}

// LogonID identifies this process's logon session. It is different after
// every sign-in (including after a restart), so it tells whether the user
// has signed out and back in since it was recorded.
func LogonID() (uint64, error) {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &tok); err != nil {
		return 0, err
	}
	defer tok.Close()
	// TOKEN_STATISTICS: TokenId (LUID), AuthenticationId (LUID), ... The
	// buffer must be 8-byte aligned, hence uint64s.
	var buf [16]uint64
	var n uint32
	if err := windows.GetTokenInformation(tok, windows.TokenStatistics, (*byte)(unsafe.Pointer(&buf[0])), uint32(unsafe.Sizeof(buf)), &n); err != nil {
		return 0, err
	}
	return buf[1], nil // AuthenticationId: LowPart then HighPart, i.e. one little-endian uint64
}

// Restart asks Windows to restart. Programs are asked to close normally, so
// one with unsaved work can still hold the restart up.
func Restart() error {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &tok); err != nil {
		return err
	}
	defer tok.Close()
	var luid windows.LUID
	name, _ := windows.UTF16PtrFromString("SeShutdownPrivilege")
	if err := windows.LookupPrivilegeValue(nil, name, &luid); err != nil {
		return err
	}
	tp := windows.Tokenprivileges{PrivilegeCount: 1}
	tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
	if err := windows.AdjustTokenPrivileges(tok, false, &tp, 0, nil, nil); err != nil {
		return err
	}
	// SHTDN_REASON_MAJOR_APPLICATION | SHTDN_REASON_MINOR_MAINTENANCE | SHTDN_REASON_FLAG_PLANNED
	return windows.ExitWindowsEx(windows.EWX_REBOOT, 0x00040000|0x00000001|0x80000000)
}
