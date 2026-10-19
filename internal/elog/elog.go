// Package elog writes to the Windows Application event log under the
// OneDriveHelper source so RMM tools can alert on it.
package elog

import (
	"strings"

	"golang.org/x/sys/windows/svc/eventlog"

	"github.com/denismcapple/OneDriveHelper/internal/version"
)

// Event IDs.
const (
	ServiceStarted = 1000
	ServiceStopped = 1001
	// InstallerWaitingForFix: install/upgrade/uninstall is waiting for a fix in progress.
	InstallerWaitingForFix = 1002
	// InstallerInterruptedFix: the installer stopped an agent while a fix was in progress.
	InstallerInterruptedFix = 1003
	AgentLaunched           = 1100
	AgentLaunchFailed       = 1101
	AgentStarted            = 2000
	OneDriveNotRunning      = 3001
	OneDriveRelaunched      = 3002
	OneDriveAlertSnoozed    = 3003
	OneDriveRecovered       = 3004
	// OneDriveAutoRestarted: OneDrive had stopped and an automatic restart worked.
	OneDriveAutoRestarted = 3005
	// OneDriveAutoRestartFailed: automatic restarts didn't work; the user was warned.
	OneDriveAutoRestartFailed = 3006
	// OneDriveStateUnknown: the process list could not be read 5 checks in a row.
	OneDriveStateUnknown = 3007
	OrphanedLibraries    = 4001
	OrphansResolved      = 4002
	// RemediationStarted is the approval step's event (an admin approved a fix).
	RemediationStarted = 5001
	// RemediationCompleted: every folder of a fix was removed after backing it up.
	RemediationCompleted = 5002
	// RemediationFailed: a copy or verification failed, or a delete stopped part-way.
	RemediationFailed    = 5003
	RemediationCancelled = 5004
	ResyncRequested      = 5005
	// FixSkippedOpen: a folder was skipped because something has it open.
	FixSkippedOpen = 5006
	// FixInterrupted: an interrupted fix was found (Information: restored; Warning: needs attention).
	FixInterrupted = 5007
	// FixReregistered: OneDrive is syncing the original location again; the local copy was kept.
	FixReregistered = 5008
	// FixRefusedLink: a junction or link in the path; the folder was not touched.
	FixRefusedLink = 5009
	// ApprovalTimedOut: nobody answered the approval in time; nothing changed.
	ApprovalTimedOut = 5010
	// FixStarted: the file work of an approved fix started.
	FixStarted = 5011
	// FixKept: a folder was left in the staging area; the message names it.
	FixKept = 5012
	// FixRefusedData: OneDrive data unreadable, OneDrive starting up, or process state unknown.
	FixRefusedData = 5013
)

// Log is a best-effort event log writer; a nil or failed log silently drops events.
type Log struct{ l *eventlog.Log }

func Open() *Log {
	l, err := eventlog.Open(version.EventSource)
	if err != nil {
		return &Log{}
	}
	return &Log{l: l}
}

func (e *Log) Info(id uint32, msg string) {
	if e != nil && e.l != nil {
		_ = e.l.Info(id, msg)
	}
}

func (e *Log) Warning(id uint32, msg string) {
	if e != nil && e.l != nil {
		_ = e.l.Warning(id, msg)
	}
}

func (e *Log) Error(id uint32, msg string) {
	if e != nil && e.l != nil {
		_ = e.l.Error(id, msg)
	}
}

func (e *Log) Close() {
	if e != nil && e.l != nil {
		_ = e.l.Close()
	}
}

// Install registers the event source (requires admin).
func Install() error {
	err := eventlog.InstallAsEventCreate(version.EventSource, eventlog.Error|eventlog.Warning|eventlog.Info)
	if err != nil && strings.Contains(err.Error(), "exists") {
		return nil
	}
	return err
}

func Remove() error { return eventlog.Remove(version.EventSource) }
