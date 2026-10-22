package remediate

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/denismcapple/OneDriveHelper/internal/elog"
	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

func TestClassifyMoveErr(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want any // errBusy or errCloud
	}{
		{"access denied", windows.ERROR_ACCESS_DENIED, errBusy{}},
		{"sharing violation", windows.ERROR_SHARING_VIOLATION, errBusy{}},
		{"provider not running", errnoCloudProviderNotRunning, errCloud{}},
		{"cloud access denied", errnoCloudAccessDenied, errCloud{}},
		{"other", windows.ERROR_FILE_NOT_FOUND, nil},
	} {
		err := classifyMoveErr(c.err)
		var b errBusy
		var cl errCloud
		switch c.want.(type) {
		case errBusy:
			if !errors.As(err, &b) {
				t.Errorf("%s: %v should classify as busy", c.name, c.err)
			}
		case errCloud:
			if !errors.As(err, &cl) {
				t.Errorf("%s: %v should classify as cloud", c.name, c.err)
			}
		default:
			if errors.As(err, &b) || errors.As(err, &cl) {
				t.Errorf("%s: %v should pass through unchanged", c.name, c.err)
			}
		}
	}
}

// --- OneDrive's read-only lock (libraries the user can only read) ---

func icacls(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("icacls", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("icacls %v: %v %s", args, err, out)
	}
	return string(out)
}

// addDeny adds an explicit deny entry for sid with exactly mask (icacls
// /deny would add SYNCHRONIZE too, which OneDrive's entries don't have).
func addDeny(t *testing.T, path, sid string, mask uint32) {
	t.Helper()
	s, err := windows.StringToSid(sid)
	must(t, err)
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	must(t, err)
	dacl, _, err := sd.DACL()
	must(t, err)
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.ACCESS_MASK(mask), AccessMode: windows.DENY_ACCESS, Inheritance: windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_WELL_KNOWN_GROUP, TrusteeValue: windows.TrusteeValueFromSID(s)},
	}}, dacl)
	must(t, err)
	must(t, windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil))
}

const oneDriveDeny = windows.DELETE | windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | fileDeleteChild

// lockReadOnly applies what OneDrive puts on copies from a read-only
// library: an explicit Everyone:(DENY)(D,WD,AD,DC) on every file and folder.
func lockReadOnly(t *testing.T, dir string) {
	t.Helper()
	must(t, filepath.Walk(dir, func(p string, _ os.FileInfo, err error) error {
		if err == nil {
			addDeny(t, p, "S-1-1-0", oneDriveDeny)
		}
		return err
	}))
	t.Cleanup(func() { exec.Command("icacls", dir, "/remove:d", "*S-1-1-0", "/T", "/C", "/Q").Run() })
}

func TestUnlockReadOnly(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.pdf")
	must(t, os.WriteFile(f, []byte("x"), 0o644))
	lockReadOnly(t, dir)
	addDeny(t, f, "S-1-5-32-546", windows.DELETE) // Guests: not OneDrive's, must stay
	t.Cleanup(func() { exec.Command("icacls", f, "/remove:d", "*S-1-5-32-546").Run() })
	if err := os.Remove(f); err == nil {
		t.Fatal("the deny entry should block deleting")
	}
	rec := record{"": {Kind: kindDir}, "a.pdf": {Kind: kindFile}}
	n, errs := unlockReadOnly(dir, rec)
	if n != 2 || len(errs) != 0 {
		t.Fatalf("unlocked %d, errors %v; want the folder and the file", n, errs)
	}
	acl := icacls(t, f)
	if strings.Contains(acl, "Everyone:(DENY)") || !strings.Contains(acl, "Guests:(DENY)") {
		t.Errorf("only OneDrive's deny entry may be removed:\n%s", acl)
	}
	if n, _ := unlockReadOnly(dir, rec); n != 0 {
		t.Errorf("a second pass has nothing to unlock, got %d", n)
	}
}

func TestExecuteReadOnlyLibraryCopies(t *testing.T) {
	_, rec := testEnv(t)
	f := newFixture(t, three)
	lockReadOnly(t, f.lib())
	o := run1(t, f).Outcomes[0]
	if !o.Removed || o.State != StateDone || exists(f.lib()) {
		t.Fatalf("read-only copies should be removed after the backup: %+v", o)
	}
	if got := readAll(t, o.SavedTo); len(got) != 3 {
		t.Errorf("backup: %v", got)
	}
	if !rec.has(elog.RemediationCompleted) {
		t.Errorf("events: %v", rec.events)
	}
}

