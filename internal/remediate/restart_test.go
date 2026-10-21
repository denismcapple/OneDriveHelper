package remediate

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/dialog"
	"github.com/denismcapple/OneDriveHelper/internal/elog"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
	"github.com/denismcapple/OneDriveHelper/internal/popup"
)

// fakeRestart replaces the restart environment and the dialogs. It records
// what the user was shown and whether Windows was asked to restart or sign out.
type fakeRestart struct {
	blockers  []string // non-empty: a shared or virtual desktop (sign out instead of restarting)
	logon     uint64   // the current sign-in
	choice    int
	restarted bool
	loggedOff bool
	shown     []string // "<kind>: <heading>"
}

var testApproval = approvalInfo{At: time.Now().Add(-time.Hour), On: "RDSH01"}

func useFakeRestart(t *testing.T, f *fixture, fr *fakeRestart) {
	t.Helper()
	savedEnv, savedFlow := restartEnv, flow
	t.Cleanup(func() { restartEnv, flow = savedEnv, savedFlow })
	restartEnv.blockers = func() []string { return fr.blockers }
	restartEnv.restart = func() error { fr.restarted = true; return nil }
	restartEnv.logOff = func() error { fr.loggedOff = true; return nil }
	restartEnv.logonID = func() (uint64, error) { return fr.logon, nil }
	restartEnv.bootTime = func() time.Time { return time.Now().Add(-24 * time.Hour) }
	restartEnv.now = time.Now
	show := func(kind string) func(string, string) {
		return func(h, _ string) { fr.shown = append(fr.shown, kind+": "+h) }
	}
	flow = flowEnv{
		target:  func() (Target, error) { return f.plan.Target, nil },
		recover: func(Target, *log.Logger) {},
		plan:    func(Target, config.Config) (*Plan, error) { return f.plan, nil },
		execute: Execute,
		resync:  func([]onedrive.Orphan, *log.Logger) string { return "" },
		info:    show("info"), success: show("success"), warn: show("warn"), fail: show("fail"),
		confirm: func(string, []popup.Item, string) bool { return true },
		result: func(m dialog.Message) {
			kind := map[dialog.Kind]string{dialog.KindSuccess: "success", dialog.KindWarning: "warn"}[m.Kind]
			fr.shown = append(fr.shown, kind+": "+m.Heading)
		},
		choose: func(h, _ string, _ []string, _ int) int {
			fr.shown = append(fr.shown, "choose: "+h)
			return fr.choice
		},
	}
}

func lockedResult(paths ...string) *Result {
	r := &Result{}
	for _, p := range paths {
		r.Outcomes = append(r.Outcomes, Outcome{Path: p, State: StateSkipped, Locked: true})
	}
	return r
}

// A restart on a single-user workstation; a sign-out on anything shared or virtual.
func TestFinishModeRestartOnWorkstationSignOutElsewhere(t *testing.T) {
	testEnv(t)
	f := newFixture(t, three)
	cfg := config.Defaults()
	for name, c := range map[string]struct {
		blockers []string
		off      bool
		want     string
	}{
		"workstation":     {want: modeRestart},
		"rds/citrix/etc.": {blockers: []string{"this is a multi-session host"}, want: modeSignOut},
		"setting off":     {off: true},
	} {
		t.Run(name, func(t *testing.T) {
			useFakeRestart(t, f, &fakeRestart{blockers: c.blockers})
			cfg.RestartToFinish = !c.off
			if got, _ := finishMode(cfg, []string{f.lib()}); got != c.want {
				t.Errorf("mode %q, want %q", got, c.want)
			}
		})
	}
}

func TestOfferRestartChoices(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for choice, want := range map[int]struct{ pending, acted bool }{0: {false, false}, 1: {true, false}, 2: {true, true}} {
			_, rec := testEnv(t)
			f := newFixture(t, three)
			fr := &fakeRestart{choice: choice, logon: 7}
			if shared {
				fr.blockers = []string{"Citrix is installed"}
			}
			useFakeRestart(t, f, fr)
			if !offerRestart(config.Defaults(), f.plan.Target, lockedResult(f.lib()), discard, 0, testApproval) {
				t.Fatalf("shared %v choice %d: the offer should have been made", shared, choice)
			}
			p, err := loadPending(f.profile)
			acted := fr.restarted || fr.loggedOff
			if (err == nil) != want.pending || acted != want.acted || (acted && fr.loggedOff != shared) {
				t.Errorf("shared %v choice %d: pending %v (%v), restarted %v, signed out %v", shared, choice, p, err, fr.restarted, fr.loggedOff)
			}
			if want.pending {
				wantMode := map[bool]string{false: modeRestart, true: modeSignOut}[shared]
				if p.Mode != wantMode || p.LogonID != 7 || p.ApprovedOn != "RDSH01" || len(p.Paths) != 1 || !rec.has(elog.FixRestartRequested) {
					t.Errorf("shared %v choice %d: pending %+v events %v", shared, choice, p, rec.events)
				}
			}
		}
	}
}

