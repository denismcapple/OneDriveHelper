package onedrive

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denismcapple/OneDriveHelper/internal/fsutil"
)

const sampleLine = `libraryScope = 2 aaaaaaaabbbb4cccdddd111122223333+2 5 "Internal" "Documents" 4 "https://contoso.sharepoint.com/sites/internal" "11111111-2222-4333-8444-555555555555" 0123456789ab4cdef012345678901234 fedcba98765443210fedcba987654321 13572468abcd4ef0a1b2c3d4e5f60718 1790167875 "C:\Users\jsmith\Contoso\Internal - Documents" 1 22222222-3333-4444-8555-666666666666  - 1407374883554875 24914674`

func TestParseLibraryScope(t *testing.T) {
	l, ok := ParseLibraryScope(sampleLine)
	if !ok {
		t.Fatal("not parsed")
	}
	want := Library{
		ScopeID: "aaaaaaaabbbb4cccdddd111122223333+2", SiteTitle: "Internal", ListTitle: "Documents",
		WebURL: "https://contoso.sharepoint.com/sites/internal", TenantID: "11111111-2222-4333-8444-555555555555",
		SiteID: "0123456789ab4cdef012345678901234", WebID: "fedcba98765443210fedcba987654321",
		ListID: "13572468abcd4ef0a1b2c3d4e5f60718", MountPoint: `C:\Users\jsmith\Contoso\Internal - Documents`,
	}
	if l != want {
		t.Fatalf("got %+v\nwant %+v", l, want)
	}
}

func TestParseLibraryScopeSkipsPersonalAndJunk(t *testing.T) {
	personal := `libraryScope = 0 99998888777746665555444433332222 5 "MySite" "ODB" 2 "https://contoso-my.sharepoint.com/personal/x" "11111111-2222-4333-8444-555555555555" abababababab4abaabababababababab cdcdcdcdcdcd4cdccdcdcdcdcdcdcdcd efefefefefef4efeefefefefefefefef 1787659133 "C:\Users\x\OneDrive - Contoso" 1`
	for _, s := range []string{personal, "libraryScope = 1 2 3", "installID = 1", ""} {
		if _, ok := ParseLibraryScope(s); ok {
			t.Errorf("should not parse: %q", s)
		}
	}
}

func TestTokenizeEmptyQuoted(t *testing.T) {
	got := tokenize(`a "" "b c" d`)
	if strings.Join(got, "|") != "a||b c|d" {
		t.Fatalf("got %q", got)
	}
}

func TestResyncURL(t *testing.T) {
	l, _ := ParseLibraryScope(sampleLine)
	l.UserEmail = "jsmith@contoso.com"
	u := l.ResyncURL()
	for _, want := range []string{
		"odopen://sync/?",
		"siteId=%7B01234567-89AB-4CDE-F012-345678901234%7D",
		"webId=%7BFEDCBA98-7654-4321-0FED-CBA987654321%7D",
		"listId=%7B13572468-ABCD-4EF0-A1B2-C3D4E5F60718%7D",
		"userEmail=jsmith%40contoso.com",
		"webUrl=https%3A%2F%2Fcontoso.sharepoint.com%2Fsites%2Finternal",
		"listTitle=Documents",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("missing %q in %s", want, u)
		}
	}
	if strings.Contains(u, "+") {
		t.Errorf("spaces must be %%20 not +: %s", u)
	}
}

func TestDecodeText(t *testing.T) {
	utf16le := []byte{'h', 0, 'i', 0, '\n', 0}
	if got := decodeText(utf16le); got != "hi\n" {
		t.Errorf("utf16 no BOM: %q", got)
	}
	if got := decodeText(append([]byte{0xFF, 0xFE}, utf16le...)); got != "hi\n" {
		t.Errorf("utf16 BOM: %q", got)
	}
	if got := decodeText([]byte("hi\n")); got != "hi\n" {
		t.Errorf("utf8: %q", got)
	}
}

// fakeFS is an in-memory directory tree keyed by lower-case path.
type fakeFS struct {
	dirs  map[string][]fsutil.Entry
	files map[string]string
}

func (f fakeFS) ReadDir(d string) ([]fsutil.Entry, error) {
	if e, ok := f.dirs[Clean(d)]; ok {
		return e, nil
	}
	return nil, os.ErrNotExist
}

func (f fakeFS) ReadFile(p string) ([]byte, error) {
	if s, ok := f.files[Clean(p)]; ok {
		return []byte(s), nil
	}
	return nil, errors.New("not found")
}

