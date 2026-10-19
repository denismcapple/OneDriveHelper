// Package ipc holds the names of the kernel objects shared between the
// service, the installer and the per-user agents, plus the "fix in progress"
// lock. It depends only on x/sys so the LocalSystem service doesn't pull in
// the agent's UI or remediation code.
package ipc

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// AgentMutex is held by the running agent of a session, in that session's
// own namespace, so a user in another session cannot squat it.
const AgentMutex = `OneDriveHelperAgent`

// AgentMutexLocal is the name the agent creates.
func AgentMutexLocal() string { return `Local\` + AgentMutex }

// AgentMutexForSession is the name the service (in session 0) opens to see
// whether a session's agent is alive.
func AgentMutexForSession(session uint32) string {
	return fmt.Sprintf(`Session\%d\%s`, session, AgentMutex)
}

// StopEventName is the per-service-instance event the service signals when
// it stops, telling agents to exit.
func StopEventName(pid int, nanos int64) string {
	return fmt.Sprintf(`Global\OneDriveHelperStop-%d-%d`, pid, nanos)
}

// FixLockName is held by an agent while a fix is moving, copying or deleting
// files. It is a signal, not mutual exclusion: the installer waits while it
// exists, and several agents (sessions) may hold it at once.
const FixLockName = `Global\OneDriveHelperFixing`

// fixLockSDDL lets SYSTEM, Administrators and any signed-in user open the
// lock for SYNCHRONIZE, so a second user's agent can hold the same object.
const fixLockSDDL = "D:(A;;GA;;;SY)(A;;GA;;;BA)(A;;0x00100000;;;AU)"

// HoldFixLock opens (or creates) the fix lock and returns a function that
// releases this holder's handle. The kernel also drops it if the process dies.
func HoldFixLock() (release func(), err error) {
	return holdNamed(FixLockName)
}

func holdNamed(name string) (func(), error) {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		if h, err := windows.OpenMutex(windows.SYNCHRONIZE, false, n); err == nil {
			return func() { windows.CloseHandle(h) }, nil
		} else if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return nil, err
		}
		sd, err := windows.SecurityDescriptorFromString(fixLockSDDL)
		if err != nil {
			return nil, err
		}
		sa := windows.SecurityAttributes{SecurityDescriptor: sd}
		sa.Length = uint32(unsafe.Sizeof(sa))
		h, err := windows.CreateMutex(&sa, false, n)
		if err == nil {
			return func() { windows.CloseHandle(h) }, nil
		}
		if h != 0 {
			windows.CloseHandle(h)
		}
		if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil, err
		}
		// Someone created it between our open and create: open it again.
	}
	return nil, errors.New("could not open or create " + name)
}

// FixInProgress reports whether any agent holds the fix lock.
func FixInProgress() bool { return exists(FixLockName) }

func exists(name string) bool {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return false
	}
	h, err := windows.OpenMutex(windows.SYNCHRONIZE, false, n)
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED) // exists, but not ours to open
	}
	windows.CloseHandle(h)
	return true
}

// FixLockVersion is the first release whose agents create the fix lock.
// The installer waits longer for agents of older versions (they finish a
// running fix before exiting, but don't announce it).
const FixLockVersion = "0.8.0"
