package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	c := Defaults()
	if c.CheckIntervalSeconds != 60 || !c.OneDriveCheckEnabled || c.BackupRoot != `{Profile}\ODHelper\Saved` || !c.PopupSound || c.AccentColor != "#0F6CBD" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults must be valid: %v", err)
	}
	if c.Title() != "OneDrive Helper" || c.SupportContact() != "Please contact your IT support team." || c.SupportLink() != "" {
		t.Errorf("title/contact/link: %q %q %q", c.Title(), c.SupportContact(), c.SupportLink())
	}
	c.CompanyName, c.SupportName, c.SupportPhone, c.SupportEmail = "Contoso", "the Contoso Service Desk", "01 234", "help@contoso.example"
	if c.Title() != "Contoso - OneDrive Helper" {
		t.Errorf("title: %q", c.Title())
	}
	if got := c.SupportContact(); got != "Please contact the Contoso Service Desk (01 234 / help@contoso.example)." {
		t.Errorf("contact: %q", got)
	}
	if c.SupportLink() != "mailto:help@contoso.example" {
		t.Errorf("link should fall back to e-mail: %q", c.SupportLink())
	}
	c.SupportURL = "https://help.contoso.example"
	if c.SupportLink() != "https://help.contoso.example" {
		t.Errorf("link: %q", c.SupportLink())
	}
}

func TestBackupDir(t *testing.T) {
	c := Defaults()
	if got := c.BackupDir(`C:\Users\jsmith`); got != `C:\Users\jsmith\ODHelper\Saved` {
		t.Errorf("default: %s", got)
	}
	c.BackupRoot = `E:\ODHelper`
	if got := c.BackupDir(`C:\Users\jsmith`); got != `E:\ODHelper` {
		t.Errorf("fixed path: %s", got)
	}
}

func TestValidate(t *testing.T) {
	bad := Defaults()
	bad.SupportEmail = "not an email"
	bad.SupportURL = "javascript:alert(1)"
	bad.AccentColor = "blue"
	bad.IgnoreOptions = "5m,forever"
	bad.CheckIntervalSeconds = 1
	bad.BackupRoot = `relative\path`
	err := bad.Validate()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"SupportEmail", "SupportURL", "AccentColor", "forever", "CheckIntervalSeconds", "BackupRoot"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %s in %v", want, err)
		}
	}
}

func TestIgnoreChoices(t *testing.T) {
	c := Defaults()
	c.IgnoreOptions = " 15m, 2h ,reboot,bogus"
	if got := strings.Join(c.IgnoreChoices(), ","); got != "15m,2h,reboot" {
		t.Errorf("choices: %s", got)
	}
	for tok, want := range map[string]string{"15m": "Ignore for 15 minutes", "1h": "Ignore for 1 hour", "2h": "Ignore for 2 hours", "reboot": "Ignore until next restart"} {
		if got := IgnoreLabel(tok); got != want {
			t.Errorf("label(%s) = %q", tok, got)
		}
	}
	if d, err := ParseIgnore("30m"); err != nil || d != 30*time.Minute {
		t.Errorf("ParseIgnore: %v %v", d, err)
	}
	if _, err := ParseIgnore("48h"); err == nil {
		t.Error("48h should be rejected")
	}
}

func TestFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := Defaults()
	c.CompanyName, c.SupportEmail, c.OrphanMessage = "Contoso", "help@contoso.com", "Line 1\r\nLine 2"
	path := filepath.Join(dir, "s.json")
	if err := WriteFile(c, path); err != nil {
		t.Fatal(err)
	}
	got, present, err := ReadFile(path)
	if err != nil || got != c || len(present) != len(Fields()) {
		t.Fatalf("round trip: %v %d %+v", err, len(present), got)
	}

	// Partial file with a BOM: missing settings take defaults.
	partial := filepath.Join(dir, "p.json")
	_ = os.WriteFile(partial, []byte("\xef\xbb\xbf{\"CompanyName\": \"Fabrikam\"}"), 0o644)
	got, present, err = ReadFile(partial)
	if err != nil || got.CompanyName != "Fabrikam" || got.SupportName != Defaults().SupportName || len(present) != 1 {
		t.Fatalf("partial: %v %+v", err, got)
	}

	for name, body := range map[string]string{
		"unknown field": `{"Bogus": 1}`,
		"wrong type":    `{"CheckIntervalSeconds": "sixty"}`,
		"invalid value": `{"AccentColor": "red"}`,
		"not json":      `CompanyName=Contoso`,
	} {
		p := filepath.Join(dir, "bad.json")
		_ = os.WriteFile(p, []byte(body), 0o644)
		if _, _, err := ReadFile(p); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestGetSet(t *testing.T) {
	c := Defaults()
	if err := c.Set("PopupSound", "0"); err != nil || c.PopupSound || c.Get("PopupSound") != "0" {
		t.Errorf("bool: %v %v", err, c.PopupSound)
	}
	if err := c.Set("CheckIntervalSeconds", " 90 "); err != nil || c.CheckIntervalSeconds != 90 {
		t.Errorf("int: %v %d", err, c.CheckIntervalSeconds)
	}
	if err := c.Set("CheckIntervalSeconds", "abc"); err == nil {
		t.Error("non-number accepted")
	}
	if err := c.Set("NotRunningMessage", "  indented\r\nline  "); err != nil || c.NotRunningMessage != "  indented\r\nline  " {
		t.Errorf("multiline must be kept verbatim: %q", c.NotRunningMessage)
	}
	for _, f := range Fields() {
		if f.Label == "" || f.Help == "" {
			t.Errorf("%s needs label and help tags for the config tool", f.Name)
		}
	}
}