func dir(name string) fsutil.Entry { return fsutil.Entry{Name: name, Attr: fsutil.AttrDirectory} }
func cloud(name string) fsutil.Entry {
	return fsutil.Entry{Name: name, Attr: fsutil.AttrDirectory | fsutil.AttrReparsePoint, Tag: 0x9000701A}
}

func TestFindOrphans(t *testing.T) {
	profile := `C:\Users\jsmith`
	org := filepath.Join(profile, "Contoso")
	guest := filepath.Join(profile, "Fabrikam")
	fs := fakeFS{
		dirs: map[string][]fsutil.Entry{
			Clean(profile): {
				dir("Contoso"), dir("Fabrikam"), dir("Documents"),
				{Name: "OneDrive - Contoso", Attr: fsutil.AttrDirectory | fsutil.AttrReparsePoint, Tag: 0x9000701A},
			},
			Clean(org): {
				cloud("Internal - Documents"),                      // active
				cloud("Old - Documents"),                           // orphan (cloud reparse)
				dir("History - Documents"),                         // orphan (known from history, no reparse)
				dir("User made folder"),                            // orphan: plain folder in an OneDrive org folder
				{Name: "Hidden", Attr: fsutil.AttrDirectory | 0x6}, // hidden+system: ignored
				{Name: "Link", Attr: fsutil.AttrDirectory | fsutil.AttrReparsePoint, Tag: fsutil.TagMountPoint}, // junction: ignored
				{Name: "desktop.ini", Attr: 0x6},
			},
			Clean(guest): {cloud("Guest - Documents")}, // tenant root found via desktop.ini only
		},
		files: map[string]string{
			Clean(filepath.Join(org, "desktop.ini")):   "[.ShellClassInfo]\r\nIconResource=C:\\Program Files\\Microsoft OneDrive\\OneDrive.exe,6",
			Clean(filepath.Join(guest, "desktop.ini")): "[.ShellClassInfo]\r\nIconResource=C:\\Program Files\\Microsoft OneDrive\\OneDrive.exe,6",
		},
	}
	in := Inputs{
		ProfileDir: profile,
		Accounts: []Account{{
			Key: "Business1", UserEmail: "d@contoso.com", UserFolder: filepath.Join(profile, "OneDrive - Contoso"),
			SyncedFolders: []string{filepath.Join(org, "Internal - Documents"), filepath.Join(profile, "OneDrive - Contoso")},
		}},
		Providers: []Provider{
			{MountPoint: filepath.Join(org, "History - Documents"), WebURL: "https://contoso.sharepoint.com/sites/history", LibraryType: "teamsite"},
			{MountPoint: filepath.Join(profile, "OneDrive - Contoso"), LibraryType: "personal"},
		},
		Known: []Library{{MountPoint: filepath.Join(org, "Old - Documents"), UserEmail: "d@contoso.com", SiteID: "x", ListID: "y", WebURL: "https://contoso.sharepoint.com/sites/old"}},
	}
	got := FindOrphans(in, fs)
	var names []string
	for _, o := range got {
		names = append(names, o.Name)
	}
	// Sorted by full path: Contoso\History, Contoso\Old, Fabrikam\Guest.
	// "User made folder" counts too: it sits in an OneDrive organisation folder but isn't synced.
	if strings.Join(names, ",") != "History - Documents,Old - Documents,User made folder,Guest - Documents" {
		t.Fatalf("orphans = %v", names)
	}
	byName := map[string]Orphan{}
	for _, o := range got {
		byName[o.Name] = o
	}
	if !byName["Old - Documents"].CanResync() {
		t.Error("Old - Documents should be re-syncable from cache")
	}
	if byName["History - Documents"].WebURL != "https://contoso.sharepoint.com/sites/history" || byName["History - Documents"].CanResync() {
		t.Error("History - Documents should have only a web URL")
	}
}

