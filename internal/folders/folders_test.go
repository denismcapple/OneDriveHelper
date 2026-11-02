package folders

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

// testHive writes a User Shell Folders key under a throwaway HKCU key.
func testHive(t *testing.T, values map[string]string) Hive {
	t.Helper()
	base := fmt.Sprintf(`Software\OneDriveHelperTest-%d\`, time.Now().UnixNano())
	k, _, err := registry.CreateKey(registry.CURRENT_USER, base+userShellFolders, registry.ALL_ACCESS)
	if err != nil {
		t.Fatal(err)
	}
	for n, v := range values {
		if err := k.SetExpandStringValue(n, v); err != nil {
			t.Fatal(err)
		}
	}
	k.Close()
	t.Cleanup(func() { _ = registry.DeleteKey(registry.CURRENT_USER, base+userShellFolders); deleteTree(base) })
	return Hive{Root: registry.CURRENT_USER, Prefix: base}
}

func deleteTree(base string) {
	parts := strings.Split(strings.TrimSuffix(userShellFolders, `\`), `\`)
	for i := len(parts) - 1; i >= 0; i-- {
		_ = registry.DeleteKey(registry.CURRENT_USER, base+strings.Join(parts[:i], `\`))
	}
}

// The values Windows kept for a real user after their redirection policy was
// removed ("leave the folder in the new location").
func TestRedirectedRealUser(t *testing.T) {
	profile := `C:\Users\jsmith`
	h := testHive(t, map[string]string{
		"AppData":                                `%USERPROFILE%\AppData\Roaming`,
		"Favorites":                              `C:\Users\jsmith\Favorites`,
		"My Music":                               `%USERPROFILE%\Music`,
		"Desktop":                                `\\fs01\users\jsmith\Desktop`,
		"My Pictures":                            `\\fs01\users\jsmith\Pictures`,
		"Personal":                               `\\fs01\users\jsmith\Documents`,
		"{F42EE2D3-909F-4907-8871-4C22FC0BF756}": `\\fs01\users\jsmith\Documents`,
		"{0DDD015D-B06C-45D5-8C4C-F59713854639}": `\\fs01\users\jsmith\Pictures`,
		"{374DE290-123F-4565-9164-39C4925E467B}": `%USERPROFILE%\Downloads`,
		"Something Else":                         `\\fs01\users\jsmith\Other`,
	})
	got, err := Redirected(h, profile)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range got {
		names = append(names, f.Value)
	}
	want := "Desktop,Personal,My Pictures,{F42EE2D3-909F-4907-8871-4C22FC0BF756},{0DDD015D-B06C-45D5-8C4C-F59713854639},Something Else"
	if strings.Join(names, ",") != want {
		t.Fatalf("redirected:\n got %s\nwant %s", strings.Join(names, ","), want)
	}
	if got[0].Name != "Desktop" || got[0].Current != `\\fs01\users\jsmith\Desktop` || got[0].Default(profile) != `C:\Users\jsmith\Desktop` || got[0].ID == "" {
		t.Errorf("desktop: %+v", got[0])
	}
	if a := got[3]; a.ID != "" || a.Rel != "Documents" || !a.Known() {
		t.Errorf("the Documents alias follows Documents, set in the registry: %+v", a)
	}
	if u := got[5]; u.Known() {
		t.Errorf("a folder the tool doesn't know is left alone: %+v", u)
	}
}

func TestRedirectedNothing(t *testing.T) {
	h := testHive(t, map[string]string{"Desktop": `%USERPROFILE%\Desktop`, "Personal": `C:\Users\jsmith\OneDrive - Contoso\Documents`})
	if got, err := Redirected(h, `C:\Users\jsmith`); err != nil || len(got) != 0 {
		t.Errorf("folders in the profile (including in OneDrive) are not redirected: %+v %v", got, err)
	}
}

// PolicyGPOs reads this PC's real Group Policy data (read only).
func TestPolicyGPOsLive(t *testing.T) {
	names, err := PolicyGPOs("")
	if err != nil {
		t.Skipf("no Group Policy data here: %v", err)
	}
	t.Logf("Folder Redirection GPOs applied to this user: %q", names)
}

func TestShortcutFile(t *testing.T) {
	for in, want := range map[string]string{"Network Drive": "Network Drive.lnk", ` U: drive? `: "U drive.lnk", "...": "", "": ""} {
		if got := ShortcutFile(in); got != want {
			t.Errorf("ShortcutFile(%q) = %q, want %q", in, got, want)
		}
	}
}

// A real shortcut, to a folder that exists and to a drive that may not.
func TestCreateShortcut(t *testing.T) {
	dir := t.TempDir()
	for name, target := range map[string]string{"Here.lnk": dir, "Network Drive.lnk": `U:\`} {
		lnk := filepath.Join(dir, name)
		if err := CreateShortcut(lnk, target, "test"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if fi, err := os.Stat(lnk); err != nil || fi.Size() == 0 {
			t.Errorf("%s not written: %v", name, err)
		}
		if err := CreateShortcut(lnk, target, "again"); err != nil {
			t.Errorf("%s: replacing it: %v", name, err)
		}
	}
	if got := ExpandTarget(`\\fs01\users\%USERNAME%`); strings.Contains(got, "%") || !strings.HasSuffix(got, os.Getenv("USERNAME")) {
		t.Errorf("expand: %q", got)
	}
}

func TestRestartExplorerStartsOnlyIfNeeded(t *testing.T) {
	saved := startExplorer
	defer func() { startExplorer = saved }()
	started := 0
	startExplorer = func() error { started++; return nil }
	if err := RestartExplorer(nil, func() bool { return true }); err != nil || started != 0 {
		t.Errorf("Windows restarted the shell: don't open another (started %d)", started)
	}
	if err := RestartExplorer(nil, func() bool { return false }); err != nil || started != 1 {
		t.Errorf("no shell after the wait: start it (started %d)", started)
	}
}
