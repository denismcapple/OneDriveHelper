package remediate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/denismcapple/OneDriveHelper/internal/elog"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

// Outcome is the result for one folder.
type Outcome struct {
	Path       string      `json:"path"`
	SavedTo    string      `json:"savedTo"`
	State      FolderState `json:"state,omitempty"`
	StagedAt   string      `json:"stagedAt,omitempty"` // where the folder is, if it was kept in staging
	Copied     Stats       `json:"copied"`
	OnlineOnly []string    `json:"onlineOnlyFiles,omitempty"`
	Removed    bool        `json:"removed"`
	Remaining  []string    `json:"remaining,omitempty"` // items not removed after a partial delete
	Error      string      `json:"error,omitempty"`
	ResyncURL  string      `json:"resyncUrl,omitempty"`
	WebURL     string      `json:"webUrl,omitempty"`
}

// Result is written to manifest.json in the run folder.
type Result struct {
	User       string    `json:"user"`
	SID        string    `json:"sid"`
	ApprovedBy string    `json:"approvedBy,omitempty"`
	Started    time.Time `json:"started"`
	Finished   time.Time `json:"finished"`
	RunDir     string    `json:"runDir"`
	StagingDir string    `json:"stagingDir"`
	Outcomes   []Outcome `json:"outcomes"`
	// ExplorerEntriesRemoved lists navigation-pane items that pointed at removed folders.
	ExplorerEntriesRemoved []string `json:"explorerEntriesRemoved,omitempty"`
	// Stopped is set when OneDrive Helper began stopping (e.g. an upgrade) and
	// remaining folders were not started.
	Stopped bool `json:"stopped,omitempty"`
}

// OK reports whether every folder was removed.
func (r *Result) OK() bool {
	for _, o := range r.Outcomes {
		if !o.Removed {
			return false
		}
	}
	return len(r.Outcomes) > 0
}

// Done returns the outcomes of folders that were fully fixed.
func (r *Result) Done() map[string]bool {
	m := map[string]bool{}
	for _, o := range r.Outcomes {
		if o.State == StateDone {
			m[onedrive.Clean(o.Path)] = true
		}
	}
	return m
}

// execEnv holds the steps of Execute that touch the registry, Explorer or the
// file system in ways tests need to replace or make fail. The defaults are the
// real implementations.
type execEnv struct {
	active        func(t Target, path string) bool // is OneDrive syncing path (or can't we tell)?
	navCleanup    func(t Target, keep []string, lg *log.Logger) []string
	tenantCleanup func(root string, t Target)
	backupWriter  func(w io.Writer) io.Writer
	flush         func(f *os.File) error
	afterRead     func(src string) // between reading a file and re-checking it
	move          func(src, dst string) error
	deleteEntry   func(h windows.Handle) error
	writeJournal  func(path string, data []byte) error
	copyFinal     func(path string, data []byte) error // final journal copy into the run folder
	beforeCheck   func(staged string)                  // after the backup, before the pre-delete check
	onEvent       func(id uint32)                      // every event logged (tests)
}

func defaultEnv() execEnv {
	return execEnv{
		active: func(t Target, path string) bool {
			return onedrive.Gather(t.Hive, t.ProfileDir).IsActive(path)
		},
		navCleanup:    removeStaleNavEntries,
		tenantCleanup: cleanupTenantRoot,
		backupWriter:  func(w io.Writer) io.Writer { return w },
		flush:         func(f *os.File) error { return f.Sync() },
		afterRead:     func(string) {},
		move:          moveByHandle,
		deleteEntry:   deleteByHandle,
		writeJournal:  writeFileAtomic,
		copyFinal:     func(p string, b []byte) error { return os.WriteFile(winapi.LongPath(p), b, 0o644) },
		beforeCheck:   func(string) {},
		onEvent:       func(uint32) {},
	}
}

// env is replaced by tests.
var env = defaultEnv()

// removeStaleNavEntries removes Explorer navigation-pane entries that now
// point at nothing, except those for folders in keep (not fully fixed: their
// original location is empty only because the folder is staged).
func removeStaleNavEntries(t Target, keep []string, lg *log.Logger) []string {
	var removed []string
	in := onedrive.Gather(t.Hive, t.ProfileDir)
	stale := onedrive.StaleNavEntries(t.Hive, in, nil)
	for _, e := range stale {
		if overlapsAny(e.Target, keep) {
			continue
		}
		if err := onedrive.RemoveNavEntry(t.Hive, e); err != nil {
			lg.Printf("remove Explorer entry %s (%s): %v", e.Name, e.Target, err)
		} else {
			lg.Printf("removed Explorer entry %s (%s)", e.Name, e.Target)
			removed = append(removed, e.Name)
		}
	}
	if len(removed) > 0 {
		onedrive.RefreshExplorer()
	}
	return removed
}

