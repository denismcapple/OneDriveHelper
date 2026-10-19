package remediate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"

	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

// FolderState is where one folder is in a fix. Each state is written to the
// journal before the step it names, so an interrupted fix can always be traced.
type FolderState string

const (
	StateMoving   FolderState = "moving"   // about to move the folder to staging
	StateSkipped  FolderState = "skipped"  // could not be moved (files open); untouched
	StateStaged   FolderState = "staged"   // moved to staging, backup not finished
	StateCopied   FolderState = "copied"   // every file backed up, flushed and recorded
	StateDeleting FolderState = "deleting" // all checks passed; deleting verified copies
	StateDone     FolderState = "done"     // staged folder removed
	StatePartial  FolderState = "partial"  // a delete failed part-way; the rest stays staged
	StateRestored FolderState = "restored" // moved back to its original location
	StateKept     FolderState = "kept"     // left in staging (could not be put back safely)
)

// Terminal reports a state no recovery needs to act on.
func (s FolderState) Terminal() bool {
	return s == StateDone || s == StateSkipped || s == StateRestored
}

// JournalEntry is one folder of a fix.
type JournalEntry struct {
	Source     string      `json:"source"`
	Staged     string      `json:"staged"`
	Backup     string      `json:"backup"`
	TenantRoot string      `json:"tenantRoot,omitempty"`
	State      FolderState `json:"state"`
	Note       string      `json:"note,omitempty"`
	Warned     bool        `json:"warned,omitempty"` // recovery already reported it
}

// Journal is the per-run intent record at
// {Profile}\ODHelper\Fixing\<run-id>\manifest.json.
type Journal struct {
	RunID   string          `json:"runId"`
	User    string          `json:"user"`
	SID     string          `json:"sid,omitempty"`
	Started time.Time       `json:"started"`
	RunDir  string          `json:"runDir"`
	Entries []*JournalEntry `json:"folders"`

	path string
}

const (
	journalName = "manifest.json"
	runLockName = "run.lock"
)

// FixingRoot is where folders are staged during a fix. It is always inside
// the profile (so moves are same-volume renames) and outside every folder
// orphan detection scans (it has no OneDrive desktop.ini).
func FixingRoot(profile string) string { return filepath.Join(profile, "ODHelper", "Fixing") }

func (j *Journal) save() error {
	b, err := jsonIndent(j)
	if err != nil {
		return err
	}
	return env.writeJournal(j.path, b)
}

func jsonIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

// set records a new state for e before the step it names.
func (j *Journal) set(e *JournalEntry, s FolderState, note string) error {
	e.State = s
	if note != "" {
		e.Note = note
	}
	return j.save()
}

// writeFileAtomic writes to a temp file, flushes it, then renames it over the
// target with write-through, so a reader never sees a half-written journal.
func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(winapi.LongPath(tmp), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	from, _ := windows.UTF16PtrFromString(winapi.LongPath(tmp))
	to, _ := windows.UTF16PtrFromString(winapi.LongPath(path))
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// LoadJournal reads a journal file.
func LoadJournal(path string) (*Journal, error) {
	b, err := os.ReadFile(winapi.LongPath(path))
	if err != nil {
		return nil, err
	}
	j := &Journal{}
	if err := json.Unmarshal(b, j); err != nil {
		return nil, err
	}
	j.path = path
	return j, nil
}

// holdRunLock creates run.lock in the run's staging folder and keeps it open
// with no sharing for the whole run. Recovery only acts on runs whose lock it
// can open exclusively, i.e. whose agent is gone (the kernel closes the handle
// when a process dies).
func holdRunLock(dir string) (func(), error) {
	p, _ := windows.UTF16PtrFromString(winapi.LongPath(filepath.Join(dir, runLockName)))
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return func() { windows.CloseHandle(h) }, nil
}

// runLockFree reports whether no live process holds the run's lock.
func runLockFree(dir string) bool {
	p, _ := windows.UTF16PtrFromString(winapi.LongPath(filepath.Join(dir, runLockName)))
	h, err := windows.CreateFile(p, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		// Missing lock file: the run never got going (or was cleaned up). Anything
		// else (sharing violation) means its agent is still running.
		return err == windows.ERROR_FILE_NOT_FOUND
	}
	windows.CloseHandle(h)
	return true
}
