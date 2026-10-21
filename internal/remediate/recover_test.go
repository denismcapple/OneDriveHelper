package remediate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/elog"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
)

// staged builds an interrupted run by hand: the folder already moved to
// staging, with the given journal state.
func stagedRun(t *testing.T, f *fixture, state FolderState) (*Journal, *JournalEntry, string) {
	t.Helper()
	dir := filepath.Join(FixingRoot(f.profile), "2026-01-01_000000")
	must(t, os.MkdirAll(dir, 0o755))
	e := &JournalEntry{Source: f.lib(), Staged: filepath.Join(dir, "Projects - Documents"),
		Backup: filepath.Join(f.backup, "2026-01-01_000000", "Projects - Documents"), TenantRoot: f.org, State: state}
	j := &Journal{RunID: "2026-01-01_000000", User: `TEST\jsmith`, Entries: []*JournalEntry{e}, path: filepath.Join(dir, journalName)}
	must(t, j.save())
	return j, e, dir
}

func recoverNow(t *testing.T, f *fixture) []Interrupted {
	t.Helper()
	return Recover(f.plan.Target, discard, nil)
}

func journalState(t *testing.T, dir string) FolderState {
	t.Helper()
	j, err := LoadJournal(filepath.Join(dir, journalName))
	if err != nil {
		if os.IsNotExist(err) {
			return "cleaned"
		}
		t.Fatal(err)
	}
	return j.Entries[0].State
}

func TestRecoverLeavesRunningFixAlone(t *testing.T) {
	testEnv(t)
	f := newFixture(t, three)
	_, e, dir := stagedRun(t, f, StateStaged)
	must(t, os.Rename(f.lib(), e.Staged))
	release, err := holdRunLock(dir)
	must(t, err)
	defer release()
	left := recoverNow(t, f)
	if len(left) != 1 || !left[0].Running || !exists(e.Staged) || exists(f.lib()) {
		t.Errorf("a running fix must not be touched: %+v", left)
	}
}

func TestDryRunListsInterruptedAndMovesNothing(t *testing.T) {
	testEnv(t)
	f := newFixture(t, three)
	_, e, _ := stagedRun(t, f, StateStaged)
	must(t, os.Rename(f.lib(), e.Staged))
	f.plan.Target.Hive = emptyHive(t)
	p, err := BuildPlan(f.plan.Target, config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Interrupted) != 1 || !strings.Contains(p.Summary(), "did not finish") {
		t.Errorf("plan: %+v\n%s", p.Interrupted, p.Summary())
	}
	if !exists(e.Staged) || exists(f.lib()) {
		t.Error("a plan must not move anything")
	}
}

func TestRecoverMovesStagedFolderBack(t *testing.T) {
	_, rec := testEnv(t)
	f := newFixture(t, three)
	_, e, dir := stagedRun(t, f, StateStaged)
	must(t, os.Rename(f.lib(), e.Staged))
	if left := recoverNow(t, f); len(left) != 0 {
		t.Errorf("nothing should be left: %+v", left)
	}
	if len(readAll(t, f.lib())) != 3 || !rec.has(elog.FixInterrupted) {
		t.Errorf("folder back: %v events %v", readAll(t, f.lib()), rec.events)
	}
	if exists(dir) {
		t.Error("the finished run's staging folder should be cleaned up")
	}
}

func TestRecoverMovingWithStagedFolderIsTreatedAsStaged(t *testing.T) {
	testEnv(t)
	f := newFixture(t, three)
	_, e, _ := stagedRun(t, f, StateMoving)
	must(t, os.Rename(f.lib(), e.Staged))
	recoverNow(t, f)
	if len(readAll(t, f.lib())) != 3 {
		t.Error("folder should be moved back")
	}
}

func TestRecoverMovingWithNothingStagedIsSkipped(t *testing.T) {
	testEnv(t)
	f := newFixture(t, three)
	_, _, dir := stagedRun(t, f, StateMoving)
	if left := recoverNow(t, f); len(left) != 0 || exists(dir) {
		t.Errorf("left %+v", left)
	}
	if len(readAll(t, f.lib())) != 3 {
		t.Error("folder untouched")
	}
}

