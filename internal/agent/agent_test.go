package agent

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
	"github.com/denismcapple/OneDriveHelper/internal/remediate"
)

func TestSnoozeState(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	boot := now.Add(-3 * time.Hour)
	cases := []struct {
		name          string
		until, bootAt uint64
		want          bool
		desc          string
	}{
		{"none", 0, 0, false, ""},
		{"until future", uint64(now.Add(time.Hour).Unix()), 0, true, "until " + now.Add(time.Hour).Format("15:04")},
		{"until past", uint64(now.Add(-time.Minute).Unix()), 0, false, ""},
		{"same boot", 0, uint64(boot.Add(30 * time.Second).Unix()), true, "until the next restart"},
		{"rebooted since", 0, uint64(boot.Add(-24 * time.Hour).Unix()), false, ""},
	}
	for _, c := range cases {
		got := snoozeState(c.until, c.bootAt, now, boot)
		if got.Active != c.want || (c.want && got.Describe() != c.desc) {
			t.Errorf("%s: got %+v (%q)", c.name, got, got.Describe())
		}
	}
}

func TestNotRunningSpec(t *testing.T) {
	var snoozed []string
	relaunched := false
	a := Actions{Relaunch: func() { relaunched = true }, Snooze: func(id, d string) { snoozed = append(snoozed, id+":"+d) }}
	s := notRunningSpec(config.Defaults(), a, false)
	if len(s.Buttons) != 2 || !s.Buttons[1].Primary || s.Buttons[1].Label != "Restart OneDrive" {
		t.Fatalf("buttons: %+v", s.Buttons)
	}
	menu := s.Buttons[0].Menu
	if len(menu) != 3 || !s.Buttons[0].Dismiss {
		t.Fatalf("ignore menu: %+v", s.Buttons[0])
	}
	for _, m := range menu {
		m.Action()
	}
	s.Buttons[1].Action()
	want := fmt.Sprintf("%s:5m,%s:30m,%s:reboot", idNotRunning, idNotRunning, idNotRunning)
	if strings.Join(snoozed, ",") != want || !relaunched {
		t.Errorf("actions not wired: %v %v", snoozed, relaunched)
	}
	if menu[2].Label != "Ignore until next restart" || s.Body != defaultNotRunning || s.Brand != "OneDrive Helper" {
		t.Errorf("labels/body/brand: %q %q %q", menu[2].Label, s.Body, s.Brand)
	}

	cfg := config.Defaults()
	cfg.IgnoreOptions, cfg.NotRunningMessage, cfg.CompanyName = "", "Call us!", "Contoso"
	s = notRunningSpec(cfg, a, true)
	if len(s.Buttons) != 1 || s.Body != "Call us!" || s.Brand != "Contoso - OneDrive Helper" {
		t.Errorf("no ignore button / custom text / company title expected: %+v", s)
	}
}

func TestOrphanGroups(t *testing.T) {
	var items []string
	for _, it := range orphanSpec(config.Defaults(), DemoOrphans(), Actions{}).Items {
		if it.Heading {
			items = append(items, "# "+it.Title)
		} else {
			items = append(items, it.Title)
		}
	}
	want := []string{
		"# Stopped syncing (2)", "•  Projects - Documents", "•  Marketing - Documents (click 'Sync' on its SharePoint page afterwards)",
		`# Old copies: these libraries now sync to "AcmeCorp" (2)`, "•  Finance - Documents", "•  Sales - Documents",
		"# From an account that is no longer connected (1)", "•  Bids Team - Documents",
		"# Empty leftover folders (1)", "•  Contoso Ltd",
	}
	if strings.Join(items, "\n") != strings.Join(want, "\n") {
		t.Errorf("items:\n%s\nwant:\n%s", strings.Join(items, "\n"), strings.Join(want, "\n"))
	}
	// Old copies that moved to different organisation folders get a general heading.
	moved := []onedrive.Orphan{
		{Name: "A", Path: `C:\Users\d\Old\A`, Kind: onedrive.KindMoved, MovedTo: `C:\Users\d\One\A`},
		{Name: "B", Path: `C:\Users\d\Old\B`, Kind: onedrive.KindMoved, MovedTo: `C:\Users\d\Two\B`},
	}
	if g := remediate.Groups(moved); len(g) != 1 || g[0].Heading != "Old copies: these libraries now sync to a new folder (2)" {
		t.Errorf("groups: %+v", g)
	}
}

