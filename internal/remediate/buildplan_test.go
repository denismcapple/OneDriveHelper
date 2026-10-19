package remediate

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/elog"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

// withPlanEnv replaces BuildPlan's inputs for one test.
func withPlanEnv(t *testing.T, in onedrive.Inputs, state onedrive.ProcState) {
	t.Helper()
	saved := planEnv
	planEnv.gather = func(Target) onedrive.Inputs { return in }
	planEnv.procState = func(string) onedrive.ProcState { return state }
	t.Cleanup(func() { planEnv = saved })
}

// libFixture makes a profile with an org folder and one library recorded in
// OneDrive's (stale) provider history, so FindOrphans reports it.
func libFixture(t *testing.T) (profile, lib string, in onedrive.Inputs) {
	t.Helper()
	profile = t.TempDir()
	lib = filepath.Join(profile, "Contoso", "Projects - Documents")
	must(t, os.MkdirAll(lib, 0o755))
	must(t, os.WriteFile(filepath.Join(lib, "a.txt"), []byte("a"), 0o644))
	in = onedrive.Inputs{ProfileDir: profile, Providers: []onedrive.Provider{{MountPoint: lib}}}
	return
}

func plan(t *testing.T, profile string) (*Plan, error) {
	t.Helper()
	return BuildPlan(Target{UserName: "jsmith", SID: "S-1-5-21-1", ProfileDir: profile}, config.Defaults())
}

func TestPlanRefusedOnReadError(t *testing.T) {
	_, rec := testEnv(t)
	profile, _, in := libFixture(t)
	in.ReadErrors = []string{`Software\Microsoft\OneDrive\Accounts: Access is denied.`}
	withPlanEnv(t, in, onedrive.StateNotRunning)
	p, err := plan(t, profile)
	var refused *RefusedError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "Sign in to OneDrive first, or contact support") {
		t.Fatalf("want a refusal, got %v", err)
	}
	planEvents(nil, p.Target, p, err)
	if !rec.has(elog.FixRefusedData) {
		t.Error("5013 should be logged")
	}
}

func TestPlanRefusedOnShellFoldersReadError(t *testing.T) {
	testEnv(t)
	profile, _, in := libFixture(t)
	in.ReadErrors = []string{`Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders: Access is denied.`}
	withPlanEnv(t, in, onedrive.StateNotRunning)
	if _, err := plan(t, profile); err == nil {
		t.Error("without the Desktop/Documents locations the exclusion can't be applied: refuse")
	}
}

func TestPlanRefusedWhileOneDriveStartsUp(t *testing.T) {
	testEnv(t)
	profile, _, in := libFixture(t) // no accounts, no errors
	withPlanEnv(t, in, onedrive.StateRunning)
	if _, err := plan(t, profile); err == nil {
		t.Error("OneDrive running with no account visible: refuse")
	}
}

func TestPlanRefusedWhenProcessStateUnknown(t *testing.T) {
	testEnv(t)
	profile, _, in := libFixture(t)
	withPlanEnv(t, in, onedrive.StateUnknown)
	if _, err := plan(t, profile); err == nil {
		t.Error("unknown process state: refuse")
	}
}

func TestPlanNormalForSignedOutUser(t *testing.T) {
	testEnv(t)
	profile, lib, in := libFixture(t) // never had (or signed out of) a business account; OneDrive closed
	withPlanEnv(t, in, onedrive.StateNotRunning)
	p, err := plan(t, profile)
	if err != nil || len(p.Items) != 1 || p.Items[0].Orphan.Path != lib {
		t.Fatalf("plan: %+v %v", p, err)
	}
}

func TestOneDriveStateIsPerUser(t *testing.T) {
	// OneDrive in another user's session doesn't count for the target user.
	if got := onedrive.OneDriveStateForUser("S-1-5-21-1-2-3-999999"); got != onedrive.StateNotRunning {
		t.Errorf("a user with no processes: %v", got)
	}
	sid, err := winapi.CurrentUserSID()
	if err != nil {
		t.Skip(err)
	}
	mine := onedrive.OneDriveStateForUser(sid)
	if mine == onedrive.StateUnknown {
		t.Error("listing works here, so the state must be known")
	}
}

