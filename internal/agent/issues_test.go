package agent

import (
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/elog"
	"github.com/denismcapple/OneDriveHelper/internal/odstatus"
	"github.com/denismcapple/OneDriveHelper/internal/popup"
)

func root(name string, s odstatus.State, status, msg, action string) odstatus.Root {
	return odstatus.Root{ID: "OneDrive!S-1-5-21-1!Business1|abc", Name: name, State: s, Status: status, Message: msg, Action: action}
}

func TestFromStatus(t *testing.T) {
	obs := fromStatus([]odstatus.Root{
		root("OneDrive - Contoso", odstatus.InSync, "Backed up and synced", "", ""),
		root("Finance - Documents", odstatus.Syncing, "Syncing 3 files", "", ""),
		root("Projects - Documents", odstatus.Error, "Sync problems", "You have 2 sync issues", "View sync issues"),
		root("HR - Documents", odstatus.Paused, "Paused", "", ""),
		root("Legal - Documents", odstatus.Offline, "", "", ""),
		{Name: "Broken", State: odstatus.Error, Err: "no status"},
	})
	if len(obs) != 3 {
		t.Fatalf("only error, paused and offline are problems; roots that couldn't be read are skipped: %+v", obs)
	}
	e := obs[0]
	if e.Text != "You have 2 sync issues" || e.Action != "View sync issues" || e.RootID == "" || e.Slow || e.Library != "Projects - Documents" {
		t.Errorf("error: OneDrive's own message and button are used: %+v", e)
	}
	if !obs[1].Slow || obs[1].Text != "Paused" || !obs[2].Slow || obs[2].Text != textOffline {
		t.Errorf("paused/offline wait for the longer grace, with our text when OneDrive gives none: %+v", obs[1:])
	}
}

func TestFromProvider(t *testing.T) {
	for _, c := range []struct {
		status uint32
		key    string
		slow   bool
	}{
		{0x1, "", false},  // idle
		{0x10, "", false}, // incremental sync
		{providerDisconnected, "disconnected", true},
		{providerError, "provider-error", false},
		{providerTerminated, "provider-error", false},
		{0x10 | providerConnectivityLost, "offline", true},
	} {
		obs := fromProvider(`C:\Users\d\Contoso\Finance - Documents\`, c.status)
		if c.key == "" {
			if len(obs) != 0 {
				t.Errorf("%#x is healthy: %+v", c.status, obs)
			}
			continue
		}
		if len(obs) != 1 || obs[0].Key != c.key || obs[0].Slow != c.slow || obs[0].Library != "Finance - Documents" {
			t.Errorf("%#x: %+v", c.status, obs)
		}
	}
}

func TestTrackerGrace(t *testing.T) {
	var tr tracker
	t0 := time.Now()
	errObs := Observation{Key: "err", Text: "2 sync issues", Library: "A"}
	paused := Observation{Key: "paused", Text: "Paused", Library: "B", Slow: true}
	if got := tr.update([]Observation{errObs, paused}, t0, 30*time.Minute); len(got) != 0 {
		t.Fatalf("nothing is reported on first sight: %+v", got)
	}
	got := tr.update([]Observation{errObs, paused}, t0.Add(shortGrace), 30*time.Minute)
	if len(got) != 1 || got[0].Key != "err" {
		t.Fatalf("an error is reported after the short grace, a pause not yet: %+v", got)
	}
	if got := tr.update([]Observation{paused}, t0.Add(31*time.Minute), 30*time.Minute); len(got) != 1 || got[0].Key != "paused" {
		t.Fatalf("the pause is reported after the long grace: %+v", got)
	}
	if got := tr.update([]Observation{errObs}, t0.Add(32*time.Minute), 30*time.Minute); len(got) != 0 {
		t.Fatalf("a problem that went away and came back starts its grace again: %+v", got)
	}
}

func TestTrackerGroupsLibraries(t *testing.T) {
	tr := tracker{}
	t0 := time.Now()
	obs := []Observation{{Key: "k", Text: "Sign in", Library: "B"}, {Key: "k", Text: "Sign in", Library: "A"}}
	tr.update(obs, t0, time.Hour)
	got := tr.update(obs, t0.Add(shortGrace), time.Hour)
	if len(got) != 1 || strings.Join(got[0].Libraries, ",") != "A,B" {
		t.Errorf("one problem with both libraries: %+v", got)
	}
}

func TestCheckIssues(t *testing.T) {
	var events []uint32
	obs := []Observation{{Key: "not-responding", Text: textNotResponding, Restart: true}}
	a := &Agent{log: log.New(io.Discard, "", 0), onEvent: func(id uint32) { events = append(events, id) },
		observe: func(string) []Observation { return obs }}
	cfg := config.Defaults()
	t0 := time.Now()
	if got := a.checkIssues(cfg, t0.Add(time.Hour), true); got != nil {
		t.Fatal("off by default")
	}
	cfg.OneDriveIssueAlerts = true
	a.checkIssues(cfg, t0, true)
	if got := a.checkIssues(cfg, t0.Add(shortGrace), true); len(got) != 1 || len(events) != 1 || events[0] != elog.OneDriveNotResponding {
		t.Fatalf("problems %+v events %v", got, events)
	}
	a.checkIssues(cfg, t0.Add(shortGrace+time.Minute), true)
	if len(events) != 1 {
		t.Errorf("the same problem is logged once: %v", events)
	}
	if got := a.checkIssues(cfg, t0.Add(shortGrace+2*time.Minute), false); got != nil || a.issueKey != "" {
		t.Error("not checked while OneDrive isn't running (the 'not running' alert covers that)")
	}
	b, ok := issueButton([]Problem{{Key: "x", Text: "Sign in", Action: "Sign in", RootID: "r"}, {Key: "stuck", Restart: true}}, Actions{OneDrive: func(Problem) {}})
	if !ok || b.Label != "Restart OneDrive" {
		t.Errorf("a hung OneDrive comes first: %+v", b)
	}
	if b, _ := issueButton([]Problem{{Key: "x", Action: "View sync issues"}}, Actions{OneDrive: func(Problem) {}}); b.Label != "View sync issues" {
		t.Errorf("OneDrive's own button label: %q", b.Label)
	}
	if b, _ := issueButton([]Problem{{Key: "x"}}, Actions{OneDrive: func(Problem) {}}); b.Label != "Open OneDrive" {
		t.Errorf("without OneDrive's button: %q", b.Label)
	}
}

func TestIssuesInStatus(t *testing.T) {
	s := Status{Configured: true, Running: true, Issues: []Problem{{Key: "k", Text: "Sign in to OneDrive", Libraries: []string{"OneDrive - Contoso"}}}}
	if s.Problems() != 1 || s.severity() != popup.Warning || !strings.Contains(s.summary(), "OneDrive needs attention") {
		t.Errorf("problems %d severity %v summary %q", s.Problems(), s.severity(), s.summary())
	}
	spec := statusSpec(config.Defaults(), s, Actions{OneDrive: func(Problem) {}, Snooze: func(string, string) {}})
	found := false
	for _, it := range spec.Items {
		found = found || strings.Contains(it.Title, "Sign in to OneDrive")
	}
	if !found {
		t.Errorf("the status window lists the problem: %+v", spec.Items)
	}
}
