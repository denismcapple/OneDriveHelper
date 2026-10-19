package remediate

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
)

// testEnv replaces every registry/Explorer step of Execute with a no-op, so
// tests never touch HKCU or Explorer. Tests override single fields as needed.
func testEnv(t *testing.T) *execEnv {
	t.Helper()
	saved := env
	env = defaultEnv()
	env.active = func(Target, string) bool { return false }
	env.navCleanup = func(Target, *log.Logger) []string { return nil }
	env.tenantCleanup = func(string, Target) {}
	t.Cleanup(func() { env = saved })
	return &env
}

var discard = log.New(io.Discard, "", 0)

// fixture builds a temp "profile" with an org folder holding one orphaned
// library containing the given files, and a plan for it.
type fixture struct {
	profile, org, lib, backup string
	plan                      *Plan
}

func newFixture(t *testing.T, files map[string]string) *fixture {
	t.Helper()
	f := &fixture{profile: t.TempDir()}
	f.org = filepath.Join(f.profile, "Contoso")
	f.lib = filepath.Join(f.org, "Projects - Documents")
	f.backup = filepath.Join(f.profile, "ODHelper", "Saved")
	for rel, content := range files {
		p := filepath.Join(f.lib, rel)
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(content), 0o644))
	}
	must(t, os.MkdirAll(f.lib, 0o755))
	f.plan = &Plan{
		Target:    Target{UserName: `TEST\jsmith`, ProfileDir: f.profile},
		BackupDir: f.backup,
		Items: []Item{{
			Orphan: onedrive.Orphan{Path: f.lib, Name: "Projects - Documents", TenantRoot: f.org},
			SaveAs: "Projects - Documents",
		}},
		FreeBytes: 1 << 40,
		Required:  1,
	}
	return f
}

// holdExclusive opens path with no sharing until the test ends.
func holdExclusive(t *testing.T, path string) {
	t.Helper()
	p, _ := windows.UTF16PtrFromString(path)
	h, err := windows.CreateFile(p, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("lock %s: %v", path, err)
	}
	t.Cleanup(func() { windows.CloseHandle(h) })
}

func TestExecuteRefusesWithoutSpace(t *testing.T) {
	testEnv(t)
	f := newFixture(t, map[string]string{"a.txt": "a"})
	f.plan.FreeBytes, f.plan.Required = 10, 100
	if _, err := Execute(f.plan, discard, nil); err == nil {
		t.Fatal("expected an error")
	}
	if exists(f.backup) {
		t.Error("no run folder should be created")
	}
	if !exists(f.lib) {
		t.Error("folder must be untouched")
	}
}

func TestExecuteLeavesActiveFolderAlone(t *testing.T) {
	e := testEnv(t)
	e.active = func(Target, string) bool { return true }
	f := newFixture(t, map[string]string{"a.txt": "a"})
	res, err := Execute(f.plan, discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	o := res.Outcomes[0]
	if o.Removed || o.Copied.LocalFiles != 0 || !exists(filepath.Join(f.lib, "a.txt")) {
		t.Errorf("active folder touched: %+v", o)
	}
	if o.Error == "" {
		t.Error("outcome should say it is being synced again")
	}
}

func TestExecuteKeepsFolderOnCopyError(t *testing.T) {
	testEnv(t)
	f := newFixture(t, map[string]string{"a.txt": "a", "locked.txt": "busy"})
	holdExclusive(t, filepath.Join(f.lib, "locked.txt"))
	res, err := Execute(f.plan, discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	if o := res.Outcomes[0]; o.Removed || o.Error == "" {
		t.Errorf("outcome: %+v", o)
	}
	if !exists(filepath.Join(f.lib, "a.txt")) || !exists(filepath.Join(f.lib, "locked.txt")) {
		t.Error("files must stay in place after a copy error")
	}
}

func TestExecuteCleanFolder(t *testing.T) {
	testEnv(t)
	f := newFixture(t, map[string]string{"a.txt": "a", "b.txt": "bb", `sub\c.txt`: "ccc"})
	res, err := Execute(f.plan, discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	o := res.Outcomes[0]
	if !o.Removed || exists(f.lib) {
		t.Fatalf("folder not removed: %+v", o)
	}
	for rel, want := range map[string]string{"a.txt": "a", "b.txt": "bb", `sub\c.txt`: "ccc"} {
		b, err := os.ReadFile(filepath.Join(o.SavedTo, rel))
		if err != nil || string(b) != want {
			t.Errorf("backup %s: %q %v", rel, b, err)
		}
	}
	if !exists(filepath.Join(res.RunDir, "manifest.json")) {
		t.Error("manifest.json missing")
	}
	if !res.OK() {
		t.Error("result should be OK")
	}
}
