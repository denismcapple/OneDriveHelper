package remediate

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/denismcapple/OneDriveHelper/internal/elog"
	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

// Interrupted is a fix that did not finish: its staging folder still holds a
// journal with folders that are not done, restored or skipped.
type Interrupted struct {
	Dir     string          // {Profile}\ODHelper\Fixing\<run-id>
	Running bool            // its agent is still working on it
	Corrupt string          // the journal could not be read (never acted on)
	Folders []*JournalEntry // the unfinished folders
}

// Text describes an interrupted fix for the status window, `check` and dialogs.
func (i Interrupted) Text() string {
	var b strings.Builder
	switch {
	case i.Corrupt != "":
		fmt.Fprintf(&b, "Unreadable fix record in %s (%s). Check it by hand before deleting that folder.\n", i.Dir, i.Corrupt)
	case i.Running:
		fmt.Fprintf(&b, "Fix in progress in %s.\n", i.Dir)
	}
	for _, e := range i.Folders {
		fmt.Fprintf(&b, "%s (%s): now in %s; backup in %s\n", e.Source, e.State, e.Staged, e.Backup)
	}
	return b.String()
}

// ListInterrupted returns unfinished fixes for a profile without changing
// anything (used by plans, dry runs, the approval step, status and uninstall).
func ListInterrupted(profile string) []Interrupted {
	dirs, _ := filepath.Glob(filepath.Join(FixingRoot(profile), "*"))
	sort.Strings(dirs)
	var out []Interrupted
	for _, d := range dirs {
		if fi, err := os.Lstat(d); err != nil || !fi.IsDir() {
			continue
		}
		i := Interrupted{Dir: d, Running: !runLockFree(d)}
		j, err := LoadJournal(filepath.Join(d, journalName))
		if err != nil {
			if os.IsNotExist(err) && i.Running {
				continue // a run that is just starting
			}
			i.Corrupt = err.Error()
			out = append(out, i)
			continue
		}
		for _, e := range j.Entries {
			if !e.State.Terminal() {
				i.Folders = append(i.Folders, e)
			}
		}
		if len(i.Folders) > 0 {
			out = append(out, i)
		}
	}
	return out
}

// Recover finishes what it safely can of fixes whose agent died (crash, kill,
// power loss, logoff) and reports the rest once. It must only ever run in the
// user's own agent: never elevated, never in a dry run. It returns the fixes
// that still need attention.
//
// What is on disk decides; the recorded state only chooses between outcomes:
//   - moving, nothing staged: skipped (the move never happened)
//   - staged path empty and the original present: restored (moved back already)
//   - deleting, nothing at either path: done
//   - moving/staged/copied with the folder staged: moved back if the original
//     location is free, its parent is a real folder and OneDrive isn't syncing it
//   - anything else (deleting, partial, kept, or a restore that isn't safe):
//     left alone and reported (5007 Warning, once per run)
func Recover(t Target, lg *log.Logger, ev *elog.Log) []Interrupted {
	var still []Interrupted
	for _, i := range ListInterrupted(t.ProfileDir) {
		if i.Running {
			still = append(still, i) // never touch a fix that is still running
			continue
		}
		if i.Corrupt != "" {
			marker := filepath.Join(i.Dir, "reported")
			if !exists(marker) {
				ev.Warning(elog.FixInterrupted, fmt.Sprintf("An interrupted OneDrive Helper fix for %s has an unreadable record in %s (%s). Check that folder by hand.", t.UserName, i.Dir, i.Corrupt))
				env.onEvent(elog.FixInterrupted)
				_ = os.WriteFile(winapi.LongPath(marker), nil, 0o644)
			}
			still = append(still, i)
			continue
		}
		if left := recoverRun(t, i.Dir, lg, ev); len(left.Folders) > 0 {
			still = append(still, left)
		}
	}
	return still
}

func recoverRun(t Target, dir string, lg *log.Logger, ev *elog.Log) Interrupted {
	j, err := LoadJournal(filepath.Join(dir, journalName))
	if err != nil {
		return Interrupted{Dir: dir, Corrupt: err.Error()}
	}
	var report []string
	changed := false
	for _, e := range j.Entries {
		if e.State.Terminal() {
			continue
		}
		staged, orig := exists(e.Staged), exists(e.Source)
		next := e.State
		switch {
		case e.State == StateMoving && !staged:
			next = StateSkipped
		case (e.State == StateMoving || e.State == StateStaged || e.State == StateCopied) && !staged && orig:
			next = StateRestored
		case e.State == StateDeleting && !staged && !orig:
			next = StateDone
		case e.State == StateMoving || e.State == StateStaged || e.State == StateCopied:
			if !orig && parentSafe(e.Source) && !env.active(t, e.Source) {
				if err := env.move(e.Staged, e.Source); err == nil {
					next = StateRestored
					lg.Printf("recovery: moved %s back to %s", e.Staged, e.Source)
					ev.Info(elog.FixInterrupted, fmt.Sprintf("An interrupted OneDrive Helper fix for %s was undone: %s was put back where it was (backup in %s).", t.UserName, e.Source, e.Backup))
					env.onEvent(elog.FixInterrupted)
				} else {
					lg.Printf("recovery: could not move %s back: %v", e.Staged, err)
				}
			}
		}
		if next != e.State {
			e.State, changed = next, true
			continue
		}
		if !e.Warned {
			report = append(report, fmt.Sprintf("%s (%s): now in %s; backup in %s", e.Source, e.State, e.Staged, e.Backup))
			e.Warned, changed = true, true
		}
	}
	j.path = filepath.Join(dir, journalName)
	if changed {
		if err := j.save(); err != nil {
			lg.Printf("recovery: update %s: %v", j.path, err)
		}
	}
	if len(report) > 0 {
		ev.Warning(elog.FixInterrupted, fmt.Sprintf("An interrupted OneDrive Helper fix for %s needs attention. These folders are not where they were:\r\n%s\r\nRun folder: %s",
			t.UserName, strings.Join(report, "\r\n"), dir))
		env.onEvent(elog.FixInterrupted)
	}
	left := Interrupted{Dir: dir}
	for _, e := range j.Entries {
		if !e.State.Terminal() {
			left.Folders = append(left.Folders, e)
		}
	}
	if len(left.Folders) == 0 {
		cleanStaging(dir)
	}
	return left
}