func junction(t *testing.T, link, target string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("cannot create a junction: %v %s", err, out)
	}
}

func TestPlanRefusesOrgFolderJunction(t *testing.T) {
	_, rec := testEnv(t)
	profile := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "Real")
	must(t, os.MkdirAll(filepath.Join(elsewhere, "Projects - Documents"), 0o755))
	org := filepath.Join(profile, "Contoso")
	junction(t, org, elsewhere)
	lib := filepath.Join(org, "Projects - Documents")
	withPlanEnv(t, onedrive.Inputs{ProfileDir: profile, Providers: []onedrive.Provider{{MountPoint: lib}}}, onedrive.StateNotRunning)
	p, err := plan(t, profile)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 0 || len(p.LinkRefused) != 1 || !strings.Contains(p.LinkRefused[0], org) {
		t.Fatalf("refused: %+v items %+v", p.LinkRefused, p.Items)
	}
	planEvents(nil, p.Target, p, nil)
	if !rec.has(elog.FixRefusedLink) {
		t.Error("5009 should be logged")
	}
}

func TestPlanRefusesFolderThatIsAJunction(t *testing.T) {
	testEnv(t)
	profile := t.TempDir()
	target := filepath.Join(t.TempDir(), "Important")
	must(t, os.MkdirAll(target, 0o755))
	must(t, os.WriteFile(filepath.Join(target, "keep.txt"), []byte("x"), 0o644))
	link := filepath.Join(profile, "OneDrive - Old Ltd")
	junction(t, link, target)
	withPlanEnv(t, onedrive.Inputs{ProfileDir: profile,
		Providers: []onedrive.Provider{{MountPoint: link, LibraryType: "personal"}}}, onedrive.StateNotRunning)
	p, err := plan(t, profile)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 0 || len(p.LinkRefused) != 1 {
		t.Fatalf("a folder that is itself a junction must be refused: items %+v refused %v", p.Items, p.LinkRefused)
	}
	if _, err := os.Stat(filepath.Join(target, "keep.txt")); err != nil {
		t.Error("the junction's target must be untouched")
	}
}

func TestPlanAllowsProfileThatIsAJunction(t *testing.T) {
	testEnv(t)
	real := t.TempDir()
	lib := filepath.Join(real, "Contoso", "Projects - Documents")
	must(t, os.MkdirAll(lib, 0o755))
	profile := filepath.Join(t.TempDir(), "jsmith")
	junction(t, profile, real) // e.g. a profile moved to D: with a junction
	viaLink := filepath.Join(profile, "Contoso", "Projects - Documents")
	withPlanEnv(t, onedrive.Inputs{ProfileDir: profile, Providers: []onedrive.Provider{{MountPoint: viaLink}}}, onedrive.StateNotRunning)
	p, err := plan(t, profile)
	if err != nil || len(p.Items) != 1 || len(p.LinkRefused) != 0 {
		t.Fatalf("only links below the profile count: %+v %v %v", p.Items, p.LinkRefused, err)
	}
}

func TestExecuteSkipsItemWithScanErrors(t *testing.T) {
	testEnv(t)
	f := newFixture(t, three)
	f.plan.Items[0].Stats.Errors = []string{"open folder x: Access is denied."}
	o := run1(t, f).Outcomes[0]
	if o.Removed || o.State != "" || !strings.Contains(o.Error, "could not be read; this folder will NOT be removed") {
		t.Errorf("outcome: %+v", o)
	}
	if len(readAll(t, f.lib())) != 3 {
		t.Error("folder must not be moved or removed")
	}
}

func TestExecuteRefusesJunctionInPath(t *testing.T) {
	_, rec := testEnv(t)
	f := newFixture(t, three)
	// The org folder became a junction after the plan was made.
	real := f.org + "-real"
	must(t, os.Rename(f.org, real))
	junction(t, f.org, real)
	o := run1(t, f).Outcomes[0]
	if o.Removed || !strings.Contains(o.Error, "junction") || !rec.has(elog.FixRefusedLink) {
		t.Errorf("outcome %+v events %v", o, rec.events)
	}
	if len(readAll(t, filepath.Join(real, "Projects - Documents"))) != 3 {
		t.Error("nothing may be moved through the junction")
	}
}
