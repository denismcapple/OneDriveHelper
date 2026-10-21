package onedrive

import (
	"path/filepath"
	"testing"

	"github.com/denismcapple/OneDriveHelper/internal/fsutil"
)

// The organisation was renamed: OneDrive now syncs Internal and External into
// "Everything IT", and the old "EVERYTHING COMPUTER SYSTEMS LIMITED" folder
// still holds old copies of them, plus Directors, which isn't synced anywhere.
func TestClassifyRenamedOrganisation(t *testing.T) {
	const (
		tenant  = "956a401c-dc69-43fe-9af8-b4c3f4d898c1"
		list    = "846c226d9e36484a98cdd4cd359a3c3e" // the default library's list ID is the same on every site
		siteInt = "191bde58367648a8a7b5bcabe6001e38"
		siteDir = "aaaabbbbccccddddeeeeffff00001111"
	)
	profile := `C:\Users\casey`
	oldOrg, newOrg, other := filepath.Join(profile, "EVERYTHING COMPUTER SYSTEMS LIMITED"), filepath.Join(profile, "Everything IT"), filepath.Join(profile, "Northwind")
	ini := "[.ShellClassInfo]\r\nIconResource=C:\\Program Files\\Microsoft OneDrive\\OneDrive.exe,6"
	desktopIni := fsutil.Entry{Name: "desktop.ini", Attr: 0x6}
	fs := fakeFS{
		dirs: map[string][]fsutil.Entry{
			Clean(profile): {dir("EVERYTHING COMPUTER SYSTEMS LIMITED"), dir("Everything IT"), dir("Northwind")},
			Clean(oldOrg): {desktopIni, cloud("Internal - Documents"), cloud("External - Documents"), cloud("EIT Directors - Documents"),
				dir("Notes")},
			Clean(newOrg): {desktopIni, cloud("Internal - Documents"), cloud("External - Documents")},
			Clean(other):  {desktopIni, cloud("Shared - Documents")},
		},
		files: map[string]string{
			Clean(filepath.Join(oldOrg, "desktop.ini")): ini,
			Clean(filepath.Join(newOrg, "desktop.ini")): ini,
			Clean(filepath.Join(other, "desktop.ini")):  ini,
		},
	}
	newInt, newExt := filepath.Join(newOrg, "Internal - Documents"), filepath.Join(newOrg, "External - Documents")
	in := Inputs{
		ProfileDir: profile,
		Accounts: []Account{{Key: "Business1", UserEmail: "casey@contoso.example", UserFolder: filepath.Join(profile, "OneDrive - Everything IT"),
			TenantID: tenant, SyncedFolders: []string{newInt, newExt}}},
		Current: []Library{
			{SiteID: siteInt, ListID: list, TenantID: tenant, WebURL: "https://eit.sharepoint.com/sites/internal", MountPoint: newInt},
		},
		Known: []Library{
			// Old Internal: same site and list as the one syncing now -> old copy.
			{SiteID: "{" + siteInt + "}", ListID: list, TenantID: tenant, WebURL: "https://eit.sharepoint.com/sites/internal",
				MountPoint: filepath.Join(oldOrg, "Internal - Documents"), UserEmail: "casey@contoso.example"},
			// Directors: same list ID but another site -> NOT the same library.
			{SiteID: siteDir, ListID: list, TenantID: tenant, WebURL: "https://eit.sharepoint.com/sites/directors",
				MountPoint: filepath.Join(oldOrg, "EIT Directors - Documents"), UserEmail: "casey@contoso.example"},
			// A tenant that is no longer signed in.
			{SiteID: siteDir, ListID: list, TenantID: "11111111-2222-3333-4444-555555555555", WebURL: "https://northwind.sharepoint.com/sites/shared",
				MountPoint: filepath.Join(other, "Shared - Documents")},
		},
		Providers: []Provider{
			// External: matched by SharePoint URL only (stale entry + live entry).
			{MountPoint: filepath.Join(oldOrg, "External - Documents"), LibraryType: "teamsite", URLNamespace: "https://eit.sharepoint.com/sites/external/Shared Documents/"},
			{MountPoint: newExt, LibraryType: "teamsite", URLNamespace: "https://eit.sharepoint.com/sites/external/Shared Documents"},
		},
	}
	got := map[string]Orphan{}
	for _, o := range FindOrphans(in, fs) {
		got[o.Path] = o
	}
	want := map[string]struct {
		kind    Kind
		movedTo string
		resync  bool
	}{
		filepath.Join(oldOrg, "Internal - Documents"):      {KindMoved, newInt, false},
		filepath.Join(oldOrg, "External - Documents"):      {KindMoved, newExt, false},
		filepath.Join(oldOrg, "EIT Directors - Documents"): {KindStopped, "", true},
		filepath.Join(oldOrg, "Notes"):                     {KindUnknown, "", true},
		filepath.Join(other, "Shared - Documents"):         {KindDisconnected, "", false},
	}
	if len(got) != len(want) {
		t.Fatalf("orphans: %+v", got)
	}
	for p, w := range want {
		o, ok := got[p]
		if !ok {
			t.Errorf("%s not reported", p)
			continue
		}
		if o.Kind != w.kind || o.MovedTo != w.movedTo || o.NeedsResync() != w.resync {
			t.Errorf("%s: kind %d moved to %q resync %v, want %d %q %v", p, o.Kind, o.MovedTo, o.NeedsResync(), w.kind, w.movedTo, w.resync)
		}
	}
}

