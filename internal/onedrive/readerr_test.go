package onedrive

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// testHive returns a hive rooted at a throwaway HKCU key, deleted afterwards.
func testHive(t *testing.T) Hive {
	t.Helper()
	base := fmt.Sprintf(`Software\OneDriveHelperTest-%d`, time.Now().UnixNano())
	k, _, err := registry.CreateKey(registry.CURRENT_USER, base, registry.ALL_ACCESS)
	if err != nil {
		t.Fatal(err)
	}
	k.Close()
	t.Cleanup(func() { deleteTestKey(t, base) })
	return Hive{Root: registry.CURRENT_USER, Prefix: base + `\`, SID: CurrentUserHive().SID}
}

func deleteTestKey(t *testing.T, path string) {
	// Restore access first (the owner can always change the DACL), then delete.
	if k, err := registry.OpenKey(registry.CURRENT_USER, path, windows.WRITE_DAC); err == nil {
		setKeyDACL(k, "D:(A;OICI;KA;;;WD)")
		k.Close()
	}
	if k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.ENUMERATE_SUB_KEYS); err == nil {
		subs, _ := k.ReadSubKeyNames(-1)
		k.Close()
		for _, s := range subs {
			deleteTestKey(t, path+`\`+s)
		}
	}
	_ = registry.DeleteKey(registry.CURRENT_USER, path)
}

func setKeyDACL(k registry.Key, sddl string) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, _ := sd.DACL()
	return windows.SetSecurityInfo(windows.Handle(k), windows.SE_REGISTRY_KEY,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

func TestGatherReportsUnreadableAccounts(t *testing.T) {
	h := testHive(t)
	k, err := h.Create(accountsPath)
	if err != nil {
		t.Fatal(err)
	}
	k.Close()
	k, err = h.Open(accountsPath, windows.WRITE_DAC)
	if err != nil {
		t.Fatal(err)
	}
	if err := setKeyDACL(k, "D:P(A;;KA;;;SY)"); err != nil { // only SYSTEM may read it now
		t.Fatal(err)
	}
	k.Close()

	in := Gather(h, t.TempDir())
	if len(in.ReadErrors) == 0 {
		t.Fatal("expected a read error for the Accounts key")
	}
	if !in.IsActive(`C:\Users\jsmith\Anything`) {
		t.Error("with a read error every path must count as active")
	}
	if got := StaleNavEntries(h, in, func(string) bool { return false }); got != nil {
		t.Errorf("no Explorer entries may be removed on incomplete data: %v", got)
	}
}

func TestGatherEmptyHiveIsNotAnError(t *testing.T) {
	h := testHive(t) // no OneDrive keys at all: a user who never used OneDrive
	in := Gather(h, t.TempDir())
	for _, e := range in.ReadErrors {
		if len(e) > 0 && !containsHKLM(e) {
			t.Errorf("missing keys must not be read errors: %v", in.ReadErrors)
		}
	}
	if len(in.Accounts) != 0 {
		t.Errorf("accounts: %+v", in.Accounts)
	}
	in.ReadErrors = nil // HKLM SyncRootManager is outside this test's control
	if in.IsActive(`C:\Users\jsmith\Contoso\Lib`) {
		t.Error("nothing is active for an empty hive")
	}
}

// Another user's sync root is readable only by that user (and SYSTEM). It
// must be skipped, not reported as a read error that blocks every fix.
func TestSyncRootsReadOnlyTheUsersOwn(t *testing.T) {
	lm := testHive(t)
	const me, other = "S-1-12-1-1857092749-1296937855-458039468-513266021", "S-1-12-1-1819264306-1243430638-1872799150-834376774"
	add := func(name, folder string) registry.Key {
		k, err := lm.Create(syncRootPath + `\` + name + `\UserSyncRoots`)
		if err != nil {
			t.Fatal(err)
		}
		if err := k.SetStringValue(me, folder); err != nil {
			t.Fatal(err)
		}
		return k
	}
	add("OneDrive!"+me+"!Business1|4b48901b+1", `C:\Users\me\Contoso\Projects - Documents`).Close()
	add("onedrive!"+strings.ToLower(me)+"!Personal|abc", `C:\Users\me\OneDrive`).Close()
	add("OneDrive!"+me+"0!Business1|aaa", `C:\Users\someone\Contoso\Lib`).Close() // a longer SID, not mine
	add("OneDriveOther!"+me+"!x", `C:\Users\me\NotOneDrive`).Close()
	k := add("OneDrive!"+other+"!Business1|38305ef2", `C:\Users\other\Contoso\Lib`)
	k.Close()
	k, err := lm.Open(syncRootPath+`\OneDrive!`+other+`!Business1|38305ef2\UserSyncRoots`, windows.WRITE_DAC)
	if err != nil {
		t.Fatal(err)
	}
	if err := setKeyDACL(k, "D:P(A;;KA;;;SY)"); err != nil { // only SYSTEM may read it now
		t.Fatal(err)
	}
	k.Close()

	r := &readErrs{}
	got := registeredSyncRoots(lm, me, r)
	if len(r.list) > 0 {
		t.Errorf("another user's sync root must not be a read error: %v", r.list)
	}
	want := []string{`C:\Users\me\Contoso\Projects - Documents`, `C:\Users\me\OneDrive`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("sync roots = %q, want %q", got, want)
	}

	// The other user's own (unreadable) key is still an error for them.
	r = &readErrs{}
	registeredSyncRoots(lm, other, r)
	if len(r.list) == 0 {
		t.Error("an unreadable key of the target user must be a read error")
	}
	// Without a SID nothing can be trusted.
	r = &readErrs{}
	if got := registeredSyncRoots(lm, "", r); got != nil || len(r.list) == 0 {
		t.Errorf("unknown SID: got %q, errors %v", got, r.list)
	}
}

func containsHKLM(s string) bool {
	return len(s) >= len(syncRootPath) && s[:len(syncRootPath)] == syncRootPath
}

func TestGatherReadsAccountsFromTestHive(t *testing.T) {
	h := testHive(t)
	k, err := h.Create(accountsPath + `\Business1`)
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(k.SetStringValue("UserFolder", `C:\Users\jsmith\OneDrive - Contoso`))
	k.Close()
	k, err = h.Create(accountsPath + `\Business1\Tenants\Contoso`)
	must(err)
	must(k.SetStringValue(`C:\Users\jsmith\Contoso\Projects - Documents`, ""))
	k.Close()

	in := Gather(h, t.TempDir())
	var mine []string
	for _, e := range in.ReadErrors {
		if !containsHKLM(e) {
			mine = append(mine, e)
		}
	}
	if len(mine) > 0 {
		t.Fatalf("unexpected read errors: %v", mine)
	}
	in.ReadErrors = nil
	if !in.IsActive(`C:\Users\jsmith\Contoso\Projects - Documents`) || in.IsActive(`C:\Users\jsmith\Contoso\Old - Documents`) {
		t.Errorf("active paths: %v", in.ActivePaths())
	}
}
