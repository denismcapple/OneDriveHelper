package remediate

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/elog"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
)

// TestHelperFixProcess is not a test: TestKilledFixRecoversAndRerunIsSafe
// runs it in a child process, which stops for good right after every file has
// been backed up (before the pre-delete check and any deleting) so the parent
// can kill it there.
func TestHelperFixProcess(t *testing.T) {
	profile := os.Getenv("ODH_HELPER_PROFILE")
	if profile == "" {
		t.Skip("helper process for TestKilledFixRecoversAndRerunIsSafe")
	}
	e, _ := testEnv(t)
	marker := filepath.Join(profile, "copied.marker")
	e.beforeCheck = func(string) {
		_ = os.WriteFile(marker, nil, 0o644)
		time.Sleep(time.Hour) // wait here until killed
	}
	org := filepath.Join(profile, "Contoso")
	lib := filepath.Join(org, "Projects - Documents")
	p := &Plan{Target: Target{UserName: `TEST\jsmith`, ProfileDir: profile}, BackupDir: filepath.Join(profile, "ODHelper", "Saved"),
		FreeBytes: 1 << 40, Required: 1, Items: []Item{{Orphan: onedrive.Orphan{Path: lib, Name: "Projects - Documents", TenantRoot: org}, SaveAs: "Projects - Documents"}}}
	Execute(p, discard, nil, nil)
	t.Fatal("the helper should have been killed before the fix finished")
}

// A fix killed after the backup copy but before cleanup (as by a crash or a
// power cut), then "restarted": recovery puts the folder back, the run leaves
// a report naming the original and the backup, the rerun does not treat
// OneDrive Helper's own folders as orphans, and a second fix never overwrites
// the first run's backup.
func TestKilledFixRecoversAndRerunIsSafe(t *testing.T) {
	_, rec := testEnv(t)
	f := newFixture(t, three)

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperFixProcess$", "-test.v")
	cmd.Env = append(os.Environ(), "ODH_HELPER_PROFILE="+f.profile)
	must(t, cmd.Start())
	defer cmd.Process.Kill()
	marker := filepath.Join(f.profile, "copied.marker")
	for deadline := time.Now().Add(60 * time.Second); !exists(marker); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the helper never reached the point after the backup")
		}
	}
	must(t, cmd.Process.Kill()) // no deferred clean-up runs: like a crash
	_ = cmd.Wait()

	runs, _ := filepath.Glob(filepath.Join(FixingRoot(f.profile), "*"))
	if len(runs) != 1 || exists(f.lib()) {
		t.Fatalf("the folder should be staged in one run: runs %v, original exists %v", runs, exists(f.lib()))
	}
	if journalState(t, runs[0]) != StateCopied {
		t.Fatalf("journal state %s, want copied", journalState(t, runs[0]))
	}
	backups, _ := filepath.Glob(filepath.Join(f.backup, "*"))
	if len(backups) != 1 {
		t.Fatalf("one backup run expected: %v", backups)
	}
	firstBackup := readAll(t, backups[0])

	// "Restart": the next agent start runs recovery (the kernel freed the run lock).
	if left := recoverNow(t, f); len(left) != 0 {
		t.Fatalf("nothing should be left: %+v", left)
	}
	if got := readAll(t, f.lib()); len(got) != 3 || got["b.txt"] != "bb" || !rec.has(elog.FixInterrupted) {
		t.Fatalf("the folder must be back with every file: %v (events %v)", got, rec.events)
	}
	report, err := os.ReadFile(filepath.Join(backups[0], ReportName))
	if err != nil {
		t.Fatalf("a recovery report must be left next to the backup: %v", err)
	}
	for _, want := range []string{f.lib(), filepath.Join(backups[0], "Projects - Documents"), "restored", "user's own copy"} {
		if !strings.Contains(string(report), want) {
			t.Errorf("report should mention %q:\n%s", want, report)
		}
	}
	if exists(runs[0]) {
		t.Error("the finished run's staging folder should be cleaned up")
	}

	// Rerun: OneDrive Helper's own folders are never reported as orphans.
	for _, o := range onedrive.FindOrphans(onedrive.Inputs{ProfileDir: f.profile}, onedrive.OSFS) {
		if strings.Contains(strings.ToLower(o.Path), "odhelper") {
			t.Errorf("own folder reported as an orphan: %s", o.Path)
		}
	}
	// A second fix gets its own run folder; the first run's backup is untouched.
	res := run1(t, f)
	if !res.OK() || onedrive.Clean(res.RunDir) == onedrive.Clean(backups[0]) {
		t.Fatalf("second run: ok %v, run dir %s (first %s)", res.OK(), res.RunDir, backups[0])
	}
	after := readAll(t, backups[0])
	for k, v := range firstBackup {
		if after[k] != v {
			t.Errorf("the first run's backup changed: %s", k)
		}
	}
}

// The backup disk fills up in the middle of a copy: nothing is deleted, the
// folder is put back, and a report says the backup is not to be relied on.
func TestExecuteBackupDiskFullPutsFolderBack(t *testing.T) {
	e, rec := testEnv(t)
	e.backupWriter = func(w io.Writer) io.Writer { return &diskFull{w: w, n: 3} }
	f := newFixture(t, map[string]string{"a.txt": "a", "big.bin": strings.Repeat("x", 4096)})
	res := run1(t, f)
	o := res.Outcomes[0]
	if o.State != StateRestored || o.Removed || len(readAll(t, f.lib())) != 2 || !rec.has(elog.RemediationFailed) {
		t.Fatalf("outcome %+v, events %v", o, rec.events)
	}
	b, err := os.ReadFile(res.Report)
	if err != nil || !strings.Contains(string(b), "restored") || !strings.Contains(string(b), f.lib()) {
		t.Errorf("report %q: %v\n%s", res.Report, err, b)
	}
	if !strings.Contains(resultMessage(res, config.Defaults(), "").PlainText(), res.Report) {
		t.Error("the result message should point support at the report")
	}
}

type diskFull struct {
	w io.Writer
	n int
}

func (d *diskFull) Write(p []byte) (int, error) {
	if d.n <= 0 {
		return 0, &os.PathError{Op: "write", Path: "backup", Err: windows.ERROR_DISK_FULL}
	}
	if len(p) > d.n {
		p = p[:d.n]
	}
	n, err := d.w.Write(p)
	d.n -= n
	if err == nil && d.n <= 0 {
		err = errors.New("There is not enough space on the disk.")
	}
	return n, err
}

// A run never reuses (and so never overwrites) an earlier run's folders,
// even one started in the same second.
func TestExecuteNeverReusesARunFolder(t *testing.T) {
	testEnv(t)
	f := newFixture(t, three)
	now := time.Now()
	var taken []string
	for _, d := range []time.Duration{0, time.Second} {
		dir := filepath.Join(f.backup, now.Add(d).Format("2006-01-02_150405"), "Projects - Documents")
		must(t, os.MkdirAll(dir, 0o755))
		must(t, os.WriteFile(filepath.Join(dir, "earlier.txt"), []byte("earlier"), 0o644))
		taken = append(taken, filepath.Dir(dir))
	}
	res := run1(t, f)
	for _, d := range taken {
		if onedrive.Clean(res.RunDir) == onedrive.Clean(d) {
			t.Fatalf("run reused %s", d)
		}
		if got := readAll(t, d); len(got) != 1 {
			t.Errorf("earlier run %s changed: %v", d, got)
		}
	}
}
