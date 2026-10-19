package onedrive

import (
	"fmt"
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
	return Hive{Root: registry.CURRENT_USER, Prefix: base + `\`}
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