func TestRecoverAlreadyMovedBackIsRestored(t *testing.T) {
	testEnv(t)
	f := newFixture(t, three) // staged path empty, original present: crashed after the move-back
	_, _, dir := stagedRun(t, f, StateStaged)
	if left := recoverNow(t, f); len(left) != 0 || exists(dir) {
		t.Errorf("left %+v", left)
	}
}

func TestRecoverDeletingOrPartialIsReportedOnce(t *testing.T) {
	for _, st := range []FolderState{StateDeleting, StatePartial} {
		t.Run(string(st), func(t *testing.T) {
			_, rec := testEnv(t)
			f := newFixture(t, three)
			_, e, dir := stagedRun(t, f, st)
			must(t, os.Rename(f.lib(), e.Staged))
			left := recoverNow(t, f)
			if len(left) != 1 || !exists(e.Staged) || exists(f.lib()) {
				t.Fatalf("must be left alone and reported: %+v", left)
			}
			if !strings.Contains(left[0].Text(), e.Staged) || !rec.has(elog.FixInterrupted) {
				t.Errorf("report: %q events %v", left[0].Text(), rec.events)
			}
			n := len(rec.events)
			recoverNow(t, f) // e.g. the next agent start
			if len(rec.events) != n {
				t.Error("5007 must not be logged twice for the same run")
			}
			if journalState(t, dir) != st {
				t.Error("state must not change")
			}
		})
	}
}

func TestRecoverDeletingWithNothingLeftIsDone(t *testing.T) {
	testEnv(t)
	f := newFixture(t, three)
	_, _, dir := stagedRun(t, f, StateDeleting)
	must(t, os.RemoveAll(f.lib()))
	if left := recoverNow(t, f); len(left) != 0 || exists(dir) {
		t.Errorf("left %+v", left)
	}
}

func TestRecoverCopiedWithOriginalRecreatedIsKept(t *testing.T) {
	testEnv(t)
	f := newFixture(t, three)
	_, e, _ := stagedRun(t, f, StateCopied)
	must(t, os.Rename(f.lib(), e.Staged))
	must(t, os.MkdirAll(f.lib(), 0o755)) // something re-created the original
	if left := recoverNow(t, f); len(left) != 1 || len(readAll(t, e.Staged)) != 3 {
		t.Errorf("must be kept and reported: %+v", left)
	}
}

func TestRecoverReadErrorBlocksRestore(t *testing.T) {
	e, _ := testEnv(t)
	e.active = func(Target, string) bool { return true } // OneDrive data unreadable counts as active
	f := newFixture(t, three)
	_, je, _ := stagedRun(t, f, StateStaged)
	must(t, os.Rename(f.lib(), je.Staged))
	if left := recoverNow(t, f); len(left) != 1 || exists(f.lib()) {
		t.Errorf("restore must be blocked and reported: %+v", left)
	}
}

func TestRecoverIgnoresFinishedRuns(t *testing.T) {
	for _, st := range []FolderState{StateDone, StateRestored, StateSkipped} {
		testEnv(t)
		f := newFixture(t, three)
		stagedRun(t, f, st)
		if left := recoverNow(t, f); len(left) != 0 {
			t.Errorf("%s: %+v", st, left)
		}
	}
}

func TestRecoverReportsCorruptJournalNeverActs(t *testing.T) {
	_, rec := testEnv(t)
	f := newFixture(t, three)
	_, e, dir := stagedRun(t, f, StateStaged)
	must(t, os.Rename(f.lib(), e.Staged))
	must(t, os.WriteFile(filepath.Join(dir, journalName), []byte(`{"runId": "x", "folders": [`), 0o644))
	left := recoverNow(t, f)
	if len(left) != 1 || left[0].Corrupt == "" || !exists(e.Staged) || !rec.has(elog.FixInterrupted) {
		t.Errorf("corrupt journal: %+v", left)
	}
	n := len(rec.events)
	recoverNow(t, f)
	if len(rec.events) != n {
		t.Error("reported once")
	}
}

