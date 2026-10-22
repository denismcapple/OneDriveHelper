// Package elog writes to the Windows Application event log under the
// OneDriveHelper source so RMM tools can alert on it.
package elog

import (
	"errors"

	"golang.org/x/sys/windows/registry"
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
	// FixRestartRequested: folders were locked; the fix will finish after the next restart.
	FixRestartRequested = 5014
	// FixResumed: a fix held up by open files is being finished after a restart.
	FixResumed = 5015
	// FixSkippedCloud: a folder was skipped because Windows still treats it as
	// part of OneDrive (a sync registration that could not be removed).
	FixSkippedCloud = 5016
	// FixDeadSyncRoot: a folder's dead OneDrive sync registration was removed so it could be fixed.
	FixDeadSyncRoot = 5017
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

const sourceKey = `SYSTEM\CurrentControlSet\Services\EventLog\Application\` + version.EventSource

// Install registers the event source (requires admin) with msgFile, the
// installed OneDriveHelper.exe, as its message file: it carries a "%1" entry
// for every event ID. Sources registered by versions up to 0.8.1 pointed at
// EventCreate.exe, which only knows IDs 1-1000, so Event Viewer showed "The
// description for Event ID ... cannot be found"; they are re-pointed here.
func Install(msgFile string) error { return install(registry.LOCAL_MACHINE, sourceKey, msgFile) }

func install(root registry.Key, path, msgFile string) error {
	k, _, err := registry.CreateKey(root, path, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.SetExpandStringValue("EventMessageFile", msgFile); err != nil {
		return err
	}
	if err := k.SetDWordValue("TypesSupported", eventlog.Error|eventlog.Warning|eventlog.Info); err != nil {
		return err
	}
	// CustomSource marks a source that eventcreate.exe may write to.
	if err := k.DeleteValue("CustomSource"); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

func Remove() error { return eventlog.Remove(version.EventSource) }