func overlapsAny(p string, list []string) bool {
	for _, k := range list {
		if within(p, k) || within(k, p) {
			return true
		}
	}
	return false
}

func (r *run) info(id uint32, msg string) { r.ev.Info(id, msg); env.onEvent(id) }
func (r *run) warn(id uint32, msg string) { r.ev.Warning(id, msg); env.onEvent(id) }
func (r *run) fail(id uint32, msg string) { r.ev.Error(id, msg); env.onEvent(id) }

// run is the state of one Execute call.
type run struct {
	p                    *Plan
	lg                   *log.Logger
	ev                   *elog.Log
	j                    *Journal
	res                  *Result
	copyFailed, partials bool
}

// Execute performs the plan as the current (target) user. Each folder is
// first moved to {Profile}\ODHelper\Fixing\<run-id>, then backed up from
// there; only files that were copied, flushed, verified and are unchanged are
// deleted. If anything about a folder is not proven, nothing in it is deleted
// and it is put back (or kept in staging if it can't be put back safely).
// stop (may be nil) is checked before each folder is moved.
func Execute(p *Plan, lg *log.Logger, ev *elog.Log, stop func() bool) (*Result, error) {
	if stop == nil {
		stop = func() bool { return false }
	}
	res := &Result{User: p.Target.UserName, SID: p.Target.SID, Started: time.Now()}
	if !p.EnoughSpace() {
		return res, fmt.Errorf("not enough free space: need %s, have %s", HumanBytes(int64(p.Required)), HumanBytes(int64(p.FreeBytes)))
	}
	if stop() {
		res.Stopped = true
		return res, errors.New("OneDrive Helper is stopping (probably for an upgrade), so the fix was not started")
	}
	runID := res.Started.Format("2006-01-02_150405")
	stageDir := filepath.Join(FixingRoot(p.Target.ProfileDir), runID)
	if err := os.MkdirAll(winapi.LongPath(stageDir), 0o755); err != nil {
		return res, fmt.Errorf("create %s: %w", stageDir, err)
	}
	release, err := holdRunLock(stageDir)
	if err != nil {
		return res, fmt.Errorf("lock %s: %w", stageDir, err)
	}
	locked := true
	defer func() {
		if locked {
			release()
		}
	}()
	if err := os.MkdirAll(p.BackupDir, 0o755); err != nil {
		return res, fmt.Errorf("create %s: %w", p.BackupDir, err)
	}
	res.RunDir = filepath.Join(p.BackupDir, runID)
	if err := os.Mkdir(res.RunDir, 0o755); err != nil {
		return res, fmt.Errorf("create %s: %w", res.RunDir, err)
	}
	res.StagingDir = stageDir
	r := &run{p: p, lg: lg, ev: ev, res: res,
		j: &Journal{RunID: runID, User: p.Target.UserName, SID: p.Target.SID, Started: res.Started, RunDir: res.RunDir,
			path: filepath.Join(stageDir, journalName)}}
	if err := r.j.save(); err != nil {
		return res, fmt.Errorf("could not write the fix record, so nothing was changed: %w", err)
	}
	r.info(elog.FixStarted, fmt.Sprintf("Fix started for %s; staging in %s, saving to %s.", p.Target.UserName, stageDir, res.RunDir))

	for _, it := range p.Items {
		out := Outcome{Path: it.Orphan.Path, SavedTo: filepath.Join(res.RunDir, it.SaveAs), WebURL: it.Orphan.WebURL}
		if it.Orphan.CanResync() {
			out.ResyncURL = it.Orphan.Library.ResyncURL()
		}
		if res.Stopped || stop() {
			res.Stopped = true
			out.Error = "not started: OneDrive Helper is stopping (probably for an upgrade); nothing was changed"
		} else {
			r.folder(it, stageDir, &out)
		}
		res.Outcomes = append(res.Outcomes, out)
	}
	if r.finish() {
		// Nothing left to recover: remove the staging folder (after the lock).
		release()
		locked = false
		cleanStaging(stageDir)
	}
	return res, nil
}

