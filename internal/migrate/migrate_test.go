package migrate

import (
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/dialog"
	"github.com/denismcapple/OneDriveHelper/internal/elog"
	"github.com/denismcapple/OneDriveHelper/internal/folders"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
)

var discard = log.New(io.Discard, "", 0)

// jsmith is what a real user had after their redirection GPO was removed.
var jsmith = []folders.Folder{
	{Value: "Desktop", Name: "Desktop", ID: "{B4BFCC3A-DB2C-424C-B029-7FE99A87C641}", Rel: "Desktop", Current: `\\fs01\users\jsmith\Desktop`},
	{Value: "Personal", Name: "Documents", ID: "{FDD39AD0-238F-46AF-ADB4-6C85480369C7}", Rel: "Documents", Current: `\\fs01\users\jsmith\Documents`},
	{Value: "My Pictures", Name: "Pictures", ID: "{33E28130-4E1E-4676-835A-98395C3BC3BB}", Rel: "Pictures", Current: `\\fs01\users\jsmith\Pictures`},
	{Value: "{F42EE2D3-909F-4907-8871-4C22FC0BF756}", Name: "Documents", Rel: "Documents", Current: `\\fs01\users\jsmith\Documents`},
}

type world struct {
	shared []string
	folds  []folders.Folder
	gpos   []string
	gpoErr error
	policy string
	work   bool
	half   bool // Business1 with a folder but no tenant (a real PC)
}

func (w *world) use(t *testing.T) {
	t.Helper()
	saved := sys
	sys = env{
		shared:     func() []string { return w.shared },
		redirected: func(folders.Hive, string) ([]folders.Folder, error) { return w.folds, nil },
		gpos:       func(string) ([]string, error) { return w.gpos, w.gpoErr },
		policy:     func() string { return w.policy },
		accounts: func(onedrive.Hive) []onedrive.Account {
			if w.half {
				return []onedrive.Account{{Key: "Business1", UserFolder: `C:\Users\jsmith\OneDrive`}}
			}
			if !w.work {
				return []onedrive.Account{{Key: "Personal", UserFolder: `C:\Users\jsmith\OneDrive`}}
			}
			return []onedrive.Account{{Key: "Business1", UserFolder: `C:\Users\jsmith\OneDrive - Contoso`, TenantID: "11111111-2222-3333-4444-555555555555"}}
		},
	}
	t.Cleanup(func() { sys = saved })
}

func on() config.Config {
	c := config.Defaults()
	c.FolderRedirectionMigration = true
	return c
}

func TestCheck(t *testing.T) {
	ready := func() *world { return &world{folds: jsmith, policy: "t", work: true} }
	for _, c := range []struct {
		name   string
		cfg    config.Config
		change func(w *world)
		want   string // part of why not ("" = offered)
	}{
		{"offered", on(), func(*world) {}, ""},
		{"setting off", config.Defaults(), func(*world) {}, "turned off"},
		{"RDS host", on(), func(w *world) { w.shared = []string{"this is a server, not a workstation"} }, "shared or virtual"},
		{"policy still applies", on(), func(w *world) { w.gpos = []string{"Staff - Redirected folders"} }, "still applies (Staff - Redirected folders)"},
		{"can't read policy", on(), func(w *world) { w.gpoErr = errors.New("access denied") }, "can't tell"},
		{"nothing redirected", on(), func(w *world) { w.folds = nil }, "no folders"},
		{"personal account only", on(), func(w *world) { w.work = false }, "no work account"},
		{"work account entry without a sign-in (a real PC)", on(), func(w *world) { w.half = true }, "no work account"},
		{"backup policy not set yet", on(), func(w *world) { w.policy = "" }, "KFMSilentOptIn"},
	} {
		w := ready()
		c.change(w)
		w.use(t)
		o, why := Check(c.cfg, "S-1-5-21-1", folders.CurrentUser, onedrive.Hive{}, `C:\Users\jsmith`)
		if (o != nil) != (c.want == "") || (c.want != "" && !strings.Contains(why, c.want)) {
			t.Errorf("%s: offer %v, why %q", c.name, o != nil, why)
		}
	}
}

func TestOfferNamesAndNote(t *testing.T) {
	o := &Offer{Folders: jsmith, Manual: []folders.Folder{{Value: "Something", Name: "Something", Current: `\\fs01\users\jsmith\Other`}}}
	if o.Names() != "Desktop, Documents and Pictures" {
		t.Errorf("names: %q", o.Names())
	}
	note := Note(config.Defaults(), o)
	for _, want := range []string{`Desktop:  \\fs01\users\jsmith\Desktop`, `Documents:  \\fs01\users\jsmith\Documents`, `Something:  \\fs01\users\jsmith\Other`, "Please contact"} {
		if !strings.Contains(note, want) {
			t.Errorf("note lacks %q:\n%s", want, note)
		}
	}
	if strings.Count(note, `\\fs01\users\jsmith\Documents`) != 1 {
		t.Error("Documents and its alias point at the same place: listed once")
	}
	cfg := config.Defaults()
	cfg.MigrationNoteText = "Ask IT.\n{folders}"
	if n := Note(cfg, o); !strings.HasPrefix(n, "Ask IT.\r\n") || !strings.Contains(n, `Pictures:  \\fs01`) {
		t.Errorf("custom note: %q", n)
	}
}

