package remediate

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/elog"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
)

// testEnv replaces every registry/Explorer step of Execute with a no-op, so
// tests never touch HKCU or Explorer, and records the events logged and the
// journal states written. Tests override single fields as needed.
type recorder struct {
	mu      sync.Mutex
	events  []uint32
	states  []string // "<source base>:<state>" in write order
	navKeep []string
	cleaned []string
}

func (r *recorder) has(id uint32) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e == id {
			return true
		}
	}
	return false
}

func testEnv(t *testing.T) (*execEnv, *recorder) {
	t.Helper()
	saved, savedTries, savedInterval := env, moveTries, moveInterval
	moveTries, moveInterval = 2, 10*time.Millisecond
	rec := &recorder{}
	env = defaultEnv()
	env.active = func(Target, string) bool { return false }
	env.navCleanup = func(_ Target, keep []string, _ *log.Logger) []string {
		rec.navKeep = append(rec.navKeep, keep...)
		return nil
	}
	env.tenantCleanup = func(root string, _ Target) { rec.cleaned = append(rec.cleaned, root) }
	env.onEvent = func(id uint32) { rec.mu.Lock(); rec.events = append(rec.events, id); rec.mu.Unlock() }
	env.writeJournal = func(path string, b []byte) error {
		if j, err := parseJournal(b); err == nil {
			for _, e := range j.Entries {
				rec.states = append(rec.states, filepath.Base(e.Source)+":"+string(e.State))
			}
		}
		return writeFileAtomic(path, b)
	}
	t.Cleanup(func() { env, moveTries, moveInterval = saved, savedTries, savedInterval })
	return &env, rec
}

func parseJournal(b []byte) (*Journal, error) {
	p := filepath.Join(os.TempDir(), fmt.Sprintf("odh-journal-%d.json", time.Now().UnixNano()))
	if err := os.WriteFile(p, b, 0o644); err != nil {
		return nil, err
	}
	defer os.Remove(p)
	return LoadJournal(p)
}

var discard = log.New(io.Discard, "", 0)

// fixture builds a temp "profile" with an org folder holding orphaned
// libraries, and a plan for them.
type fixture struct {
	profile, org, backup string
	libs                 []string
	plan                 *Plan
}

func newFixture(t *testing.T, files map[string]string) *fixture {
	t.Helper()
	f := &fixture{profile: t.TempDir()}
	f.org = filepath.Join(f.profile, "Contoso")
	f.backup = filepath.Join(f.profile, "ODHelper", "Saved")
	f.plan = &Plan{
		Target:    Target{UserName: `TEST\jsmith`, ProfileDir: f.profile},
		BackupDir: f.backup,
		FreeBytes: 1 << 40,
		Required:  1,
	}
	f.addLib(t, "Projects - Documents", files)
	return f
}

func (f *fixture) addLib(t *testing.T, name string, files map[string]string) string {
	t.Helper()
	lib := filepath.Join(f.org, name)
	must(t, os.MkdirAll(lib, 0o755))
	for rel, content := range files {
		p := filepath.Join(lib, rel)
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(content), 0o644))
	}
	f.libs = append(f.libs, lib)
	f.plan.Items = append(f.plan.Items, Item{
		Orphan: onedrive.Orphan{Path: lib, Name: name, TenantRoot: f.org},
		SaveAs: name,
	})
	return lib
}

func (f *fixture) lib() string { return f.libs[0] }

// holdOpen opens path with the given sharing until the test ends (or until
// the returned func is called).
func holdOpen(t *testing.T, path string, access, share uint32) func() {
	t.Helper()
	p, _ := windows.UTF16PtrFromString(path)
	h, err := windows.CreateFile(p, access, share, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	var once sync.Once
	closeFn := func() { once.Do(func() { windows.CloseHandle(h) }) }
	t.Cleanup(closeFn)
	return closeFn
}

func readAll(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(dir, p)
			out[rel] = string(b)
		}
		return nil
	})
	return out
}