func TestCheckForDeleteWording(t *testing.T) {
	testEnv(t)
	f := newFixture(t, map[string]string{"a.txt": "a"})
	rec, st, _ := copyRecorded(f.lib(), filepath.Join(t.TempDir(), "b"))
	if len(st.Errors) > 0 {
		t.Fatal(st.Errors)
	}
	lockReadOnly(t, f.lib())
	if _, why := checkForDelete(f.lib(), rec); !strings.Contains(why, "would not allow") || strings.Contains(why, "open in another program") {
		t.Errorf("a permission refusal must not be called an open file: %q", why)
	}
	holdOpen(t, filepath.Join(f.lib(), "a.txt"), windows.GENERIC_READ, windows.FILE_SHARE_READ)
	exec.Command("icacls", f.lib(), "/remove:d", "*S-1-1-0", "/T", "/C", "/Q").Run()
	if _, why := checkForDelete(f.lib(), rec); !strings.Contains(why, "open in another program") {
		t.Errorf("a file held open should be reported as open: %q", why)
	}
}

// --- dead sync roots ---

var procCfRegisterSyncRoot = windows.NewLazySystemDLL("cldapi.dll").NewProc("CfRegisterSyncRoot")

type cfSyncRegistration struct {
	StructSize             uint32
	ProviderName           *uint16
	ProviderVersion        *uint16
	SyncRootIdentity       unsafe.Pointer
	SyncRootIdentityLength uint32
	FileIdentity           unsafe.Pointer
	FileIdentityLength     uint32
	ProviderID             windows.GUID
}

type cfSyncPolicies struct {
	StructSize                             uint32
	HydrationPrimary, HydrationModifier    uint16
	PopulationPrimary, PopulationModifier  uint16
	InSync, HardLink, PlaceholderManagment uint32
}

// registerDeadSyncRoot makes dir a sync root of a "OneDrive" provider that
// never connects: what a library OneDrive dropped without unregistering
// looks like to Windows.
func registerDeadSyncRoot(t *testing.T, dir string) {
	t.Helper()
	if err := procCfRegisterSyncRoot.Find(); err != nil {
		t.Skipf("Cloud Files API not available: %v", err)
	}
	name, _ := windows.UTF16PtrFromString("OneDrive - Test")
	ver, _ := windows.UTF16PtrFromString("1.0")
	id := []byte("odh-test")
	reg := cfSyncRegistration{ProviderName: name, ProviderVersion: ver, SyncRootIdentity: unsafe.Pointer(&id[0]),
		SyncRootIdentityLength: uint32(len(id)), ProviderID: windows.GUID{Data1: 0x0d4e1a5e, Data2: 0x7e57, Data3: 0x4d1e}}
	reg.StructSize = uint32(unsafe.Sizeof(reg))
	pol := cfSyncPolicies{HydrationPrimary: 2, PopulationPrimary: 3} // full hydration, always-full population
	pol.StructSize = uint32(unsafe.Sizeof(pol))
	p, _ := windows.UTF16PtrFromString(dir)
	if r, _, _ := procCfRegisterSyncRoot.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&reg)), uintptr(unsafe.Pointer(&pol)), 0); r != 0 {
		t.Skipf("cannot register a test sync root: HRESULT %#x", r)
	}
	t.Cleanup(func() { _ = winapi.UnregisterSyncRoot(dir) })
}

func TestExecuteDeadSyncRoot(t *testing.T) {
	_, rec := testEnv(t)
	f := newFixture(t, three)
	registerDeadSyncRoot(t, f.lib())
	if dead, provider := deadSyncRoot(f.lib()); !dead || provider != "OneDrive - Test" {
		t.Fatalf("deadSyncRoot = %v %q", dead, provider)
	}
	if dead, _ := deadSyncRoot(filepath.Join(f.lib(), "sub")); dead {
		t.Error("a folder inside a sync root is not the root")
	}
	if err := moveByHandle(f.lib(), f.lib()+" moved"); err == nil {
		must(t, moveByHandle(f.lib()+" moved", f.lib()))
		t.Log("Windows allowed renaming a registered, disconnected sync root here")
	} else {
		t.Logf("move while registered: %v", err)
	}
	o := run1(t, f).Outcomes[0]
	if !o.Removed || o.State != StateDone || exists(f.lib()) {
		t.Fatalf("dead sync root should be unregistered and fixed: %+v", o)
	}
	if got := readAll(t, o.SavedTo); len(got) != 3 {
		t.Errorf("backup: %v", got)
	}
	if !rec.has(elog.FixDeadSyncRoot) || rec.has(elog.FixSkippedCloud) {
		t.Errorf("want 5017 and no 5016: %v", rec.events)
	}
}

func TestDeadSyncRootIgnoresPlainFolder(t *testing.T) {
	if dead, _ := deadSyncRoot(t.TempDir()); dead {
		t.Error("a plain folder is not a sync root")
	}
}