// Crash injection: kill the fix right after each journal write and each move,
// then run recovery. Every file must end up either back in place, or in the
// staged folder plus its backup, and anything not restored must be reported.
func TestRecoverAfterCrashAtEveryStep(t *testing.T) {
	for _, kind := range []string{"journal", "move"} {
		for n := 1; n <= 8; n++ {
			t.Run(fmt.Sprintf("%s-%d", kind, n), func(t *testing.T) {
				e, _ := testEnv(t)
				f := newFixture(t, three)
				count := 0
				crash := errors.New("simulated crash")
				innerJ, innerM := e.writeJournal, e.move
				e.writeJournal = func(p string, b []byte) error {
					err := innerJ(p, b)
					if kind == "journal" {
						if count++; count == n {
							panic(crash)
						}
					}
					return err
				}
				e.move = func(src, dst string) error {
					err := innerM(src, dst)
					if kind == "move" {
						if count++; count == n {
							panic(crash)
						}
					}
					return err
				}
				func() {
					defer func() {
						if r := recover(); r != nil && r != crash {
							panic(r)
						}
					}()
					Execute(f.plan, discard, nil, nil)
				}()
				*e = defaultEnvForRecovery(e)
				left := recoverNow(t, f)

				orig := readAll(t, f.lib())
				var staged, backup map[string]string
				for _, i := range left {
					for _, je := range i.Folders {
						staged, backup = readAll(t, je.Staged), readAll(t, je.Backup)
					}
				}
				switch {
				case len(orig) == 3:
					// restored (or never moved)
				case len(left) == 1 && len(staged) == 3:
					// kept in staging and reported
				case len(left) == 1 && len(staged)+len(backup) >= 3:
					// partly deleted: whatever is gone from staging must be in the backup
					for k, v := range map[string]string{"a.txt": "a", "b.txt": "bb", `sub\c.txt`: "ccc"} {
						if staged[k] != v && backup[k] != v {
							t.Errorf("%s lost (staged %v, backup %v)", k, staged, backup)
						}
					}
				case len(left) == 0 && !exists(f.lib()):
					// the fix completed before the crash point: everything must be in a backup
					if runs, _ := filepath.Glob(filepath.Join(f.backup, "*", "Projects - Documents")); len(runs) == 0 || len(readAll(t, runs[0])) != 3 {
						t.Errorf("completed fix without a full backup: %v", runs)
					}
				default:
					t.Errorf("unexpected state: original %v, left %+v", orig, left)
				}
			})
		}
	}
}

// defaultEnvForRecovery keeps the test's no-op registry hooks but restores
// the real file operations (the crash hooks must not fire during recovery).
func defaultEnvForRecovery(e *execEnv) execEnv {
	d := defaultEnv()
	d.active, d.navCleanup, d.tenantCleanup, d.onEvent = e.active, e.navCleanup, e.tenantCleanup, e.onEvent
	return d
}

func TestStagingFolderIsNotAnOrphan(t *testing.T) {
	f := newFixture(t, three)
	dir := filepath.Join(FixingRoot(f.profile), "2026-01-01_000000", "Projects - Documents")
	must(t, os.MkdirAll(dir, 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644))
	for _, o := range onedrive.FindOrphans(onedrive.Inputs{ProfileDir: f.profile}, onedrive.OSFS) {
		if strings.Contains(strings.ToLower(o.Path), `odhelper`) {
			t.Errorf("staging folder reported as an orphan: %s", o.Path)
		}
	}
}

// emptyHive is a throwaway HKCU hive with no OneDrive keys.
func emptyHive(t *testing.T) onedrive.Hive {
	t.Helper()
	base := fmt.Sprintf(`Software\OneDriveHelperTest-%d`, time.Now().UnixNano())
	k, _, err := registry.CreateKey(registry.CURRENT_USER, base, registry.ALL_ACCESS)
	if err != nil {
		t.Fatal(err)
	}
	k.Close()
	t.Cleanup(func() { registry.DeleteKey(registry.CURRENT_USER, base) })
	return onedrive.Hive{Root: registry.CURRENT_USER, Prefix: base + `\`, SID: onedrive.CurrentUserHive().SID}
}
