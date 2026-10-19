package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

var progs = []string{"OneDriveHelper.exe", "OneDriveHelperAgent.exe", "OneDriveHelperConfig.exe"}

// setup makes a source folder with "new" programs and an install folder with "old" ones.
func setup(t *testing.T, srcProgs []string) (src, dir string) {
	t.Helper()
	src, dir = t.TempDir(), t.TempDir()
	for _, p := range srcProgs {
		must(t, os.WriteFile(filepath.Join(src, p), []byte("new "+p), 0o644))
	}
	for _, p := range progs {
		must(t, os.WriteFile(filepath.Join(dir, p), []byte("old "+p), 0o644))
	}
	return src, dir
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func contents(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		out[e.Name()] = string(b)
	}
	return out
}

func allOld(t *testing.T, dir string) {
	t.Helper()
	got := contents(t, dir)
	for _, p := range progs {
		if got[p] != "old "+p {
			t.Errorf("%s = %q, want the old version", p, got[p])
		}
	}
}

func TestUpgradeSwapsAndCleansUpNextTime(t *testing.T) {
	src, dir := setup(t, progs)
	rb, err := upgrade(src, dir, progs, realFileOps())
	if err != nil || rb == nil {
		t.Fatal(err)
	}
	got := contents(t, dir)
	for _, p := range progs {
		if got[p] != "new "+p {
			t.Errorf("%s = %q", p, got[p])
		}
	}
	var first []string
	for name := range got {
		if strings.Contains(name, ".old-") {
			first = append(first, name)
		}
	}
	if len(first) != 3 {
		t.Fatalf("the previous programs are kept as .old-* until the next install: %v", got)
	}
	// The next install removes the .old-* files of this one.
	src2, _ := setup(t, progs)
	if _, err := upgrade(src2, dir, progs, realFileOps()); err != nil {
		t.Fatal(err)
	}
	after := contents(t, dir)
	for _, name := range first {
		if _, ok := after[name]; ok {
			t.Errorf("stale file from the previous install left: %s", name)
		}
	}
	for name := range after {
		if strings.HasSuffix(name, ".new") {
			t.Errorf("staging file left: %s", name)
		}
	}
}

func TestUpgradeStagingFailureChangesNothing(t *testing.T) {
	src, dir := setup(t, progs)
	o := realFileOps()
	o.copy = func(s, d string) error {
		if strings.Contains(d, "Agent") {
			return errors.New("disk full")
		}
		return copyFlush(s, d)
	}
	if _, err := upgrade(src, dir, progs, o); err == nil {
		t.Fatal("expected an error")
	}
	allOld(t, dir)
	if len(contents(t, dir)) != 3 {
		t.Errorf("no .new files may be left: %v", contents(t, dir))
	}
}

func TestUpgradeSwapFailureRollsBack(t *testing.T) {
	src, dir := setup(t, progs)
	o := realFileOps()
	o.rename = func(from, to string) error {
		if strings.HasSuffix(from, "OneDriveHelperConfig.exe.new") {
			return errors.New("access denied")
		}
		return os.Rename(from, to)
	}
	if _, err := upgrade(src, dir, progs, o); err == nil {
		t.Fatal("expected an error")
	}
	allOld(t, dir)
}

func TestUpgradeRollbackAfterServiceFailsToStart(t *testing.T) {
	src, dir := setup(t, progs)
	rb, err := upgrade(src, dir, progs, realFileOps())
	if err != nil {
		t.Fatal(err)
	}
	// The new service didn't start: Install calls rollback.
	if err := rb(); err != nil {
		t.Fatal(err)
	}
	allOld(t, dir)
}

func TestUpgradeIgnoresStaleFileInUse(t *testing.T) {
	src, dir := setup(t, progs)
	stale := filepath.Join(dir, "OneDriveHelperAgent.exe.old-20260101-000000.000")
	must(t, os.WriteFile(stale, []byte("very old"), 0o644))
	p, _ := windows.UTF16PtrFromString(stale)
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, 0, 0)
	must(t, err)
	defer windows.CloseHandle(h)
	if _, err := upgrade(src, dir, progs, realFileOps()); err != nil {
		t.Fatalf("a stale file in use must not block the upgrade: %v", err)
	}
	if contents(t, dir)["OneDriveHelperAgent.exe"] != "new OneDriveHelperAgent.exe" {
		t.Error("agent not upgraded")
	}
}

func TestUpgradeWithoutConfigTool(t *testing.T) {
	src, dir := setup(t, progs[:2])
	if _, err := upgrade(src, dir, progs, realFileOps()); err != nil {
		t.Fatal(err)
	}
	got := contents(t, dir)
	if got[progs[0]] != "new "+progs[0] || got[progs[1]] != "new "+progs[1] || got[progs[2]] != "old "+progs[2] {
		t.Errorf("got %v", got)
	}
}

func TestUpgradeRemovesLeftoversBeforeStaging(t *testing.T) {
	src, dir := setup(t, progs)
	for _, n := range []string{"OneDriveHelper.exe.new", "OneDriveHelper.exe.old", "OneDriveHelper.exe.old-20250101-000000.000"} {
		must(t, os.WriteFile(filepath.Join(dir, n), []byte("junk"), 0o644))
	}
	if _, err := upgrade(src, dir, progs, realFileOps()); err != nil {
		t.Fatal(err)
	}
	for name, c := range contents(t, dir) {
		if c == "junk" {
			t.Errorf("leftover %s not removed", name)
		}
	}
}

func TestPreflightRejectsBadSettingsBeforeStopping(t *testing.T) {
	src, _ := setup(t, progs)
	bad := filepath.Join(t.TempDir(), "bad.json")
	must(t, os.WriteFile(bad, []byte(`{"SupportEmail": "not an e-mail"}`), 0o644))
	if err := preflight(src, "OneDriveHelper.exe", bad); err == nil {
		t.Error("an invalid --config file must fail before anything is stopped")
	}
	if err := preflight(t.TempDir(), "OneDriveHelper.exe", ""); err == nil {
		t.Error("missing agent must fail")
	}
	if err := preflight(src, "OneDriveHelper.exe", ""); err != nil {
		t.Error(err)
	}
}