// Casey's PC: OneDrive Helper was installed after the rename, and OneDrive
// keeps no entries for the old paths, so only the folder names link the old
// folders to libraries: its current settings (Internal) and leftover
// ClientPolicy files (External, EIT Senior Management Team). Nothing at all
// is known about EIT Directors.
func TestClassifyByFolderName(t *testing.T) {
	const tenant = "956a401c-dc69-43fe-9af8-b4c3f4d898c1"
	profile := `C:\Users\casey`
	oldOrg, newOrg := filepath.Join(profile, "EVERYTHING COMPUTER SYSTEMS LIMITED"), filepath.Join(profile, "Everything IT")
	pol := func(site, list, siteID, ns string) Library {
		return Library{SiteTitle: site, ListTitle: list, SiteID: siteID, WebID: siteID, ListID: "846c226d9e36484a98cdd4cd359a3c3e", URLNamespace: ns, Account: "Business1"}
	}
	smt := pol("EIT Senior Management Team", "Documents", "11111111111111111111111111111111", "https://eitcomputers.sharepoint.com/sites/EIT-SMT/Shared Documents/")
	internal := pol("Internal", "Documents", "22222222222222222222222222222222", "https://eitcomputers.sharepoint.com/sites/internal/Shared Documents/")
	external := pol("External", "Documents", "33333333333333333333333333333333", "https://eitcomputers.sharepoint.com/sites/external/Shared Documents/")
	newSMT, newInt, newExt := filepath.Join(newOrg, "EIT Senior Management Team - Documents"), filepath.Join(newOrg, "Internal - Documents"), filepath.Join(newOrg, "External - Documents")
	in := Inputs{
		ProfileDir: profile,
		Accounts: []Account{{Key: "Business1", UserFolder: filepath.Join(profile, "OneDrive - Everything IT"), TenantID: tenant,
			SyncedFolders: []string{newSMT, newInt, newExt}}},
		// Internal is in OneDrive's current settings (same site + list IDs as its policy file).
		Current: []Library{{SiteTitle: "Internal", ListTitle: "Documents", SiteID: internal.SiteID, ListID: internal.ListID, TenantID: tenant,
			WebURL: "https://eitcomputers.sharepoint.com/sites/internal", MountPoint: newInt}},
		// External and SMT: only the live provider entries for the NEW folders.
		Providers: []Provider{
			{MountPoint: newExt, LibraryType: "teamsite", URLNamespace: external.URLNamespace},
			{MountPoint: newSMT, LibraryType: "teamsite", URLNamespace: smt.URLNamespace},
		},
		Policies: []Library{smt, internal, external},
	}
	active := cleanAll([]string{newSMT, newInt, newExt})
	for name, want := range map[string]struct {
		kind  Kind
		moved string
	}{
		"Internal - Documents":                   {KindMoved, newInt},
		"External - Documents":                   {KindMoved, newExt},
		"EIT Senior Management Team - Documents": {KindMoved, newSMT},
		"EIT Directors - Documents":              {KindUnknown, ""},
	} {
		if k, moved := in.classify(Clean(filepath.Join(oldOrg, name)), active); k != want.kind || moved != want.moved {
			t.Errorf("%s: kind %d moved to %q, want %d %q", name, k, moved, want.kind, want.moved)
		}
	}

	// A library with that name that isn't synced anywhere now: stopped, and its
	// SharePoint host shows the organisation is still signed in.
	in.Providers = in.Providers[1:] // External no longer synced
	in.Accounts[0].SyncedFolders = []string{newSMT, newInt}
	if k, moved := in.classify(Clean(filepath.Join(oldOrg, "External - Documents")), cleanAll([]string{newSMT, newInt})); k != KindStopped || moved != "" {
		t.Errorf("External, not synced anywhere: kind %d moved to %q", k, moved)
	}

	// Two different libraries with the same name (e.g. in two organisations): don't guess.
	in.Policies = append(in.Policies, pol("Internal", "Documents", "44444444444444444444444444444444", "https://fabrikam.sharepoint.com/sites/internal/Shared Documents/"))
	if k, moved := in.classify(Clean(filepath.Join(oldOrg, "Internal - Documents")), active); k != KindUnknown || moved != "" {
		t.Errorf("ambiguous name: kind %d moved to %q", k, moved)
	}
}

