package agent

import (
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/denismcapple/OneDriveHelper/internal/config"
)

func testAgent(launches *int, fail bool) *Agent {
	return &Agent{log: log.New(io.Discard, "", 0), launch: func() error {
		*launches++
		if fail {
			return errors.New("not found")
		}
		return nil
	}}
}

func TestAutoRestartTriesThenGivesUp(t *testing.T) {
	cfg := config.Defaults() // 3 attempts
	var launches int
	a := testAgent(&launches, false)
	now := time.Unix(1_800_000_000, 0)

	// Attempt 1 straight away, then wait at least autoRestartSpacing between tries.
	for i, step := range []struct {
		after       time.Duration
		wantPending bool
		wantLaunch  int
	}{
		{0, true, 1},                  // try #1
		{10 * time.Second, true, 1},   // too soon: give it time to start
		{30 * time.Second, true, 2},   // try #2
		{60 * time.Second, true, 3},   // try #3
		{90 * time.Second, false, 3},  // still not running: give up and warn
		{120 * time.Second, false, 3}, // stays given up for this outage
	} {
		got := a.tryAutoRestart(cfg, now.Add(step.after))
		if got != step.wantPending || launches != step.wantLaunch {
			t.Fatalf("step %d (+%s): pending=%v launches=%d, want %v/%d", i, step.after, got, launches, step.wantPending, step.wantLaunch)
		}
	}
	if !a.autoGaveUp {
		t.Error("should have given up")
	}
}

func TestAutoRestartBudgetAcrossOutages(t *testing.T) {
	cfg := config.Defaults()
	cfg.AutoRestartAttempts = 2
	var launches int
	a := testAgent(&launches, false)
	now := time.Unix(1_800_000_000, 0)

	// Two outages, each fixed by one restart (OneDrive came back, so the state resets)...
	a.tryAutoRestart(cfg, now)
	a.autoGaveUp = false
	a.tryAutoRestart(cfg, now.Add(5*time.Minute))
	a.autoGaveUp = false
	// ...then it crashes again within 30 minutes: the budget is spent, so warn at once.
	if a.tryAutoRestart(cfg, now.Add(10*time.Minute)) || launches != 2 {
		t.Fatalf("a OneDrive that keeps crashing must end in a warning: launches=%d", launches)
	}
	// After the window has passed, automatic restarts are allowed again.
	a.autoGaveUp = false
	if !a.tryAutoRestart(cfg, now.Add(45*time.Minute)) || launches != 3 {
		t.Fatalf("budget should reset after %s: launches=%d", autoRestartWindow, launches)
	}
}

func TestAutoRestartDisabledOrFailingLaunch(t *testing.T) {
	cfg := config.Defaults()
	cfg.AutoRestartEnabled = false
	var launches int
	a := testAgent(&launches, false)
	if a.tryAutoRestart(cfg, time.Now()) || launches != 0 {
		t.Fatal("disabled: must not launch")
	}

	// OneDrive.exe missing: every attempt fails, then the user is warned.
	cfg = config.Defaults()
	launches = 0
	a = testAgent(&launches, true)
	now := time.Unix(1_800_000_000, 0)
	for i := 0; i < 3; i++ {
		if !a.tryAutoRestart(cfg, now.Add(time.Duration(i)*autoRestartSpacing)) {
			t.Fatalf("attempt %d should be pending", i+1)
		}
	}
	if a.tryAutoRestart(cfg, now.Add(3*autoRestartSpacing)) || launches != 3 {
		t.Fatalf("expected warning after 3 failed launches, launches=%d", launches)
	}
}

func TestAutoRestartWording(t *testing.T) {
	cfg := config.Defaults()
	if s := notRunningSpec(cfg, Actions{}, true); s.Body != defaultAutoRestartFailed || !strings.Contains(s.Body, "tried to restart it automatically") {
		t.Errorf("after failed automatic restarts: %q", s.Body)
	}
	if s := notRunningSpec(cfg, Actions{}, false); s.Body != defaultNotRunning {
		t.Errorf("without automatic restarts: %q", s.Body)
	}
	cfg.NotRunningMessage = "Call the desk"
	if s := notRunningSpec(cfg, Actions{}, true); s.Body != "Call the desk" {
		t.Errorf("custom text must win: %q", s.Body)
	}
	if !strings.Contains(Explain(config.Defaults(), topicNotRunning, nil), "tried to restart it for you automatically") {
		t.Error("explanation should mention the automatic restart")
	}
	off := config.Defaults()
	off.AutoRestartEnabled = false
	if strings.Contains(Explain(off, topicNotRunning, nil), "automatically") {
		t.Error("no automatic-restart wording when it is turned off")
	}
}
