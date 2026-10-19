package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
	"github.com/denismcapple/OneDriveHelper/internal/popup"
	"github.com/denismcapple/OneDriveHelper/internal/version"
)

func sampleOrphans() []onedrive.Orphan {
	return []onedrive.Orphan{
		{Name: "Bids - Documents", Path: `C:\Users\d\Fabrikam\Bids - Documents`, TenantRoot: `C:\Users\d\Fabrikam`},
		{Name: "OLD LTD", Path: `C:\Users\d\OLD LTD`, TenantRoot: `C:\Users\d\OLD LTD`, EmptyOrg: true},
	}
}

func TestStatusSummaryAndSeverity(t *testing.T) {
	ok := Status{Configured: true, Running: true}
	if ok.Problems() != 0 || ok.severity() != popup.OK || ok.summary() != "Everything looks good" {
		t.Errorf("ok: %d %v %q", ok.Problems(), ok.severity(), ok.summary())
	}
	down := Status{Configured: true}
	if down.Problems() != 1 || down.severity() != popup.Warning || down.summary() != "OneDrive is not running" {
		t.Errorf("down: %d %v %q", down.Problems(), down.severity(), down.summary())
	}
	updating := Status{Configured: true, Updating: true}
	if updating.NotRunning() {
		t.Error("an update in progress is not an outage")
	}
	both := Status{Configured: true, Orphans: sampleOrphans()}
	if both.Problems() != 3 || both.severity() != popup.Critical || both.summary() != "OneDrive is not running, 2 folders not syncing" {
		t.Errorf("both: %d %v %q", both.Problems(), both.severity(), both.summary())
	}
}

func TestStatusSpec(t *testing.T) {
	cfg := config.Defaults()
	s := Status{At: time.Now(), Configured: true, Orphans: sampleOrphans()}
	spec := statusSpec(cfg, s, Actions{})
	if spec.Title != "3 problems need attention" || spec.OnHelp == nil {
		t.Errorf("title/help: %q", spec.Title)
	}
	if !strings.Contains(spec.Body, "NOT running") || strings.Contains(spec.Body, "Checked") || strings.Contains(spec.Body, "version") {
		t.Errorf("body: %q", spec.Body)
	}
	if len(spec.Items) != 2 || spec.Items[0].Title != "•  Bids - Documents (Fabrikam)" || spec.Items[0].Path != `C:\Users\d\Fabrikam\Bids - Documents` ||
		spec.Items[0].Note == "" || !strings.Contains(spec.Items[1].Title, "empty organisation folder") {
		t.Errorf("items: %+v", spec.Items)
	}
	var labels []string
	for _, b := range spec.Buttons {
		labels = append(labels, b.Label)
	}
	if strings.Join(labels, ",") != "Ignore,Copy details,Contact support,Restart OneDrive,Fix (admin)" {
		t.Errorf("buttons: %v", labels)
	}
	var ignored []string
	a := Actions{Snooze: func(id, d string) { ignored = append(ignored, id) }}
	statusSpec(cfg, s, a).Buttons[0].Menu[0].Action()
	if strings.Join(ignored, ",") != idNotRunning+","+idOrphans {
		t.Errorf("status-card Ignore should cover the alerts shown: %v", ignored)
	}
	s.OrphanSnooze = SnoozeInfo{Active: true, UntilReboot: true}
	if f := statusSpec(cfg, s, Actions{}).Footer; !strings.Contains(f, "ignored until the next restart") {
		t.Errorf("snooze not shown: %q", f)
	}
	healthy := statusSpec(cfg, Status{At: time.Now(), Configured: true, Running: true}, Actions{})
	if healthy.Title != "Everything looks good" || healthy.Severity != popup.OK || len(healthy.Buttons) != 3 || healthy.Buttons[0].Label != "Close" {
		t.Errorf("healthy: %q %v %+v", healthy.Title, healthy.Severity, healthy.Buttons)
	}
}

func TestReport(t *testing.T) {
	log := filepath.Join(t.TempDir(), "agent.log")
	_ = os.WriteFile(log, []byte("line one\nline two\n"), 0o644)
	cfg := config.Defaults()
	cfg.CompanyName = "Contoso"
	r := Report(cfg, Status{At: time.Now(), Configured: true, Orphans: sampleOrphans(),
		Accounts: []onedrive.Account{{Key: "Business1", UserEmail: "d@contoso.com", UserFolder: `C:\Users\d\OneDrive - Contoso`}}}, log)
	for _, want := range []string{"Contoso", "d@contoso.com", "Folders not syncing (2)", `C:\Users\d\Fabrikam\Bids - Documents`, "line two", "running: false"} {
		if !strings.Contains(r, want) {
			t.Errorf("report missing %q:\n%s", want, r)
		}
	}
}

func TestTrayTooltipShowsVersion(t *testing.T) {
	cfg := config.Defaults()
	cfg.CompanyName = "Contoso"
	tip := trayTooltip(cfg, Status{Configured: true, Running: true})
	if want := "Contoso - OneDrive Helper v" + version.Version + "\nEverything looks good"; tip != want {
		t.Errorf("tooltip %q, want %q", tip, want)
	}
	cfg.CompanyName = "A Very Long Company Name Limited Trading As Something"
	s := Status{Configured: true, Orphans: sampleOrphans()}
	if tip := trayTooltip(cfg, s); len([]rune(tip)) > 127 {
		t.Errorf("tooltip too long for Windows (%d chars): %q", len([]rune(tip)), tip)
	}
}

func TestExplanationsAndHelpLinks(t *testing.T) {
	cfg := config.Defaults()
	cfg.SupportName = "the Contoso Service Desk"
	for topic, want := range map[string][]string{
		topicNotRunning: {"WHAT HAS HAPPENED?", "WHY ARE WE TELLING YOU?", "WHY DOES IT NEED FIXING?", "Restart OneDrive", "the Contoso Service Desk"},
		topicOrphans:    {"WHAT HAS HAPPENED?", "Bids - Documents (Fabrikam)", "not backed up", "saves a copy", "the Contoso Service Desk"},
		topicStatus:     {"Copy details", "the Contoso Service Desk"},
	} {
		text := Explain(cfg, topic, sampleOrphans())
		for _, w := range want {
			if !strings.Contains(text, w) {
				t.Errorf("%s: missing %q", topic, w)
			}
		}
	}
	var got []string
	a := Actions{Explain: func(topic string) { got = append(got, topic) }}
	notRunningSpec(cfg, a, false).OnHelp()
	orphanSpec(cfg, sampleOrphans(), a).OnHelp()
	if strings.Join(got, ",") != topicNotRunning+","+topicOrphans {
		t.Errorf("help links wired to %v", got)
	}
}