func TestClassifyConnectedBySharePointHost(t *testing.T) {
	active := []string{Clean(`C:\Users\d\Contoso\Live - Documents`), Clean(`C:\Users\d\OneDrive - Contoso`)}
	in := Inputs{
		Accounts: []Account{{Key: "Business1", UserFolder: `C:\Users\d\OneDrive - Contoso`}}, // no tenant ID recorded
		Providers: []Provider{
			{MountPoint: `C:\Users\d\OneDrive - Contoso`, LibraryType: "personal", URLNamespace: "https://contoso-my.sharepoint.com/personal/d/Documents/"},
			{MountPoint: `C:\Users\d\Contoso\Gone - Documents`, LibraryType: "teamsite", URLNamespace: "https://contoso.sharepoint.com/sites/gone/Shared Documents/"},
			{MountPoint: `C:\Users\d\Fabrikam\Bids - Documents`, LibraryType: "teamsite", URLNamespace: "https://fabrikam.sharepoint.com/sites/bids/Shared Documents/"},
		},
	}
	for path, want := range map[string]Kind{
		`C:\Users\d\Contoso\Gone - Documents`:  KindStopped,      // contoso-my.sharepoint.com is the same tenant
		`C:\Users\d\Fabrikam\Bids - Documents`: KindDisconnected, // nothing signed in to Fabrikam
		`C:\Users\d\Contoso\Mystery`:           KindUnknown,
	} {
		if k, moved := in.classify(Clean(path), active); k != want || moved != "" {
			t.Errorf("%s: %d %q, want %d", path, k, moved, want)
		}
	}
	// Signed out of every work account: anything identifiable is from a disconnected account.
	in.Accounts = nil
	if k, _ := in.classify(Clean(`C:\Users\d\Contoso\Gone - Documents`), nil); k != KindDisconnected {
		t.Errorf("no accounts: %d", k)
	}
	if k, _ := in.classify(Clean(`C:\Users\d\Contoso\Mystery`), nil); k != KindUnknown {
		t.Errorf("no accounts, nothing known: %d", k)
	}
}

func TestSPHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://Contoso-my.sharepoint.com/personal/d": "contoso.sharepoint.com",
		"https://contoso.sharepoint.com/sites/x/":      "contoso.sharepoint.com",
		"http://contoso.sharepoint.com":                "",
		`C:\Users\d`:                                   "",
		"":                                             "",
	} {
		if got := spHost(in); got != want {
			t.Errorf("spHost(%q) = %q, want %q", in, got, want)
		}
	}
}
