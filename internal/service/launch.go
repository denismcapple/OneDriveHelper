package service

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/denismcapple/OneDriveHelper/internal/agent"
)

// activeUserSessions returns the IDs of sessions with a logged-on user in the
// Active state (console or RDP).
func activeUserSessions() ([]uint32, error) {
	var p *windows.WTS_SESSION_INFO
	var n uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &p, &n); err != nil {
		return nil, err
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(p)))
	var out []uint32
	for _, s := range unsafe.Slice(p, n) {
		if s.SessionID != 0 && s.State == windows.WTSActive {
			out = append(out, s.SessionID)
		}
	}
	return out, nil
}

// agentRunning checks for the agent's per-session mutex.
func agentRunning(session uint32) bool {
	name, _ := windows.UTF16PtrFromString(agent.MutexName(session))
	h, err := windows.OpenMutex(windows.SYNCHRONIZE, false, name)
	if err != nil {
		return false
	}
	windows.CloseHandle(h)
	return true
}

// launchAgent starts the agent as the session's user on its interactive desktop.
func launchAgent(session uint32, exe, stopEvent string) (uint32, error) {
	var tok windows.Token
	if err := windows.WTSQueryUserToken(session, &tok); err != nil {
		return 0, fmt.Errorf("WTSQueryUserToken(%d): %w", session, err)
	}
	defer tok.Close()

	var env *uint16
	if err := windows.CreateEnvironmentBlock(&env, tok, false); err != nil {
		return 0, fmt.Errorf("CreateEnvironmentBlock: %w", err)
	}
	defer windows.DestroyEnvironmentBlock(env)

	line := windows.EscapeArg(exe) + " agent"
	if stopEvent != "" {
		line += " --stop-event " + windows.EscapeArg(stopEvent)
	}
	cmd, _ := windows.UTF16PtrFromString(line)
	app, _ := windows.UTF16PtrFromString(exe)
	desktop, _ := windows.UTF16PtrFromString(`winsta0\default`)
	si := windows.StartupInfo{Desktop: desktop}
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi windows.ProcessInformation
	err := windows.CreateProcessAsUser(tok, app, cmd, nil, nil, false,
		windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_NEW_PROCESS_GROUP, env, nil, &si, &pi)
	if err != nil {
		return 0, fmt.Errorf("CreateProcessAsUser: %w", err)
	}
	windows.CloseHandle(pi.Thread)
	windows.CloseHandle(pi.Process)
	return pi.ProcessId, nil
}