// folder fixes one folder, recording each step in the journal first.
func (r *run) folder(it Item, stageDir string, out *Outcome) {
	t, o := r.p.Target, it.Orphan
	// The confirmation dialog promised that a folder whose scan failed is not removed.
	if n := len(it.Stats.Errors); n > 0 {
		out.Error = fmt.Sprintf("%d item(s) could not be read; this folder will NOT be removed", n)
		return
	}
	// Never touch a folder OneDrive has picked up again (or if we can't tell).
	if env.active(t, o.Path) {
		out.Error = "folder is being synced again (or OneDrive's data could not be read); left untouched"
		return
	}
	// Check again for junctions/links anywhere below the profile, right before moving.
	if err := noLinkBelow(t.ProfileDir, o.Path); err != nil {
		out.Error = "refused: " + err.Error() + "; nothing was changed"
		r.warn(elog.FixRefusedLink, fmt.Sprintf("Fix for %s: %s was not touched because %v.", t.UserName, o.Path, err))
		return
	}
	e := &JournalEntry{Source: o.Path, Staged: filepath.Join(stageDir, it.SaveAs), Backup: out.SavedTo, TenantRoot: o.TenantRoot}
	r.j.Entries = append(r.j.Entries, e)
	if err := r.j.set(e, StateMoving, ""); err != nil {
		r.j.Entries = r.j.Entries[:len(r.j.Entries)-1]
		out.Error = "could not record the fix, so this folder was skipped: " + err.Error()
		return
	}

	// 1. Move the folder aside, so nothing (apps, OneDrive) can write to its
	// original location while it is being backed up.
	if err := env.move(o.Path, e.Staged); err != nil {
		_ = r.j.set(e, StateSkipped, err.Error())
		out.State = StateSkipped
		var busy errBusy
		if errors.As(err, &busy) {
			out.Error = "skipped because files in it are open. Close any files and Explorer windows in that folder and try again"
			r.warn(elog.FixSkippedOpen, fmt.Sprintf("Fix for %s: %s was skipped because it is open in another program (%v).", t.UserName, o.Path, err))
		} else {
			out.Error = "skipped: the folder could not be moved: " + err.Error()
		}
		r.lg.Printf("skip %s: %v", o.Path, err)
		return
	}
	if err := r.j.set(e, StateStaged, ""); err != nil {
		r.restoreOrKeep(e, out, "could not record the fix: "+err.Error())
		return
	}

	// 2. Back up every locally stored file, recording each from its handle.
	r.lg.Printf("saving %s (staged at %s) -> %s", o.Path, e.Staged, out.SavedTo)
	rec, st, online := copyRecorded(e.Staged, out.SavedTo)
	out.Copied, out.OnlineOnly = st, online
	if len(st.Errors) > 0 {
		r.copyFailed = true
		r.lg.Printf("%s: %d item(s) could not be saved: %v", o.Path, len(st.Errors), st.Errors)
		r.restoreOrKeep(e, out, fmt.Sprintf("%d item(s) could not be saved, so nothing was removed (the backup is incomplete)", len(st.Errors)))
		return
	}
	if err := r.j.set(e, StateCopied, ""); err != nil {
		r.restoreOrKeep(e, out, "could not record the fix: "+err.Error())
		return
	}

	// 3. OneDrive started syncing the original location again: keep both copies.
	if env.active(t, o.Path) {
		_ = r.j.set(e, StateKept, "OneDrive is syncing the original location again")
		out.State, out.StagedAt = StateKept, e.Staged
		out.Error = fmt.Sprintf("OneDrive started syncing this location again, so nothing was deleted. The old local copy is in %s and the backup in %s", e.Staged, out.SavedTo)
		r.warn(elog.FixReregistered, fmt.Sprintf("Fix for %s: OneDrive is syncing %s again. Nothing was deleted; old local copy kept in %s, backup in %s.",
			t.UserName, o.Path, e.Staged, out.SavedTo))
		return
	}

	// 4. Check every entry against the record before deleting anything.
	env.beforeCheck(e.Staged)
	v, why := checkForDelete(e.Staged, rec)
	if why != "" {
		r.copyFailed = true
		r.restoreOrKeep(e, out, "nothing was removed because "+why)
		return
	}
	defer v.close() // also on a panic, so no file stays locked
	if err := r.j.set(e, StateDeleting, ""); err != nil {
		v.close()
		r.restoreOrKeep(e, out, "could not record the fix: "+err.Error())
		return
	}

	// 5. Delete the verified copies. A failure now stops at once and the rest
	// stays staged; a partly emptied folder is never moved back.
	if remaining, err := deleteVerified(e.Staged, v, rec); err != nil {
		r.partials = true
		_ = r.j.set(e, StatePartial, err.Error())
		out.State, out.StagedAt, out.Remaining = StatePartial, e.Staged, remaining
		out.Error = fmt.Sprintf("only partly removed (%v); everything not removed is in %s", err, e.Staged)
		r.lg.Printf("partial delete of %s: %v; remaining: %v", e.Staged, err, remaining)
		return
	}
	_ = r.j.set(e, StateDone, "")
	out.State, out.Removed = StateDone, true
}