func pendingFor(f *fixture, logon uint64) *Pending {
	return &Pending{Paths: []string{f.lib()}, Mode: modeSignOut, Requested: time.Now().Add(-time.Hour), LogonID: logon,
		Approved: testApproval.At, ApprovedOn: testApproval.On}
}

// At the next sign-in (here, or on another session host: the file travels
// with the profile) the agent finishes the fix by itself.
func TestResumeAtNextSignInFinishesTheFix(t *testing.T) {
	_, rec := testEnv(t)
	f := newFixture(t, three)
	fr := &fakeRestart{logon: 2} // signed in again: a new logon session
	useFakeRestart(t, f, fr)
	must(t, savePending(f.profile, pendingFor(f, 1)))
	busy := 0
	if ResumeAfterRestart(discard, func(b bool) { busy++ }, func() bool { return false }) {
		t.Fatal("no retry expected")
	}
	if exists(f.lib()) || busy != 2 || !rec.has(elog.FixResumed) {
		t.Errorf("folder should be fixed: exists %v busy %d events %v", exists(f.lib()), busy, rec.events)
	}
	if runs, _ := filepath.Glob(filepath.Join(f.backup, "*", "Projects - Documents")); len(runs) != 1 || len(readAll(t, runs[0])) != 3 {
		t.Errorf("backup: %v", runs)
	}
	if _, err := loadPending(f.profile); !os.IsNotExist(err) {
		t.Errorf("the request must be used up: %v", err)
	}
	if len(fr.shown) != 1 || fr.shown[0] != "success: Folders fixed" {
		t.Errorf("shown: %v", fr.shown)
	}
}

func TestResumeDoesNothingInTheSameSignIn(t *testing.T) {
	testEnv(t)
	f := newFixture(t, three)
	useFakeRestart(t, f, &fakeRestart{logon: 1})
	must(t, savePending(f.profile, pendingFor(f, 1)))
	ResumeAfterRestart(discard, func(bool) {}, func() bool { return false })
	if len(readAll(t, f.lib())) != 3 || !RestartPending(f.profile) {
		t.Error("before signing in again nothing may happen and the request must stay")
	}
}

func TestResumeOnlyFixesRequestedFoldersAndExpires(t *testing.T) {
	testEnv(t)
	f := newFixture(t, three)
	other := f.addLib(t, "Finance - Documents", map[string]string{"x.txt": "x"})
	useFakeRestart(t, f, &fakeRestart{logon: 2})
	must(t, savePending(f.profile, pendingFor(f, 1)))
	ResumeAfterRestart(discard, func(bool) {}, func() bool { return false })
	if exists(f.lib()) || len(readAll(t, other)) != 1 {
		t.Error("only the requested folder may be fixed")
	}

	f2 := newFixture(t, three)
	useFakeRestart(t, f2, &fakeRestart{logon: 2})
	old := pendingFor(f2, 1)
	old.Requested = time.Now().Add(-pendingValidity - time.Hour)
	must(t, savePending(f2.profile, old))
	ResumeAfterRestart(discard, func(bool) {}, func() bool { return false })
	if len(readAll(t, f2.lib())) != 3 || RestartPending(f2.profile) {
		t.Error("an expired request is dropped and nothing is changed")
	}
}

func TestResumeStillLockedGivesUp(t *testing.T) {
	testEnv(t)
	f := newFixture(t, map[string]string{"open.docx": "busy"})
	fr := &fakeRestart{logon: 2, choice: 1}
	useFakeRestart(t, f, fr)
	holdOpen(t, filepath.Join(f.lib(), "open.docx"), windows.GENERIC_READ, windows.FILE_SHARE_READ)
	p := pendingFor(f, 1)
	p.Attempts = 1
	must(t, savePending(f.profile, p))
	ResumeAfterRestart(discard, func(bool) {}, func() bool { return false })
	if len(readAll(t, f.lib())) != 1 || RestartPending(f.profile) {
		t.Error("after the last attempt nothing more is scheduled")
	}
	if len(fr.shown) != 1 || !strings.HasPrefix(fr.shown[0], "warn:") {
		t.Errorf("the user is told it is still in use: %v", fr.shown)
	}
}