type recorded struct {
	repointed []string
	note      string
	shortcut  string
	explorer  int
	onedrive  int
	events    []uint32
	results   []dialog.Message
	elevated  int
}

func useFlow(t *testing.T, fail map[string]bool) *recorded {
	t.Helper()
	saved := flow
	r := &recorded{}
	flow = flowEnv{
		confirm: func(dialog.Message) bool { return true },
		elevate: func(string, []string, time.Time) (uint32, error, <-chan struct{}) {
			r.elevated++
			done := make(chan struct{})
			close(done)
			return 0, nil, done
		},
		repoint: func(f folders.Folder, _ string) error {
			if fail[f.Name] {
				return errors.New("access denied")
			}
			r.repointed = append(r.repointed, f.Value)
			return nil
		},
		note:     func(_, text string) error { r.note = text; return nil },
		shortcut: func(lnk, target, _ string) error { r.shortcut = lnk + " -> " + target; return nil },
		explorer: func() error { r.explorer++; return nil },
		onedrive: func() error { r.onedrive++; return nil },
		desktop:  func() (string, error) { return `C:\Users\jsmith\Desktop`, nil },
		result:   func(m dialog.Message) { r.results = append(r.results, m) },
		event:    func(id uint32, _ bool, _ string) { r.events = append(r.events, id) },
	}
	t.Cleanup(func() { flow = saved })
	return r
}

func TestDo(t *testing.T) {
	r := useFlow(t, nil)
	Do(on(), &Offer{Folders: jsmith}, `CONTOSO\jsmith`, `C:\Users\jsmith`, "confirmed", discard)
	if len(r.repointed) != 4 || r.explorer != 1 || r.onedrive != 1 || r.note == "" {
		t.Fatalf("every folder (and alias) is pointed back, then Explorer and OneDrive restart: %+v", r)
	}
	if len(r.events) != 1 || r.events[0] != elog.FoldersMigrated || r.results[0].Kind != dialog.KindSuccess {
		t.Errorf("events %v results %+v", r.events, r.results)
	}
}

func TestDoShortcut(t *testing.T) {
	r := useFlow(t, nil)
	Do(on(), &Offer{Folders: jsmith}, `CONTOSO\jsmith`, `C:\Users\jsmith`, "confirmed", discard)
	if r.shortcut != "" {
		t.Errorf("no shortcut unless one is set: %q", r.shortcut)
	}
	cfg := on()
	cfg.MigrationShortcutName, cfg.MigrationShortcutTarget = "Network Drive", `U:\`
	r = useFlow(t, nil)
	Do(cfg, &Offer{Folders: jsmith}, `CONTOSO\jsmith`, `C:\Users\jsmith`, "confirmed", discard)
	if r.shortcut != `C:\Users\jsmith\Desktop\Network Drive.lnk -> U:\` || !strings.Contains(r.results[0].Text, "open 'Network Drive' on your Desktop") {
		t.Errorf("shortcut %q, result %q", r.shortcut, r.results[0].Text)
	}
	if strings.Count(r.note, "Documents:") != 1 {
		t.Error("the second Documents entry is listed once in the note")
	}
}

func TestDoPartFailure(t *testing.T) {
	r := useFlow(t, map[string]bool{"Pictures": true})
	Do(on(), &Offer{Folders: jsmith}, `CONTOSO\jsmith`, `C:\Users\jsmith`, "confirmed", discard)
	if len(r.events) != 1 || r.events[0] != elog.FolderMigrationFailed || !strings.Contains(r.results[0].Text, "Pictures") {
		t.Errorf("a folder that could not be changed is reported: %v %+v", r.events, r.results)
	}
}

func TestRunApprovalSetting(t *testing.T) {
	w := &world{folds: jsmith, policy: "t", work: true}
	w.use(t)
	r := useFlow(t, nil)
	cfg := on()
	cfg.RequireAdminApproval = false
	o, _ := Check(cfg, "S-1-5-21-1", folders.CurrentUser, onedrive.Hive{}, `C:\Users\jsmith`)
	Run(cfg, o, `CONTOSO\jsmith`, "S-1-5-21-1", `C:\Users\jsmith`, "agent.exe", discard)
	if r.elevated != 0 || len(r.repointed) != 4 {
		t.Errorf("approval off: no UAC prompt, and the move runs: %+v", r)
	}
	r2 := useFlow(t, nil)
	cfg.RequireAdminApproval = true
	Run(cfg, o, `CONTOSO\jsmith`, "S-1-5-21-1", `C:\Users\jsmith`, "agent.exe", discard)
	if r2.elevated != 1 || len(r2.repointed) != 4 {
		t.Errorf("approval on: approved through UAC first: %+v", r2)
	}
}