// restoreOrKeep runs when nothing has been deleted yet: the folder goes back
// to its original location if that is free, its parent is a real folder and
// OneDrive isn't syncing it; otherwise it stays in staging.
func (r *run) restoreOrKeep(e *JournalEntry, out *Outcome, why string) {
	out.Error = why
	if !exists(e.Source) && parentSafe(e.Source) && !env.active(r.p.Target, e.Source) {
		if err := env.move(e.Staged, e.Source); err == nil {
			_ = r.j.set(e, StateRestored, why)
			out.State = StateRestored
			out.Error = why + "; the folder was put back where it was"
			return
		} else {
			why += "; it could not be put back: " + err.Error()
		}
	} else {
		why += "; it could not be put back safely"
	}
	_ = r.j.set(e, StateKept, why)
	out.State, out.StagedAt = StateKept, e.Staged
	out.Error = fmt.Sprintf("%s. The folder is kept in %s", why, e.Staged)
	r.warn(elog.FixKept, fmt.Sprintf("Fix for %s: %s was kept in %s (%s).", r.p.Target.UserName, e.Source, e.Staged, why))
}

// finish runs the follow-up steps for fully fixed folders, writes the final
// record and the summary event. It reports whether every folder ended in a
// terminal state (nothing for recovery to do).
func (r *run) finish() bool {
	res, t := r.res, r.p.Target
	var keep []string
	pending := map[string]bool{} // tenant roots with a folder that is not done
	for _, e := range r.j.Entries {
		if e.State != StateDone {
			keep = append(keep, e.Source)
			pending[onedrive.Clean(e.TenantRoot)] = true
		}
	}
	for _, e := range r.j.Entries {
		if e.State == StateDone && !pending[onedrive.Clean(e.TenantRoot)] {
			env.tenantCleanup(e.TenantRoot, t)
		}
	}
	res.ExplorerEntriesRemoved = env.navCleanup(t, keep, r.lg)
	res.Finished = time.Now()
	_ = r.j.save()
	if b, err := json.MarshalIndent(res, "", "  "); err == nil {
		if err := env.copyFinal(filepath.Join(res.RunDir, journalName), b); err != nil {
			r.lg.Printf("copy the fix record to %s: %v (the record in %s stays)", res.RunDir, err, res.StagingDir)
		}
	}
	terminal := true
	for _, e := range r.j.Entries {
		terminal = terminal && e.State.Terminal()
	}

	msg := fmt.Sprintf("Fix for %s finished. Backups: %s\r\n%s", t.UserName, res.RunDir, res.Text())
	switch {
	case res.OK():
		r.info(elog.RemediationCompleted, msg)
	case r.copyFailed || r.partials:
		r.fail(elog.RemediationFailed, msg)
	}
	return terminal
}

// cleanStaging removes a finished run's staging folder (journal, lock, temp).
func cleanStaging(dir string) {
	for _, n := range []string{journalName, journalName + ".tmp", runLockName} {
		_ = os.Remove(winapi.LongPath(filepath.Join(dir, n)))
	}
	_ = removeDir(dir)
	_ = removeDir(filepath.Dir(dir)) // Fixing, if now empty
}

// Text summarises the result.
func (r *Result) Text() string {
	var b strings.Builder
	for _, o := range r.Outcomes {
		status := "NOT removed"
		switch o.State {
		case StateDone:
			status = "removed"
		case StateRestored:
			status = "NOT removed, put back where it was"
		case StateKept:
			status = "NOT removed, kept in " + o.StagedAt
		case StatePartial:
			status = "only partly removed, the rest is in " + o.StagedAt
		case StateSkipped:
			status = "skipped"
		}
		fmt.Fprintf(&b, "• %s: %d file(s) saved, %s", o.Path, o.Copied.LocalFiles, status)
		if o.Error != "" && o.State != StateKept && o.State != StatePartial {
			b.WriteString(" - " + o.Error)
		} else if o.Error != "" {
			b.WriteString(" - " + strings.SplitN(o.Error, ". The folder is kept", 2)[0])
		}
		b.WriteString("\n")
	}
	if len(r.ExplorerEntriesRemoved) > 0 {
		b.WriteString("• Removed from Explorer's side panel: " + strings.Join(r.ExplorerEntriesRemoved, ", ") + "\n")
	}
	return b.String()
}
