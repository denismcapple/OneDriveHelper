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
	ServiceStarted       = 1000
	ServiceStopped       = 1001
	AgentLaunched        = 1100
	AgentLaunchFailed    = 1101
	AgentStarted         = 2000
	OneDriveNotRunning   = 3001
	OneDriveRelaunched   = 3002
	OneDriveAlertSnoozed = 3003
	OneDriveRecovered    = 3004
	OrphanedLibraries    = 4001
	OrphansResolved      = 4002
	RemediationStarted   = 5001
	RemediationCompleted = 5002
	RemediationFailed    = 5003
	RemediationCancelled = 5004
	ResyncRequested      = 5005
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
