package remediate

import (
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/elog"
	"github.com/denismcapple/OneDriveHelper/internal/ipc"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

func TestApprovalDeadlineAlreadyPassed(t *testing.T) {
	now := time.Now()
	shown := false
	code, ev := decideApproval(now.Add(-time.Second), func() time.Time { return now }, func(time.Duration) (bool, bool) {
		shown = true
		return true, false
	})
	if code != ExitCancelled || ev != elog.ApprovalTimedOut || shown {
		t.Errorf("code %d event %d shown %v", code, ev, shown)
	}
}

func TestApprovalDialogLimitBeforeDeadline(t *testing.T) {
	saved := approvalDialogLimit
	approvalDialogLimit = 50 * time.Millisecond
	defer func() { approvalDialogLimit = saved }()
	var gotLimit time.Duration
	code, ev := decideApproval(time.Now().Add(time.Hour), time.Now, func(limit time.Duration) (bool, bool) {
		gotLimit = limit
		return false, true // the dialog closed itself
	})
	if code != ExitCancelled || ev != elog.ApprovalTimedOut || gotLimit != 50*time.Millisecond {
		t.Errorf("code %d event %d limit %s", code, ev, gotLimit)
	}
}

func TestApprovalLimitIsCappedByDeadline(t *testing.T) {
	var gotLimit time.Duration
	decideApproval(time.Now().Add(time.Minute), time.Now, func(limit time.Duration) (bool, bool) {
		gotLimit = limit
		return true, false
	})
	if gotLimit > time.Minute || gotLimit < 55*time.Second {
		t.Errorf("limit %s should be the time left to the deadline", gotLimit)
	}
}

func TestApprovalOutcomes(t *testing.T) {
	if code, ev := decideApproval(time.Now().Add(time.Hour), time.Now, func(time.Duration) (bool, bool) { return true, false }); code != ExitApproved || ev != elog.RemediationStarted {
		t.Errorf("approved: %d %d", code, ev)
	}
	if code, ev := decideApproval(time.Now().Add(time.Hour), time.Now, func(time.Duration) (bool, bool) { return false, false }); code != ExitCancelled || ev != elog.RemediationCancelled {
		t.Errorf("declined: %d %d", code, ev)
	}
	// OK pressed after the deadline: never 5001.
	start := time.Now()
	late := func() time.Time { return start.Add(2 * time.Hour) }
	calls := 0
	clock := func() time.Time {
		calls++
		if calls > 1 {
			return late()
		}
		return start
	}
	if code, ev := decideApproval(start.Add(time.Hour), clock, func(time.Duration) (bool, bool) { return true, false }); code != ExitCancelled || ev != elog.ApprovalTimedOut {
		t.Errorf("late approval: %d %d", code, ev)
	}
}

func TestParseDeadline(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	if got := ParseDeadline("1800000600", start); !got.Equal(time.Unix(1_800_000_600, 0)) {
		t.Errorf("valid: %v", got)
	}
	for _, bad := range []string{"", "soon", "-5"} {
		if got := ParseDeadline(bad, start); !got.Equal(start.Add(approvalDeadline)) {
			t.Errorf("%q: %v", bad, got)
		}
	}
}

// A panic inside Execute (after the folder was staged) unwinds UserFlow:
// the fix lock is released, nothing more runs, and recovery puts the folder back.
func TestPanicDuringExecuteReleasesLockAndIsRecoverable(t *testing.T) {
	e, _ := testEnv(t)
	f := newFixture(t, three)
	e.beforeCheck = func(string) { panic("crash after copying") }
	saved := flow
	defer func() { flow = saved }()
	flow = defaultFlowEnv()
	flow.target = func() (Target, error) { return f.plan.Target, nil }
	flow.recover = func(Target, *log.Logger) {}
	flow.plan = func(Target, config.Config) (*Plan, error) { return f.plan, nil }
	flow.elevate = func(string, []string, time.Time) (uint32, error, <-chan struct{}) {
		done := make(chan struct{})
		close(done)
		return ExitApproved, nil, done
	}
	resynced := false
	flow.resync = func([]onedrive.Orphan, *log.Logger) { resynced = true }
	flow.info, flow.warn, flow.fail = func(string, string) {}, func(string, string) {}, func(string, string) {}
	flow.confirm = func(string, string) bool { return true }
	func() {
		defer func() { recover() }()
		UserFlow("agent.exe", discard, func(bool) {}, func() bool { return false })
	}()
	if ipc.FixInProgress() {
		t.Error("the fix lock must be released by the panic unwinding")
	}
	if resynced {
		t.Error("nothing may run after the panic")
	}
	*e = defaultEnvForRecovery(e)
	if left := Recover(f.plan.Target, discard, nil); len(left) != 0 || len(readAll(t, f.lib())) != 3 {
		t.Errorf("recovery should put the folder back: left %+v, files %v", left, readAll(t, f.lib()))
	}
}

// UserFlow on an approval timeout: the user is told nothing changed, Execute
// is not called, and UserFlow (so the agent's "fixing" state) lasts until the
// stale UAC call returns.
func TestUserFlowApprovalTimeout(t *testing.T) {
	saved := flow
	defer func() { flow = saved }()
	var mu sync.Mutex
	var infos []string
	executed := false
	stale := make(chan struct{})
	flow = flowEnv{
		target:  func() (Target, error) { return Target{UserName: "jsmith", SID: "S-1-5-21-1"}, nil },
		recover: func(Target, *log.Logger) {},
		plan: func(Target, config.Config) (*Plan, error) {
			return &Plan{Items: []Item{{Orphan: onedrive.Orphan{Path: `C:\Users\jsmith\Contoso\Lib`, Name: "Lib"}}},
				FreeBytes: 10, Required: 1}, nil
		},
		elevate: func(string, []string, time.Time) (uint32, error, <-chan struct{}) {
			return 0, winapi.ErrTimedOut, stale
		},
		execute: func(*Plan, *log.Logger, *elog.Log, func() bool) (*Result, error) {
			executed = true
			return &Result{}, nil
		},
		resync:  func([]onedrive.Orphan, *log.Logger) {},
		info:    func(_, m string) { mu.Lock(); infos = append(infos, m); mu.Unlock() },
		warn:    func(_, m string) {},
		fail:    func(_, m string) {},
		confirm: func(string, string) bool { return true },
	}
	done := make(chan struct{})
	go func() {
		UserFlow("agent.exe", discard, func(bool) {}, func() bool { return false })
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("UserFlow must wait for the stale elevation to return")
	case <-time.After(200 * time.Millisecond):
	}
	close(stale)
	<-done
	mu.Lock()
	defer mu.Unlock()
	if executed || len(infos) != 1 || !strings.Contains(infos[0], "nothing was changed") {
		t.Errorf("executed=%v infos=%v", executed, infos)
	}
}