func TestOrphanSpec(t *testing.T) {
	cfg := config.Defaults()
	cfg.SupportName = "Contoso Service Desk"
	var orphans []onedrive.Orphan
	for _, n := range []string{"A", "B", "C", "D", "E", "F", "G"} {
		orphans = append(orphans, onedrive.Orphan{Name: n, Path: `C:\Users\d\Org\` + n, TenantRoot: `C:\Users\d\Org`})
	}
	var snoozed []string
	s := orphanSpec(cfg, orphans, Actions{Snooze: func(id, d string) { snoozed = append(snoozed, id) }})
	// The Contact support button covers the support details: they aren't repeated in the text.
	if s.Title != orphanTitle || s.Body != defaultOrphan || s.Footer != "…and 1 more." {
		t.Errorf("title/body/footer: %q / %q / %q", s.Title, s.Body, s.Footer)
	}
	if len(s.Items) != maxListed+1 || !s.Items[0].Heading || s.Items[0].Title != "Stopped syncing (7)" || s.Items[0].Note != remediate.ActionResync ||
		s.Items[1].Path != `C:\Users\d\Org\A` || s.Items[1].Title != "•  A" || s.Items[1].Note != "" {
		t.Errorf("items: %+v", s.Items)
	}
	empty := orphanSpec(cfg, []onedrive.Orphan{{Name: "Old", Path: `C:\Users\d\Old`, TenantRoot: `C:\Users\d\Old`, EmptyOrg: true}}, Actions{})
	if empty.Title != orphanTitle || empty.Items[0].Title != "Empty leftover folders (1)" {
		t.Errorf("leftover organisation folder: %q %+v", empty.Title, empty.Items)
	}
	var labels []string
	for _, b := range s.Buttons {
		labels = append(labels, b.Label)
	}
	if strings.Join(labels, ",") != "Ignore,Contact support,Fix (admin)" {
		t.Errorf("buttons: %v", labels)
	}
	s.Buttons[0].Menu[0].Action()
	if strings.Join(snoozed, ",") != idOrphans {
		t.Errorf("ignore must apply to this alert only: %v", snoozed)
	}
	cfg.RemediationEnabled = false
	off := orphanSpec(cfg, orphans, Actions{})
	for _, b := range off.Buttons {
		if strings.Contains(b.Label, "Fix") {
			t.Error("fix button shown although remediation disabled")
		}
	}
	if off.Items[0].Note != "" {
		t.Errorf("no 'We will...' line when fixing is turned off: %q", off.Items[0].Note)
	}
}

func TestSupportButton(t *testing.T) {
	var got []string
	a := Actions{
		Support: func() { got = append(got, "info") },
		Email:   func() { got = append(got, "email") },
		Website: func() { got = append(got, "web") },
	}
	cfg := config.Defaults()
	if b := supportButton(cfg, a); b.Menu != nil || b.Action == nil {
		t.Fatalf("no contact details: expected a plain button: %+v", b)
	} else {
		b.Action()
	}
	cfg.SupportEmail = "help@contoso.example"
	if b := supportButton(cfg, a); b.Menu != nil {
		t.Fatalf("e-mail only: expected direct action: %+v", b)
	} else {
		b.Action()
	}
	cfg.SupportURL, cfg.SupportPhone = "https://help.contoso.example", "01 234"
	b := supportButton(cfg, a)
	if len(b.Menu) != 3 || b.Dismiss || b.Menu[0].Label != "Send an e-mail to help@contoso.example" || b.Menu[1].Label != "Visit the support website" || b.Menu[2].Label != "Call 01 234" {
		t.Fatalf("menu: %+v", b)
	}
	b.Menu[1].Action()
	if strings.Join(got, ",") != "info,email,web" {
		t.Errorf("actions: %v", got)
	}
}

func TestSupportMailto(t *testing.T) {
	s := Status{Configured: true, Orphans: []onedrive.Orphan{{Name: "Bids", Path: `C:\Users\d\Fabrikam\Bids & Co - Documents`}}}
	link := supportMailto(config.Defaults(), "help@contoso.example", s, true)
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "mailto" || u.Opaque != "help@contoso.example" {
		t.Fatalf("link %q: %v", link, err)
	}
	q := u.Query()
	if !strings.Contains(q.Get("subject"), "OneDrive is not running, 1 folder not syncing") {
		t.Errorf("subject: %q", q.Get("subject"))
	}
	if body := q.Get("body"); !strings.Contains(body, `Not syncing: C:\Users\d\Fabrikam\Bids & Co - Documents`) || !strings.Contains(body, "OneDrive is not running") {
		t.Errorf("body: %q", body)
	}
	if strings.Contains(link, "+") {
		t.Errorf("spaces must be %%20 for mail programs: %s", link)
	}
	if !strings.Contains(q.Get("body"), "Ctrl+V") {
		t.Errorf("paste prompt missing: %q", q.Get("body"))
	}
}

func TestSupportEML(t *testing.T) {
	s := Status{Configured: true, Running: true, Orphans: []onedrive.Orphan{{Name: "Bids", Path: `C:\Users\d\Fabrikam\Bids & Co - Documents`}}}
	report := "OneDrive Helper diagnostic report\r\nFolders not syncing (1):\r\n  - <Bids> & Co\r\n"
	msg, err := mail.ReadMessage(bytes.NewReader(supportEML(config.Defaults(), "help@contoso.example", s, report)))
	if err != nil {
		t.Fatal(err)
	}
	if msg.Header.Get("To") != "help@contoso.example" || msg.Header.Get("X-Unsent") != "1" {
		t.Errorf("headers: %v", msg.Header)
	}
	subj, _ := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if !strings.Contains(subj, "1 folder not syncing") {
		t.Errorf("subject: %q", subj)
	}
	_, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	mr := multipart.NewReader(msg.Body, params["boundary"])
	parts := map[string]string{}
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		raw, _ := io.ReadAll(base64.NewDecoder(base64.StdEncoding, p))
		ct, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		parts[ct] = string(raw)
	}
	if !strings.Contains(parts["text/plain"], "Not syncing: C:\\Users\\d\\Fabrikam\\Bids & Co - Documents") || !strings.Contains(parts["text/plain"], "<Bids> & Co") {
		t.Errorf("plain part: %q", parts["text/plain"])
	}
	h := parts["text/html"]
	if !strings.Contains(h, "font-size:8pt") || !strings.Contains(h, "Consolas") || !strings.Contains(h, "&lt;Bids&gt; &amp; Co") || strings.Contains(h, "<Bids>") {
		t.Errorf("html part must hold the escaped report in small monospace: %q", h)
	}
}