func run1(t *testing.T, f *fixture) *Result {
	t.Helper()
	res, err := Execute(f.plan, discard, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

var three = map[string]string{"a.txt": "a", "b.txt": "bb", `sub\c.txt`: "ccc"}

// --- U3: characterization of the safety checks ---

func TestExecuteRefusesWithoutSpace(t *testing.T) {
	testEnv(t)
	f := newFixture(t, map[string]string{"a.txt": "a"})
	f.plan.FreeBytes, f.plan.Required = 10, 100
	if _, err := Execute(f.plan, discard, nil, nil); err == nil {
		t.Fatal("expected an error")
	}
	if exists(f.backup) || exists(FixingRoot(f.profile)) {
		t.Error("no run or staging folder should be created")
	}
	if !exists(filepath.Join(f.lib(), "a.txt")) {
		t.Error("folder must be untouched")
	}
}

func TestExecuteLeavesActiveFolderAlone(t *testing.T) {
	e, _ := testEnv(t)
	e.active = func(Target, string) bool { return true }
	f := newFixture(t, map[string]string{"a.txt": "a"})
	o := run1(t, f).Outcomes[0]
	if o.Removed || o.Copied.LocalFiles != 0 || !exists(filepath.Join(f.lib(), "a.txt")) || o.Error == "" {
		t.Errorf("active folder touched: %+v", o)
	}
}

// --- happy path (U4 + U5) ---

func TestExecuteCleanFolder(t *testing.T) {
	_, rec := testEnv(t)
	f := newFixture(t, three)
	res := run1(t, f)
	o := res.Outcomes[0]
	if !o.Removed || o.State != StateDone || exists(f.lib()) {
		t.Fatalf("folder not removed: %+v", o)
	}
	if got := readAll(t, o.SavedTo); len(got) != 3 || got["a.txt"] != "a" || got[`sub\c.txt`] != "ccc" {
		t.Errorf("backup: %v", got)
	}
	if !exists(filepath.Join(res.RunDir, "manifest.json")) {
		t.Error("manifest.json missing from the run folder")
	}
	if exists(res.StagingDir) {
		t.Error("staging folder should be removed after a clean run")
	}
	want := []string{"moving", "staged", "copied", "deleting", "done"}
	var got []string
	for _, s := range rec.states {
		st := strings.TrimPrefix(s, "Projects - Documents:")
		if len(got) == 0 || got[len(got)-1] != st {
			got = append(got, st)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("journal states %v, want %v", got, want)
	}
	if !rec.has(elog.FixStarted) || !rec.has(elog.RemediationCompleted) || rec.has(elog.RemediationFailed) {
		t.Errorf("events: %v", rec.events)
	}
	if len(rec.cleaned) != 1 || rec.cleaned[0] != f.org {
		t.Errorf("tenant cleanup should run for a done folder: %v", rec.cleaned)
	}
}

func TestExecuteStagesOnProfileVolumeWithBackupElsewhere(t *testing.T) {
	testEnv(t)
	f := newFixture(t, three)
	f.plan.BackupDir = filepath.Join(t.TempDir(), "elsewhere") // another tree, as if BackupRoot were on E:
	res := run1(t, f)
	if !strings.HasPrefix(strings.ToLower(res.StagingDir), strings.ToLower(FixingRoot(f.profile))) {
		t.Errorf("staging %s must be under the profile", res.StagingDir)
	}
	if !res.OK() || len(readAll(t, res.Outcomes[0].SavedTo)) != 3 {
		t.Errorf("result: %+v", res.Outcomes[0])
	}
}

// --- U4: moving aside ---

func TestExecuteSkipsFolderWithOpenFile(t *testing.T) {
	_, rec := testEnv(t)
	f := newFixture(t, map[string]string{"a.txt": "a", "open.docx": "busy"})
	holdOpen(t, filepath.Join(f.lib(), "open.docx"), windows.GENERIC_READ, windows.FILE_SHARE_READ) // no share-delete, like Word
	res := run1(t, f)
	o := res.Outcomes[0]
	if o.State != StateSkipped || o.Removed || !strings.Contains(o.Error, "open") {
		t.Errorf("outcome: %+v", o)
	}
	if got := readAll(t, f.lib()); len(got) != 2 {
		t.Errorf("folder must stay where it was: %v", got)
	}
	if !rec.has(elog.FixSkippedOpen) || rec.has(elog.RemediationFailed) || rec.has(elog.RemediationCompleted) {
		t.Errorf("only skipped: want 5006 and no 5003/5002, got %v", rec.events)
	}
	if exists(res.StagingDir) {
		t.Error("a run that only skipped leaves no staging folder")
	}
	if res.RunDir != "" || len(readAll(t, f.backup)) != 0 {
		t.Errorf("nothing was saved, so no backup folder may be left or mentioned: %q", res.RunDir)
	}
	if res.Changed() {
		t.Error("a run that only skipped changed nothing")
	}
	msg := resultMessage(res, config.Defaults())
	if !strings.Contains(msg, "exactly where they were") || strings.Contains(msg, "saved files are in") {
		t.Errorf("message: %q", msg)
	}
	if !o.Locked || len(res.LockedPaths()) != 1 || res.LockedPaths()[0] != f.lib() {
		t.Errorf("a folder held open must be marked locked (a restart may release it): %+v", o)
	}
}

func TestExecuteFixesCleanFolderAndSkipsBlockedOne(t *testing.T) {
	_, rec := testEnv(t)
	f := newFixture(t, map[string]string{"a.txt": "a"})
	blocked := f.addLib(t, "Finance - Documents", map[string]string{"open.xlsx": "x"})
	holdOpen(t, filepath.Join(blocked, "open.xlsx"), windows.GENERIC_READ, windows.FILE_SHARE_READ)
	res := run1(t, f)
	if res.Outcomes[0].State != StateDone || res.Outcomes[1].State != StateSkipped {
		t.Fatalf("outcomes: %+v", res.Outcomes)
	}
	if exists(f.lib()) || !exists(filepath.Join(blocked, "open.xlsx")) {
		t.Error("clean folder removed, blocked one untouched")
	}
	seen := map[string]bool{}
	for _, s := range rec.states {
		seen[s] = true
	}
	if !seen["Projects - Documents:done"] || !seen["Finance - Documents:skipped"] {
		t.Errorf("each folder has its own journal state: %v", rec.states)
	}
}

func TestWriteFileAtomic(t *testing.T) {
	p := filepath.Join(t.TempDir(), "manifest.json")
	for i := 0; i < 20; i++ {
		j := &Journal{RunID: fmt.Sprint(i), path: p}
		for k := 0; k < i; k++ {
			j.Entries = append(j.Entries, &JournalEntry{Source: fmt.Sprint(k), State: StateStaged})
		}
		b, _ := jsonIndent(j)
		must(t, writeFileAtomic(p, b))
		got, err := LoadJournal(p)
		if err != nil || got.RunID != fmt.Sprint(i) || len(got.Entries) != i {
			t.Fatalf("round %d: %+v %v", i, got, err)
		}
	}
	if exists(p + ".tmp") {
		t.Error("temp file left behind")
	}
}

func TestExecuteJournalFailureBeforeMoveSkipsFolder(t *testing.T) {
	e, _ := testEnv(t)
	inner := e.writeJournal
	e.writeJournal = func(path string, b []byte) error {
		if strings.Contains(string(b), `"state": "moving"`) {
			return errors.New("disk full")
		}
		return inner(path, b)
	}
	f := newFixture(t, three)
	o := run1(t, f).Outcomes[0]
	if o.Removed || !strings.Contains(o.Error, "could not record") || len(readAll(t, f.lib())) != 3 {
		t.Errorf("folder must not be moved: %+v", o)
	}
}

func TestExecuteJournalFailureAfterStagingMovesBack(t *testing.T) {
	e, _ := testEnv(t)
	inner := e.writeJournal
	e.writeJournal = func(path string, b []byte) error {
		if strings.Contains(string(b), `"state": "staged"`) {
			return errors.New("disk full")
		}
		return inner(path, b)
	}
	f := newFixture(t, three)
	o := run1(t, f).Outcomes[0]
	if o.State != StateRestored || len(readAll(t, f.lib())) != 3 {
		t.Errorf("folder must be moved back: %+v", o)
	}
}

func TestExecuteFileChangingWhileCopiedIsCopyError(t *testing.T) {
	e, rec := testEnv(t)
	f := newFixture(t, three)
	e.afterRead = func(src string) {
		if filepath.Base(src) == "b.txt" {
			fh, err := os.OpenFile(src, os.O_APPEND|os.O_WRONLY, 0)
			if err == nil {
				fh.WriteString("more")
				fh.Close()
			}
		}
	}
	o := run1(t, f).Outcomes[0]
	if o.State != StateRestored || o.Removed || len(readAll(t, f.lib())) != 3 {
		t.Errorf("nothing may be deleted and the folder put back: %+v", o)
	}
	if !rec.has(elog.RemediationFailed) {
		t.Error("a copy error logs 5003")
	}
}

type failAfter struct {
	w io.Writer
	n int
}

func (f *failAfter) Write(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errors.New("There is not enough space on the disk.")
	}
	if len(p) > f.n {
		p = p[:f.n]
	}
	n, err := f.w.Write(p)
	f.n -= n
	return n, err
}

func TestExecuteBackupWriterErrorMovesBack(t *testing.T) {
	e, _ := testEnv(t)
	e.backupWriter = func(w io.Writer) io.Writer { return &failAfter{w: w, n: 2} }
	f := newFixture(t, map[string]string{"big.bin": strings.Repeat("x", 100)})
	o := run1(t, f).Outcomes[0]
	if o.State != StateRestored || !strings.Contains(o.Error, "backup is incomplete") || readAll(t, f.lib())["big.bin"] != strings.Repeat("x", 100) {
		t.Errorf("outcome: %+v", o)
	}
}

func TestExecuteFlushErrorIsCopyError(t *testing.T) {
	e, _ := testEnv(t)
	e.flush = func(*os.File) error { return errors.New("flush not supported") }
	f := newFixture(t, three)
	o := run1(t, f).Outcomes[0]
	if o.Removed || len(o.Copied.Errors) == 0 || !strings.Contains(strings.Join(o.Copied.Errors, " "), "flush") {
		t.Errorf("outcome: %+v", o)
	}
	if len(readAll(t, f.lib())) != 3 {
		t.Error("nothing may be deleted")
	}
}

// --- U5: verified delete ---

func TestExecuteFileAddedAfterCopyIsNotDeleted(t *testing.T) {
	e, _ := testEnv(t)
	e.beforeCheck = func(staged string) {
		os.WriteFile(filepath.Join(staged, "new.txt"), []byte("saved during the fix"), 0o644)
	}
	f := newFixture(t, three)
	o := run1(t, f).Outcomes[0]
	if o.State != StateRestored || !strings.Contains(o.Error, "new.txt") {
		t.Errorf("outcome: %+v", o)
	}
	if got := readAll(t, f.lib()); len(got) != 4 || got["new.txt"] != "saved during the fix" {
		t.Errorf("everything must be back in place: %v", got)
	}
}

func TestExecuteFileChangedAfterCopyIsNotDeleted(t *testing.T) {
	e, _ := testEnv(t)
	e.beforeCheck = func(staged string) {
		os.WriteFile(filepath.Join(staged, "a.txt"), []byte("edited"), 0o644)
	}
	f := newFixture(t, three)
	o := run1(t, f).Outcomes[0]
	if o.State != StateRestored || readAll(t, f.lib())["a.txt"] != "edited" {
		t.Errorf("outcome: %+v", o)
	}
}

func TestExecuteFileOpenForWritingIsNotDeleted(t *testing.T) {
	e, _ := testEnv(t)
	var release func()
	e.beforeCheck = func(staged string) {
		release = holdOpen(t, filepath.Join(staged, "b.txt"), windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
	}
	e.move = func(src, dst string) error {
		if release != nil {
			release() // let the folder be moved back once the check has failed
		}
		return moveByHandle(src, dst)
	}
	f := newFixture(t, three)
	o := run1(t, f).Outcomes[0]
	if o.Removed || (o.State != StateRestored && o.State != StateKept) {
		t.Errorf("outcome: %+v", o)
	}
	loc := f.lib()
	if o.State == StateKept {
		loc = o.StagedAt
	}
	if len(readAll(t, loc)) != 3 {
		t.Error("nothing may be deleted")
	}
}

func TestExecutePartialDeleteStopsAndKeepsRest(t *testing.T) {
	e, rec := testEnv(t)
	n := 0
	e.deleteEntry = func(h windows.Handle) error {
		if n++; n == 2 {
			return errors.New("access denied (antivirus)")
		}
		return deleteByHandle(h)
	}
	f := newFixture(t, three)
	res := run1(t, f)
	o := res.Outcomes[0]
	if o.State != StatePartial || o.Removed || len(o.Remaining) == 0 || exists(f.lib()) {
		t.Fatalf("outcome: %+v", o)
	}
	if left := readAll(t, o.StagedAt); len(left) != 2 {
		t.Errorf("the rest stays staged: %v", left)
	}
	if !rec.has(elog.RemediationFailed) {
		t.Error("a partial delete logs 5003")
	}
	if !exists(res.StagingDir) {
		t.Error("the staging folder must stay for a partial run")
	}
	if len(rec.cleaned) != 0 || len(rec.navKeep) != 1 {
		t.Errorf("no tenant cleanup, nav entry kept: %v %v", rec.cleaned, rec.navKeep)
	}
}

func TestExecuteReregisteredKeepsBothCopies(t *testing.T) {
	e, rec := testEnv(t)
	calls := 0
	e.active = func(_ Target, p string) bool {
		calls++
		return calls > 1 // active again at the pre-delete check
	}
	f := newFixture(t, three)
	o := run1(t, f).Outcomes[0]
	if o.State != StateKept || len(readAll(t, o.StagedAt)) != 3 || len(readAll(t, o.SavedTo)) != 3 {
		t.Errorf("outcome: %+v", o)
	}
	if !rec.has(elog.FixReregistered) || rec.has(elog.RemediationFailed) {
		t.Errorf("want 5008 and no 5003: %v", rec.events)
	}
}

func TestExecuteOnlineOnlyEntryRecordedNotCopied(t *testing.T) {
	testEnv(t)
	f := newFixture(t, map[string]string{"local.txt": "here", "cloud.txt": ""})
	p, _ := windows.UTF16PtrFromString(filepath.Join(f.lib(), "cloud.txt"))
	must(t, windows.SetFileAttributes(p, windows.FILE_ATTRIBUTE_OFFLINE))
	o := run1(t, f).Outcomes[0]
	if o.State != StateDone || len(o.OnlineOnly) != 1 || o.Copied.LocalFiles != 1 {
		t.Errorf("outcome: %+v", o)
	}
	if got := readAll(t, o.SavedTo); len(got) != 1 {
		t.Errorf("online-only files are not copied: %v", got)
	}
}

func TestExecuteOriginalRecreatedKeepsInStaging(t *testing.T) {
	e, rec := testEnv(t)
	f := newFixture(t, three)
	inner := e.writeJournal
	e.writeJournal = func(path string, b []byte) error {
		if strings.Contains(string(b), `"state": "copied"`) {
			os.MkdirAll(f.lib(), 0o755) // something re-created the original path
			return errors.New("disk full")
		}
		return inner(path, b)
	}
	o := run1(t, f).Outcomes[0]
	if o.State != StateKept || len(readAll(t, o.StagedAt)) != 3 {
		t.Errorf("outcome: %+v", o)
	}
	if !rec.has(elog.FixKept) || rec.has(elog.RemediationFailed) {
		t.Errorf("want 5012 and no 5003: %v", rec.events)
	}
}

func TestExecuteTenantRootGoneKeepsInStaging(t *testing.T) {
	e, _ := testEnv(t)
	f := newFixture(t, three)
	e.beforeCheck = func(staged string) {
		os.WriteFile(filepath.Join(staged, "new.txt"), []byte("x"), 0o644) // forces a restore
		os.RemoveAll(f.org)                                                // the parent is gone
	}
	o := run1(t, f).Outcomes[0]
	if o.State != StateKept || len(readAll(t, o.StagedAt)) != 4 {
		t.Errorf("outcome: %+v", o)
	}
}

func TestExecuteMoveBackFailureKeeps(t *testing.T) {
	e, rec := testEnv(t)
	e.beforeCheck = func(staged string) { os.WriteFile(filepath.Join(staged, "new.txt"), []byte("x"), 0o644) }
	first := true
	e.move = func(src, dst string) error {
		if first {
			first = false
			return moveByHandle(src, dst)
		}
		return errors.New("move back failed")
	}
	f := newFixture(t, three)
	o := run1(t, f).Outcomes[0]
	if o.State != StateKept || !strings.Contains(o.Error, o.StagedAt) || !rec.has(elog.FixKept) {
		t.Errorf("outcome: %+v events %v", o, rec.events)
	}
}

func TestExecuteFinalJournalCopyFailureIsHarmless(t *testing.T) {
	e, _ := testEnv(t)
	e.copyFinal = func(string, []byte) error { return errors.New("network drive gone") }
	f := newFixture(t, three)
	res := run1(t, f)
	if !res.OK() || res.Outcomes[0].State != StateDone {
		t.Errorf("result must be unchanged: %+v", res.Outcomes[0])
	}
}

func TestExecuteStopsBeforeNextFolder(t *testing.T) {
	testEnv(t)
	f := newFixture(t, three)
	second := f.addLib(t, "Finance - Documents", map[string]string{"x.txt": "x"})
	calls := 0
	stop := func() bool { calls++; return calls > 2 } // fine at start and for folder 1, then stopping
	res, err := Execute(f.plan, discard, nil, stop)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcomes[0].State != StateDone || res.Outcomes[1].State != "" || !res.Stopped {
		t.Errorf("outcomes: %+v", res.Outcomes)
	}
	if !exists(filepath.Join(second, "x.txt")) {
		t.Error("the second folder must not be moved")
	}
}

func TestExecuteStoppedBeforeStart(t *testing.T) {
	testEnv(t)
	f := newFixture(t, three)
	if _, err := Execute(f.plan, discard, nil, func() bool { return true }); err == nil {
		t.Fatal("expected an error")
	}
	if len(readAll(t, f.lib())) != 3 || exists(FixingRoot(f.profile)) {
		t.Error("nothing may be moved")
	}
}

// --- U20: unreadable OneDrive data, links ---

func TestExecuteUnreadableOneDriveDataKeepsFolder(t *testing.T) {
	e, _ := testEnv(t)
	e.active = defaultEnv().active // the real Gather + IsActive
	f := newFixture(t, three)
	f.plan.Target.Hive = unreadableHive(t)
	o := run1(t, f).Outcomes[0]
	if o.Removed || len(readAll(t, f.lib())) != 3 {
		t.Errorf("folder must be left alone: %+v", o)
	}
}

func TestExecuteRestoreRefusedWhenParentIsJunction(t *testing.T) {
	e, _ := testEnv(t)
	f := newFixture(t, three)
	elsewhere := t.TempDir()
	e.beforeCheck = func(staged string) {
		os.WriteFile(filepath.Join(staged, "new.txt"), []byte("x"), 0o644) // forces a restore
		must(t, os.Rename(f.org, f.org+"-old"))
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", f.org, elsewhere).CombinedOutput(); err != nil {
			t.Skipf("cannot create a junction: %v %s", err, out)
		}
	}
	o := run1(t, f).Outcomes[0]
	if o.State != StateKept || len(readAll(t, o.StagedAt)) != 4 {
		t.Errorf("outcome: %+v", o)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Error("nothing may be moved through the junction")
	}
}

// unreadableHive is a throwaway HKCU hive whose OneDrive Accounts key denies reading.
func unreadableHive(t *testing.T) onedrive.Hive {
	t.Helper()
	base := fmt.Sprintf(`Software\OneDriveHelperTest-%d`, time.Now().UnixNano())
	k, _, err := registry.CreateKey(registry.CURRENT_USER, base+`\Software\Microsoft\OneDrive\Accounts`, registry.ALL_ACCESS)
	if err != nil {
		t.Fatal(err)
	}
	k.Close()
	k, err = registry.OpenKey(registry.CURRENT_USER, base+`\Software\Microsoft\OneDrive\Accounts`, windows.WRITE_DAC)
	if err != nil {
		t.Fatal(err)
	}
	setDACL(t, k, "D:P(A;;KA;;;SY)")
	k.Close()
	t.Cleanup(func() {
		if k, err := registry.OpenKey(registry.CURRENT_USER, base+`\Software\Microsoft\OneDrive\Accounts`, windows.WRITE_DAC); err == nil {
			setDACL(t, k, "D:(A;OICI;KA;;;WD)")
			k.Close()
		}
		for _, p := range []string{`\Software\Microsoft\OneDrive\Accounts`, `\Software\Microsoft\OneDrive`, `\Software\Microsoft`, `\Software`, ``} {
			registry.DeleteKey(registry.CURRENT_USER, base+p)
		}
	})
	return onedrive.Hive{Root: registry.CURRENT_USER, Prefix: base + `\`, SID: onedrive.CurrentUserHive().SID}
}

func setDACL(t *testing.T, k registry.Key, sddl string) {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, _ := sd.DACL()
	if err := windows.SetSecurityInfo(windows.Handle(k), windows.SE_REGISTRY_KEY,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}