// FindOrphans is a pure function: it still reports these folders even when
// the inputs carry read errors or OneDrive is starting up. Refusing to act on
// such data happens in remediate.BuildPlan (RefusedError, event 5013).
func TestFindOrphansEmptyAndPlainOrgFolders(t *testing.T) {
	profile := `C:\Users\jsmith`
	ini := "[.ShellClassInfo]\r\nIconResource=C:\\Program Files\\Microsoft OneDrive\\OneDrive.exe,6"
	empty, ey, other := filepath.Join(profile, "OLD COMPANY LTD"), filepath.Join(profile, "Fabrikam"), filepath.Join(profile, "Projects")
	fs := fakeFS{
		dirs: map[string][]fsutil.Entry{
			Clean(profile): {dir("OLD COMPANY LTD"), dir("Fabrikam"), dir("Projects")},
			Clean(empty):   {{Name: "desktop.ini", Attr: 0x6}},
			Clean(ey):      {{Name: "desktop.ini", Attr: 0x6}, dir("Bids Team - Documents")}, // lost its placeholder attributes
			Clean(other):   {dir("Not OneDrive")},                                            // no OneDrive desktop.ini
		},
		files: map[string]string{
			Clean(filepath.Join(empty, "desktop.ini")): ini,
			Clean(filepath.Join(ey, "desktop.ini")):    ini,
		},
	}
	got := FindOrphans(Inputs{ProfileDir: profile}, fs)
	if len(got) != 2 {
		t.Fatalf("orphans: %+v", got)
	}
	if got[0].Name != "Bids Team - Documents" || got[0].EmptyOrg || got[0].DisplayName() != "Bids Team - Documents (Fabrikam)" {
		t.Errorf("Fabrikam library: %+v", got[0])
	}
	if got[1].Path != empty || !got[1].EmptyOrg || got[1].CanResync() {
		t.Errorf("empty org folder: %+v", got[1])
	}

	// An org folder that still contains an active library is never "empty".
	fs.dirs[Clean(empty)] = []fsutil.Entry{{Name: "desktop.ini", Attr: 0x6}}
	in := Inputs{ProfileDir: profile, Accounts: []Account{{Key: "Business1", SyncedFolders: []string{filepath.Join(empty, "Live - Documents")}}}}
	for _, o := range FindOrphans(in, fs) {
		if o.EmptyOrg {
			t.Errorf("org folder with an active library flagged: %+v", o)
		}
	}
}

func TestFindOrphansPersonalAndKnownFolders(t *testing.T) {
	profile := `C:\Users\jsmith`
	live, old := filepath.Join(profile, "OneDrive - Contoso"), filepath.Join(profile, "OneDrive - OLD COMPANY")
	odIni := "[.ShellClassInfo]\r\nIconResource=C:\\Program Files\\Microsoft OneDrive\\OneDrive.exe,5"
	fs := fakeFS{
		dirs: map[string][]fsutil.Entry{
			Clean(profile): {cloud("OneDrive - Contoso"), cloud("OneDrive - OLD COMPANY"), dir("OneDrive notes")},
			Clean(old):     {dir("Documents")},
		},
		files: map[string]string{Clean(filepath.Join(old, "desktop.ini")): odIni},
	}
	in := Inputs{ProfileDir: profile, Accounts: []Account{{Key: "Business1", UserFolder: live, SyncedFolders: []string{live}}}}
	got := FindOrphans(in, fs)
	if len(got) != 1 || got[0].Path != old || !got[0].Personal || got[0].DisplayName() != "OneDrive - OLD COMPANY (personal OneDrive folder)" {
		t.Fatalf("expected only the old personal folder (not the live one or a plain 'OneDrive notes' folder): %+v", got)
	}
	if len(got[0].KnownFolders) != 0 {
		t.Errorf("no known folders expected: %v", got[0].KnownFolders)
	}
	in.ShellFolders = map[string]string{"Personal": filepath.Join(old, "Documents"), "Desktop": filepath.Join(live, "Desktop")}
	got = FindOrphans(in, fs)
	if len(got) != 1 || strings.Join(got[0].KnownFolders, ",") != "Documents" {
		t.Errorf("Documents redirected into the orphan must be reported: %+v", got)
	}
}

