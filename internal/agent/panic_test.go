package agent

import (
	"bytes"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
)

func TestActionPanicIsLoggedAndAgentGoesOn(t *testing.T) {
	var buf bytes.Buffer
	st := onedrive.StateRunning
	a := &Agent{log: log.New(&buf, "", 0), procState: func() onedrive.ProcState { return st }}
	a.safe("copy details", func() { panic("boom") })() // must not propagate
	if !strings.Contains(buf.String(), "panic in copy details: boom") || !strings.Contains(buf.String(), "goroutine") {
		t.Errorf("panic and stack should be logged: %q", buf.String())
	}
	a.checkOneDrive(config.Defaults(), time.Now()) // the agent's next check still runs
	if a.lastRunning.IsZero() {
		t.Error("next check did not run")
	}
}

// The tray icon must not stay red until the next interval after a fix: the
// check runs as soon as the file work is done, before the result card is closed.
func TestFixRefreshesStatusAsSoonAsFilesAreDone(t *testing.T) {
	a := &Agent{log: log.New(io.Discard, "", 0), wake: make(chan struct{}, 1)}
	var woken []bool
	a.userFlow = func(_ string, _ *log.Logger, busy func(bool), _ func() bool) {
		busy(true)
		woken = append(woken, len(a.wake) > 0)
		busy(false)
		woken = append(woken, len(a.wake) > 0) // result card still open here
	}
	a.actions().Fix()
	if len(woken) != 2 || woken[0] || !woken[1] {
		t.Errorf("woken during/after the file work = %v, want [false true]", woken)
	}
	if len(a.wake) != 1 {
		t.Error("a pending check must still be queued after the fix")
	}
	a.refresh() // never blocks, even with a check already queued
	(&Agent{}).refresh()
}

func TestPanicInFixClearsFixingAndRecoversAtOnce(t *testing.T) {
	var buf bytes.Buffer
	recovered := 0
	a := &Agent{
		log: log.New(&buf, "", 0),
		userFlow: func(string, *log.Logger, func(bool), func() bool) {
			panic("crash after staging")
		},
		afterPanic: func() { recovered++ },
	}
	a.actions().Fix()
	if a.fixing {
		t.Error("fixing must be cleared after a panic")
	}
	if recovered != 1 {
		t.Error("recovery must run straight after the panic, without an agent restart")
	}
	if !strings.Contains(buf.String(), "panic in fix") {
		t.Errorf("log: %q", buf.String())
	}
}
