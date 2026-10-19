// Package service implements the LocalSystem Windows service that keeps a
// user agent running in every interactive session.
package service

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/elog"
	"github.com/denismcapple/OneDriveHelper/internal/ipc"
	"github.com/denismcapple/OneDriveHelper/internal/logx"
	"github.com/denismcapple/OneDriveHelper/internal/version"
)

// Session change event types (WTS_*).
const (
	wtsConsoleConnect = 0x1
	wtsRemoteConnect  = 0x3
	wtsSessionLogon   = 0x5
	wtsSessionUnlock  = 0x8
)

type sessionState struct {
	lastLaunch time.Time
	failures   int // consecutive launches where the agent died quickly
}

type handler struct {
	log       *log.Logger
	ev        *elog.Log
	agentExe  string
	sessions  map[uint32]*sessionState
	stopName  string         // per-service-instance event name passed to agents
	stopEvent windows.Handle // signalled when the service stops so agents exit
}

// createStopEvent creates a uniquely named event that agents (any
// authenticated user) may wait on but not signal.
func createStopEvent() (string, windows.Handle, error) {
	name := ipc.StopEventName(os.Getpid(), time.Now().UnixNano())
	sd, err := windows.SecurityDescriptorFromString("D:(A;;GA;;;SY)(A;;GA;;;BA)(A;;0x00100000;;;AU)") // AU: SYNCHRONIZE only
	if err != nil {
		return "", 0, err
	}
	sa := windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(sa))
	n, _ := windows.UTF16PtrFromString(name)
	h, err := windows.CreateEvent(&sa, 1, 0, n)
	return name, h, err
}

// Run is called when the process is started by the Service Control Manager.
func Run() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	prepErr := logx.PrepareProgramDataDir()
	h := &handler{
		log:      logx.Open(filepath.Join(logx.ProgramDataDir(), "service.log"), false),
		ev:       elog.Open(),
		agentExe: filepath.Join(filepath.Dir(exe), version.AgentExe),
		sessions: map[uint32]*sessionState{},
	}
	defer h.ev.Close()
	if prepErr != nil {
		h.log.Printf("securing %s: %v", logx.ProgramDataDir(), prepErr)
	}
	if h.stopName, h.stopEvent, err = createStopEvent(); err != nil {
		h.log.Printf("create stop event: %v (agents will notice the service stopping by polling)", err)
		h.stopName = ""
	} else {
		defer windows.CloseHandle(h.stopEvent)
	}
	return svc.Run(version.ServiceName, h)
}

func (h *handler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	h.log.Printf("service %s starting; agent %s", version.Version, h.agentExe)
	h.ev.Info(elog.ServiceStarted, "OneDrive Helper service "+version.Version+" started.")
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown | svc.AcceptSessionChange}

	cfg := config.Load()
	tick := time.NewTimer(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			cfg = config.Load()
			h.reconcile()
			tick.Reset(cfg.Watchdog())
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				h.log.Printf("service stopping; telling agents to exit")
				if h.stopEvent != 0 {
					_ = windows.SetEvent(h.stopEvent)
				}
				h.ev.Info(elog.ServiceStopped, "OneDrive Helper service stopped.")
				return false, 0
			case svc.SessionChange:
				switch c.EventType {
				case wtsSessionLogon, wtsSessionUnlock, wtsConsoleConnect, wtsRemoteConnect:
					// Give the shell a moment, then make sure the agent is up.
					tick.Reset(5 * time.Second)
				}
			}
		}
	}
}

func (h *handler) reconcile() {
	defer func() {
		if r := recover(); r != nil {
			h.log.Printf("panic in reconcile: %v", r)
		}
	}()
	ids, err := activeUserSessions()
	if err != nil {
		h.log.Printf("enumerate sessions: %v", err)
		return
	}
	live := map[uint32]bool{}
	for _, id := range ids {
		live[id] = true
		st := h.sessions[id]
		if st == nil {
			st = &sessionState{}
			h.sessions[id] = st
		}
		if agentRunning(id) {
			if time.Since(st.lastLaunch) > 2*time.Minute {
				st.failures = 0
			}
			continue
		}
		// Back off if the agent keeps dying shortly after launch.
		if !st.lastLaunch.IsZero() && time.Since(st.lastLaunch) < 2*time.Minute {
			st.failures++
		}
		if st.failures >= 3 && time.Since(st.lastLaunch) < 10*time.Minute {
			continue
		}
		pid, err := launchAgent(id, h.agentExe, h.stopName)
		st.lastLaunch = time.Now()
		if err != nil {
			h.log.Printf("launch agent in session %d: %v", id, err)
			if st.failures == 0 {
				h.ev.Error(elog.AgentLaunchFailed, fmt.Sprintf("Could not start the OneDrive Helper agent in session %d: %v", id, err))
			}
			continue
		}
		h.log.Printf("launched agent pid %d in session %d", pid, id)
		h.ev.Info(elog.AgentLaunched, fmt.Sprintf("Started OneDrive Helper agent (pid %d) in session %d.", pid, id))
	}
	for id := range h.sessions {
		if !live[id] {
			delete(h.sessions, id)
		}
	}
}