func TestWebURLOnlyHTTPS(t *testing.T) {
	for in, want := range map[string]string{
		"https://contoso.sharepoint.com/sites/x": "https://contoso.sharepoint.com/sites/x",
		" HTTPS://contoso.sharepoint.com ":       "HTTPS://contoso.sharepoint.com",
		`C:\Windows\System32\calc.exe`:           "",
		"file:///C:/evil.exe":                    "",
		"http://contoso.sharepoint.com":          "",
		`https://x" & calc`:                      "",
	} {
		if got := webURL(in); got != want {
			t.Errorf("webURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStaleNavEntries(t *testing.T) {
	entries := []NavEntry{
		{CLSID: "{04271989-C4D2-1}", Name: "Gone", Target: `C:\Users\d\Gone`},
		{CLSID: "{04271989-C4D2-2}", Name: "Here", Target: `C:\Users\d\Here`},
		{CLSID: "{04271989-C4D2-3}", Name: "Active but missing", Target: `C:\Users\d\Org`},
		{CLSID: "{04271989-C4D2-4}", Name: "No target"},
	}
	exists := func(p string) bool { return p == `C:\Users\d\Here` }
	got := staleFrom(entries, cleanAll([]string{`C:\Users\d\Org\Lib - Documents`}), exists)
	if len(got) != 1 || got[0].Name != "Gone" {
		t.Errorf("stale: %+v", got)
	}
}

func TestFindOrphansNeverScansProfileOrDriveRoot(t *testing.T) {
	profile := `C:\Users\jsmith`
	fs := fakeFS{dirs: map[string][]fsutil.Entry{
		Clean(profile): {cloud("Something")},
		`c:\`:          {cloud("Users")},
	}}
	in := Inputs{
		ProfileDir: profile,
		Providers: []Provider{
			{MountPoint: filepath.Join(profile, "Bogus"), LibraryType: "teamsite"}, // parent == profile
			{MountPoint: `C:\Bogus`, LibraryType: "teamsite"},                      // parent == drive root
		},
	}
	if got := FindOrphans(in, fs); len(got) != 0 {
		t.Fatalf("unexpected orphans %v", got)
	}
}

func TestClientPolicyFallback(t *testing.T) {
	text := "DavUrlNamespace = https://contoso.sharepoint.com/sites/Old/Shared Documents/\r\n" +
		"SkyDriveURI = https://contoso.sharepoint.com/sites/Old/_api/SPFileSync/sync/c0ffee00c0ffee00c0ffee00c0ffee00/RootFolder\r\n" +
		"SiteTitle = Old Site\r\nLibraryTitle = Documents\r\n" +
		"WebID = {BEEFBEEF-BEEF-4BEE-BEEF-BEEFBEEFBEEF}\r\nSiteID = {5EED5EED-5EED-5EED-5EED-5EED5EED5EED}\r\n"
	pol, ok := parseClientPolicy("ClientPolicy_c0ffee00c0ffee00c0ffee00c0ffee00_5eed5eed5eed5eed5eed5eed5eed5eed.ini", text)
	if !ok || pol.ListID != "c0ffee00c0ffee00c0ffee00c0ffee00" || pol.SiteTitle != "Old Site" {
		t.Fatalf("parse: %v %+v", ok, pol)
	}
	pol.UserEmail = "d@contoso.com"

	profile := `C:\Users\jsmith`
	org := filepath.Join(profile, "Contoso")
	fs := fakeFS{dirs: map[string][]fsutil.Entry{Clean(org): {cloud("Old Site - Documents")}}}
	in := Inputs{
		ProfileDir: profile,
		Providers: []Provider{{
			MountPoint: filepath.Join(org, "Old Site - Documents"), LibraryType: "teamsite",
			URLNamespace: "https://contoso.sharepoint.com/sites/Old/Shared Documents", // no trailing slash
			WebURL:       "https://contoso.sharepoint.com/sites/Old",
		}},
		Policies: []Library{pol},
	}
	got := FindOrphans(in, fs)
	if len(got) != 1 || !got[0].CanResync() {
		t.Fatalf("expected a re-syncable orphan: %+v", got)
	}
	if u := got[0].Library.ResyncURL(); !strings.Contains(u, "listId=%7BC0FFEE00-C0FF-EE00-C0FF-EE00C0FFEE00%7D") || !strings.Contains(u, "webUrl=https%3A%2F%2Fcontoso.sharepoint.com%2Fsites%2FOld") {
		t.Errorf("resync url: %s", u)
	}
}

func TestIsActive(t *testing.T) {
	in := Inputs{Accounts: []Account{{SyncedFolders: []string{`C:\Users\d\Org\Lib - Documents`}}}}
	for p, want := range map[string]bool{
		`C:\Users\d\Org\Lib - Documents`:     true,
		`c:\users\d\org\lib - documents\sub`: true,
		`C:\Users\d\Org`:                     true, // contains an active library
		`C:\Users\d\Org\Lib - Documents 2`:   false,
		`C:\Users\d\Org\Other - Documents`:   false,
	} {
		if got := in.IsActive(p); got != want {
			t.Errorf("IsActive(%s) = %v, want %v", p, got, want)
		}
	}
}
