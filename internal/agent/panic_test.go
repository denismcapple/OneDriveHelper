package agent

import (
	"bytes"
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
