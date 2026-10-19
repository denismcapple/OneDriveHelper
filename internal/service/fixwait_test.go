package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denismcapple/OneDriveHelper/internal/elog"
)

// fakeWorld is a virtual clock plus scripted lock / agent state.
type fakeWorld struct {
	t          time.Time
	lock       func(now time.Time) bool
	agents     func(now time.Time) ([]uint32, error)
	terminated []uint32
	events     []uint32
	logs       []string
}

func (f *fakeWorld) env() waitEnv {
	return waitEnv{
		fixInProgress: func() bool { return f.lock != nil && f.lock(f.t) },
		agents: func() ([]uint32, error) {
			if f.agents == nil {
				return nil, nil
			}
			return f.agents(f.t)
		},
		terminate: func(pid uint32) error { f.terminated = append(f.terminated, pid); return nil },
		now:       func() time.Time { return f.t },
		sleep:     func(d time.Duration) { f.t = f.t.Add(d) },
		logf:      func(s string, a ...any) { f.logs = append(f.logs, fmt.Sprintf(s, a...)) },
		event:     func(id uint32, _ bool, _ string) { f.events = append(f.events, id) },
	}
}

func (f *fakeWorld) has(id uint32) bool {
	for _, e := range f.events {
		if e == id {
			return true
		}
	}
	return false
}

var t0 = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func TestWaitForFixWaitsUntilReleased(t *testing.T) {
	f := &fakeWorld{t: t0, lock: func(now time.Time) bool { return now.Before(t0.Add(3 * time.Minute)) }}
	w := f.env()
	w.waitForFix(t0.Add(fixWaitCap))
	if got := f.t.Sub(t0); got < 3*time.Minute || got > 3*time.Minute+3*time.Second {
		t.Errorf("should continue within one poll of the release, waited %s", got)
	}
	if !f.has(elog.InstallerWaitingForFix) || len(f.logs) < 3 {
		t.Errorf("should log 1002 and every minute: %v %v", f.events, f.logs)
	}
}

func TestWaitForFixWithoutLockProceedsAtOnce(t *testing.T) {
	f := &fakeWorld{t: t0}
	f.env().waitForFix(t0.Add(fixWaitCap))
	if f.t != t0 || len(f.events) != 0 {
		t.Errorf("no wait, no 1002: waited %s, events %v", f.t.Sub(t0), f.events)
	}
}

func TestLockHeldPastCapTerminatesAndLogs1003(t *testing.T) {
	saved := fixWaitCap
	fixWaitCap = 5 * time.Minute
	defer func() { fixWaitCap = saved }()
	f := &fakeWorld{t: t0, lock: func(time.Time) bool { return true },
		agents: func(time.Time) ([]uint32, error) { return []uint32{42}, nil }}
	w := f.env()
	deadline := t0.Add(fixWaitCap)
	w.waitForFix(deadline)
	w.stopAgents(deadline, false)
	if f.t.Before(deadline) || len(f.terminated) != 1 || !f.has(elog.InstallerInterruptedFix) {
		t.Errorf("t=%s terminated=%v events=%v", f.t.Sub(t0), f.terminated, f.events)
	}
}

func TestLockAppearingLateIsWaitedFor(t *testing.T) {
	// No lock before the service stops; then an agent takes it (late start) and holds it 2 minutes.
	f := &fakeWorld{t: t0,
		lock: func(now time.Time) bool { return now.After(t0.Add(time.Second)) && now.Before(t0.Add(2*time.Minute)) },
		agents: func(now time.Time) ([]uint32, error) {
			if now.Before(t0.Add(2*time.Minute + time.Second)) {
				return []uint32{7}, nil
			}
			return nil, nil
		}}
	w := f.env()
	deadline := t0.Add(fixWaitCap)
	w.waitForFix(deadline)
	w.stopAgents(deadline, false)
	if len(f.terminated) != 0 || f.t.Sub(t0) < 2*time.Minute {
		t.Errorf("should wait for the fix and let the agent exit: t=%s terminated=%v", f.t.Sub(t0), f.terminated)
	}
}

