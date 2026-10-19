package agent

import (
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/elog"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
)

// stateAgent has a scripted process state and counts launches and events.
func stateAgent(state *onedrive.ProcState) (*Agent, *int, *[]uint32) {
	launches, events := 0, []uint32{}
	a := &Agent{
		log:       log.New(io.Discard, "", 0),
		procState: func() onedrive.ProcState { return *state },
		launch:    func() error { launches++; return nil },
		onEvent:   func(id uint32) { events = append(events, id) },
	}
	return a, &launches, &events
}

func TestUnknownStateChangesNothing(t *testing.T) {
	cfg := config.Defaults()
	cfg.OneDriveMissingGraceSeconds = 0
	st := onedrive.StateNotRunning
	a, launches, _ := stateAgent(&st)
	now := time.Unix(1_800_000_000, 0)
	a.checkOneDrive(cfg, now) // first sighting: missingSince = now
	since := a.missingSince
	st = onedrive.StateUnknown
	for i := 1; i <= 10; i++ {
		a.checkOneDrive(cfg, now.Add(time.Duration(i)*time.Minute))
	}
	if a.missingSince != since || *launches != 0 || a.outageLogged {
		t.Errorf("unknown must not move the clock, restart or alert: since=%v launches=%d outage=%v", a.missingSince, *launches, a.outageLogged)
	}
}

func TestUnknownLoggedOnceAfterFive(t *testing.T) {
	cfg := config.Defaults()
	st := onedrive.StateUnknown
	a, _, events := stateAgent(&st)
	now := time.Unix(1_800_000_000, 0)
	count := func() int {
		n := 0
		for _, e := range *events {
			if e == elog.OneDriveStateUnknown {
				n++
			}
		}
		return n
	}
	for i := 1; i <= 4; i++ {
		a.checkOneDrive(cfg, now.Add(time.Duration(i)*time.Minute))
	}
	if count() != 0 {
		t.Fatal("four unknowns log nothing")
	}
	a.checkOneDrive(cfg, now.Add(5*time.Minute))
	a.checkOneDrive(cfg, now.Add(6*time.Minute))
	if count() != 1 {
		t.Errorf("3007 once on the fifth, not repeated: %v", *events)
	}
}

func TestUnknownThenNotRunningUsesLastKnownRunning(t *testing.T) {
	cfg := config.Defaults() // 120 s grace
	st := onedrive.StateRunning
	a, launches, _ := stateAgent(&st)
	t0 := time.Unix(1_800_000_000, 0)
	a.checkOneDrive(cfg, t0) // running
	st = onedrive.StateUnknown
	a.checkOneDrive(cfg, t0.Add(time.Minute))
	a.checkOneDrive(cfg, t0.Add(2*time.Minute))
	st = onedrive.StateNotRunning
	a.checkOneDrive(cfg, t0.Add(3*time.Minute)) // 3 minutes since last seen running > grace
	if !a.missingSince.Equal(t0) || !a.outageLogged || *launches != 1 {
		t.Errorf("should act at once, counting from the last known running time: since=%v outage=%v launches=%d",
			a.missingSince, a.outageLogged, *launches)
	}
}

func TestStatusShowsLastKnownStateAndNoRelaunchWhileUnknown(t *testing.T) {
	st := onedrive.StateUnknown
	a, _, _ := stateAgent(&st)
	a.last = Status{Running: false}
	s := a.collect(config.Config{}) // orphan check off
	s.Configured = true
	if !s.Unknown || s.Running || s.NotRunning() {
		t.Fatalf("status: %+v", s)
	}
	spec := statusSpec(config.Defaults(), s, Actions{})
	for _, b := range spec.Buttons {
		if b.Label == "Restart OneDrive" {
			t.Error("Restart must not be offered while the state is unknown")
		}
	}
	if !strings.Contains(spec.Body, "can't tell") || !strings.Contains(spec.Body, "Last known: not running") {
		t.Errorf("body: %q", spec.Body)
	}
}