func TestListingErrorIsNotNoAgents(t *testing.T) {
	saved := fixWaitCap
	fixWaitCap = 2 * time.Minute
	defer func() { fixWaitCap = saved }()
	f := &fakeWorld{t: t0, agents: func(time.Time) ([]uint32, error) { return nil, errors.New("snapshot failed") }}
	w := f.env()
	w.stopAgents(t0.Add(fixWaitCap), false)
	if f.t.Before(t0.Add(fixWaitCap)) {
		t.Errorf("must keep polling the whole window, stopped after %s", f.t.Sub(t0))
	}
}

func TestLegacyAgentsGetTheWholeWindow(t *testing.T) {
	// The installed version predates the lock: an old agent finishing a fix
	// takes 4 minutes to exit on its own.
	f := &fakeWorld{t: t0, agents: func(now time.Time) ([]uint32, error) {
		if now.Before(t0.Add(4 * time.Minute)) {
			return []uint32{9}, nil
		}
		return nil, nil
	}}
	w := f.env()
	w.stopAgents(t0.Add(fixWaitCap), true)
	if len(f.terminated) != 0 || f.t.Sub(t0) < 4*time.Minute {
		t.Errorf("old agent should be allowed to exit by itself: t=%s terminated=%v", f.t.Sub(t0), f.terminated)
	}
}

func TestCurrentAgentsWithoutFixNoExtraWait(t *testing.T) {
	f := &fakeWorld{t: t0, agents: func(now time.Time) ([]uint32, error) {
		if now.Before(t0.Add(time.Second)) {
			return []uint32{3}, nil // exits on the stop event
		}
		return nil, nil
	}}
	w := f.env()
	w.stopAgents(t0.Add(fixWaitCap), false)
	if f.t.Sub(t0) > 2*time.Second || len(f.terminated) != 0 {
		t.Errorf("t=%s terminated=%v", f.t.Sub(t0), f.terminated)
	}
}

func TestStuckCurrentAgentTerminatedAfterGrace(t *testing.T) {
	f := &fakeWorld{t: t0, agents: func(time.Time) ([]uint32, error) { return []uint32{5}, nil }}
	w := f.env()
	w.stopAgents(t0.Add(fixWaitCap), false)
	if f.t.Sub(t0) > agentGrace+time.Second || len(f.terminated) != 1 || f.has(elog.InstallerInterruptedFix) {
		t.Errorf("t=%s terminated=%v events=%v", f.t.Sub(t0), f.terminated, f.events)
	}
}

func TestUnfinishedFixesReadOnly(t *testing.T) {
	p1, p2 := t.TempDir(), t.TempDir()
	write := func(profile, run, state string) string {
		d := filepath.Join(profile, "ODHelper", "Fixing", run)
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "manifest.json"),
			[]byte(`{"folders":[{"source":"C:\\Users\\x\\Contoso\\Lib","staged":"`+strings.ReplaceAll(d, `\`, `\\`)+`\\Lib","state":"`+state+`"}]}`), 0o644)
		return d
	}
	kept := write(p1, "run1", "kept")
	write(p2, "run2", "done")
	got := unfinishedFixes([]string{p1, p2})
	if len(got) != 1 || !strings.Contains(got[0], kept) {
		t.Errorf("want one warning naming %s: %v", kept, got)
	}
	if _, err := os.Stat(filepath.Join(kept, "manifest.json")); err != nil {
		t.Error("nothing may be changed on disk")
	}
}

func TestVersionLess(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.7.1", "0.8.0", true}, {"0.8.0", "0.8.0", false}, {"0.10.0", "0.8.0", false},
		{"1.0.0", "0.8.0", false}, {"dev", "0.8.0", true}, {"v0.9.2", "0.8.0", false},
	} {
		if got := versionLess(c.a, c.b); got != c.want {
			t.Errorf("versionLess(%s, %s) = %v", c.a, c.b, got)
		}
	}
}
