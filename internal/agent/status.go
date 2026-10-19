package agent

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
	"github.com/denismcapple/OneDriveHelper/internal/popup"
	"github.com/denismcapple/OneDriveHelper/internal/remediate"
	"github.com/denismcapple/OneDriveHelper/internal/version"
	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

const idStatus = "status"

// Status is a snapshot of everything the agent knows.
type Status struct {
	At           time.Time
	Accounts     []onedrive.Account
	Configured   bool
	Running      bool // last known state when Unknown is set
	Unknown      bool // the process list could not be read
	Updating     bool
	MissingSince time.Time  // zero unless OneDrive has been seen missing
	Snoozed      bool       // the "OneDrive not running" alert is ignored
	OrphanSnooze SnoozeInfo // the "folders not syncing" alert is ignored
	Orphans      []onedrive.Orphan
	StaleNav     []onedrive.NavEntry // Explorer side-panel entries pointing at missing folders
	Session      uint32
	// Interrupted lists earlier fixes that did not finish (folders not where they were).
	Interrupted []remediate.Interrupted
}

// NotRunning reports a OneDrive outage worth telling the user about.
func (s Status) NotRunning() bool { return s.Configured && !s.Running && !s.Updating && !s.Unknown }

// unfinished counts interrupted fixes that are not still running.
func (s Status) unfinished() int {
	n := 0
	for _, i := range s.Interrupted {
		if !i.Running {
			n++
		}
	}
	return n
}

// Problems counts what needs attention.
func (s Status) Problems() int {
	n := len(s.Orphans) + s.unfinished()
	if s.NotRunning() {
		n++
	}
	return n
}

func (s Status) severity() popup.Severity {
	switch {
	case len(s.Orphans) > 0 || s.unfinished() > 0:
		return popup.Critical
	case s.NotRunning():
		return popup.Warning
	}
	return popup.OK
}

// summary is a one-line description (tray tooltip).
func (s Status) summary() string {
	var parts []string
	if s.NotRunning() {
		parts = append(parts, "OneDrive is not running")
	}
	if s.unfinished() > 0 {
		parts = append(parts, "an earlier fix did not finish")
	}
	switch n := len(s.Orphans); {
	case n == 1:
		parts = append(parts, "1 folder not syncing")
	case n > 1:
		parts = append(parts, fmt.Sprintf("%d folders not syncing", n))
	}
	if len(parts) == 0 {
		if !s.Configured {
			return "OneDrive is not set up for this user"
		}
		return "Everything looks good"
	}
	return strings.Join(parts, ", ")
}

// collect gathers a fresh status (no side effects).
func (a *Agent) collect(cfg config.Config) Status {
	accts := onedrive.Accounts(a.hive)
	s := Status{
		At: time.Now(), Accounts: accts, Configured: onedrive.AnyConfigured(accts),
		Updating:     onedrive.UpdateInProgress(),
		Snoozed:      Snoozed(a.hive, idNotRunning, time.Now(), winapi.BootTime()),
		OrphanSnooze: Snooze(a.hive, idOrphans, time.Now(), winapi.BootTime()),
		Session:      a.session,
	}
	if cfg.OrphanCheckEnabled {
		in := onedrive.Gather(a.hive, a.profile)
		s.Orphans = onedrive.FindOrphans(in, onedrive.OSFS)
		s.StaleNav = onedrive.StaleNavEntries(a.hive, in, nil)
	}
	s.Interrupted = remediate.ListInterrupted(a.profile)
	a.mu.Lock()
	switch a.oneDriveState() {
	case onedrive.StateRunning:
		s.Running = true
	case onedrive.StateUnknown:
		s.Unknown, s.Running = true, a.last.Running // show the last known state
	}
	s.MissingSince = a.missingSince
	a.mu.Unlock()
	return s
}

func statusSpec(c config.Config, s Status, a Actions) popup.Spec {
	var b strings.Builder
	switch {
	case !s.Configured:
		b.WriteString("OneDrive:  not set up for this user.")
	case s.Unknown:
		b.WriteString("OneDrive:  can't tell right now (the list of running programs could not be read). Last known: ")
		if s.Running {
			b.WriteString("running.")
		} else {
			b.WriteString("not running.")
		}
	case s.Running:
		b.WriteString("OneDrive:  running.")
	case s.Updating:
		b.WriteString("OneDrive:  updating - it should start again by itself.")
	default:
		b.WriteString("OneDrive:  NOT running")
		if !s.MissingSince.IsZero() {
			b.WriteString(" (since " + s.MissingSince.Format("15:04") + ")")
		}
		b.WriteString(".")
		if s.Snoozed {
			b.WriteString(" The alert is being ignored for now.")
		}
	}
	var items []popup.Item
	footer := ""
	if len(s.Orphans) == 0 {
		b.WriteString("\nShared folders:  all syncing.")
	} else {
		fmt.Fprintf(&b, "\n\nFolders that have stopped syncing (%d):", len(s.Orphans))
		items, footer = orphanItems(s.Orphans, remediate.ResyncNote)
		if s.OrphanSnooze.Active {
			footer = strings.TrimSpace(footer + "\n\nThe alert for these folders is ignored " + s.OrphanSnooze.Describe() + ".")
		}
	}

	for _, i := range s.Interrupted {
		if !i.Running {
			b.WriteString("\n\nAn earlier fix did not finish. These folders are not where they were:\n" + strings.TrimSpace(i.Text()))
		}
	}

	title := "Everything looks good"
	if n := s.Problems(); n == 1 {
		title = "1 problem needs attention"
	} else if n > 1 {
		title = fmt.Sprintf("%d problems need attention", n)
	}
	spec := popup.Spec{
		ID: idStatus, Severity: s.severity(), Brand: c.Title(), Accent: c.AccentColor,
		Title: title, Body: b.String(), Items: items, Footer: footer, OnHelp: func() { a.Explain(topicStatus) },
	}
	// "Ignore" applies to the alerts for the problems shown here.
	var alerts []string
	if s.NotRunning() {
		alerts = append(alerts, idNotRunning)
	}
	if len(s.Orphans) > 0 {
		alerts = append(alerts, idOrphans)
	}
	if ig, ok := ignoreButton(c, a, alerts...); ok && len(alerts) > 0 {
		spec.Buttons = append(spec.Buttons, ig)
	} else {
		spec.Buttons = append(spec.Buttons, popup.Button{Label: "Close", Dismiss: true})
	}
	spec.Buttons = append(spec.Buttons, popup.Button{Label: "Copy details", Action: a.CopyDetails}, supportButton(c, a))
	if s.NotRunning() {
		spec.Buttons = append(spec.Buttons, popup.Button{Label: "Restart OneDrive", Primary: true, Dismiss: true, Action: a.Relaunch})
	}
	if len(s.Orphans) > 0 && c.RemediationEnabled {
		spec.Buttons = append(spec.Buttons, popup.Button{Label: "Fix (admin)", Primary: true, Dismiss: true, Action: a.Fix})
	}
	return spec
}

// Report is the diagnostic text copied for support tickets.
func Report(c config.Config, s Status, logPath string) string {
	var b strings.Builder
	host, _ := os.Hostname()
	fmt.Fprintf(&b, "OneDrive Helper diagnostic report\r\n")
	fmt.Fprintf(&b, "Generated:  %s\r\n", s.At.Format("2006-01-02 15:04:05 -07:00"))
	fmt.Fprintf(&b, "Computer:   %s\r\nUser:       %s (session %d)\r\nVersion:    %s\r\nCompany:    %s\r\n",
		host, winapi.CurrentUserName(), s.Session, version.Version, c.CompanyName)
	fmt.Fprintf(&b, "Summary:    %s\r\n\r\n", s.summary())
	fmt.Fprintf(&b, "OneDrive set up: %v   running: %v (unknown: %v)   updating: %v   'not running' alert ignored: %v   'folders' alert ignored: %v\r\n",
		s.Configured, s.Running, s.Unknown, s.Updating, s.Snoozed, s.OrphanSnooze.Active)
	if !s.MissingSince.IsZero() {
		fmt.Fprintf(&b, "Missing since: %s\r\n", s.MissingSince.Format(time.RFC3339))
	}
	b.WriteString("\r\nAccounts:\r\n")
	for _, a := range s.Accounts {
		if !a.Configured() {
			continue
		}
		fmt.Fprintf(&b, "  %s  %s  %s\r\n", a.Key, a.UserEmail, a.UserFolder)
		for _, f := range a.SyncedFolders {
			fmt.Fprintf(&b, "    synced: %s\r\n", f)
		}
	}
	fmt.Fprintf(&b, "\r\nFolders not syncing (%d):\r\n", len(s.Orphans))
	for _, o := range s.Orphans {
		fmt.Fprintf(&b, "  - %s\r\n    path:    %s\r\n    re-sync: %s\r\n", o.DisplayName(), o.Path, remediate.ResyncNote(o))
		if o.WebURL != "" {
			fmt.Fprintf(&b, "    site:    %s\r\n", o.WebURL)
		}
	}
	if len(s.Interrupted) > 0 {
		b.WriteString("\r\nUnfinished fixes:\r\n")
		for _, i := range s.Interrupted {
			b.WriteString(strings.ReplaceAll("  "+strings.TrimSpace(i.Text()), "\n", "\r\n  ") + "\r\n")
		}
	}
	if len(s.StaleNav) > 0 {
		b.WriteString("\r\nExplorer side-panel entries pointing at missing folders (removed automatically after 5 minutes):\r\n")
		for _, e := range s.StaleNav {
			fmt.Fprintf(&b, "  - %s -> %s\r\n", e.Name, e.Target)
		}
	}
	if data, err := os.ReadFile(logPath); err == nil {
		lines := strings.Split(strings.TrimRight(string(data), "\r\n"), "\n")
		if len(lines) > 20 {
			lines = lines[len(lines)-20:]
		}
		b.WriteString("\r\nRecent agent log:\r\n")
		for _, l := range lines {
			b.WriteString("  " + strings.TrimRight(l, "\r") + "\r\n")
		}
	}
	return b.String()
}

// PreviewStatus shows the tray icon, the status card and both alerts with
// sample data (used for visual checks).
func PreviewStatus(ui *popup.Manager, c config.Config) {
	s := Status{At: time.Now(), Configured: true, Running: true, Orphans: []onedrive.Orphan{
		{Name: "OneDrive - Contoso Ltd", Path: `C:\Users\jsmith\OneDrive - Contoso Ltd`,
			TenantRoot: `C:\Users\jsmith\OneDrive - Contoso Ltd`, Personal: true},
		{Name: "Bids Team - Documents", Path: `C:\Users\jsmith\Fabrikam\Bids Team - Documents`, TenantRoot: `C:\Users\jsmith\Fabrikam`},
	}}
	nop := Actions{Relaunch: func() {}, Snooze: func(string, string) {}, Support: func() {}, Email: func() {}, Website: func() {},
		Fix: func() {}, Explain: func(string) {}, CopyDetails: func() {}}
	ui.SetTray(popup.Tray{Tooltip: trayTooltip(c, s), Severity: s.severity()})
	ui.Show(orphanSpec(c, s.Orphans, nop))
	ui.Show(statusSpec(c, s, nop))
}

// --- tray ---

// trayTooltip is shown when hovering over the tray icon, e.g.
// "Contoso - OneDrive Helper v0.7.1" / "Everything looks good".
// (Windows shows at most 127 characters; the summary is last so the name and
// version are never cut off.)
func trayTooltip(c config.Config, s Status) string {
	return c.Title() + " v" + version.Version + "\n" + s.summary()
}

func (a *Agent) updateTray(cfg config.Config, s Status) {
	if !cfg.TrayIconEnabled {
		a.ui.RemoveTray()
		return
	}
	a.ui.SetTray(popup.Tray{
		Tooltip:  trayTooltip(cfg, s),
		Severity: s.severity(),
		OnClick:  a.showStatus,
		Menu:     a.trayMenu,
	})
	if a.ui.Visible(idStatus) { // keep an open status card current
		a.ui.Show(statusSpec(cfg, s, a.actions()))
	}
}

func (a *Agent) trayMenu() []popup.TrayItem {
	cfg := config.Load()
	a.mu.Lock()
	s := a.last
	a.mu.Unlock()
	saved := cfg.BackupDir(a.profile)
	_, err := os.Stat(saved)
	items := []popup.TrayItem{
		{Label: "Show status", Action: a.showStatus},
		{Label: "Show alerts again", Action: a.reshowAlerts},
		{Separator: true},
		{Label: "Restart OneDrive", Action: a.relaunch, Disabled: !s.NotRunning()},
		{Label: "Fix folders that stopped syncing (admin)...", Action: a.fix, Disabled: len(s.Orphans) == 0 || !cfg.RemediationEnabled},
		{Separator: true},
	}
	if addr := cfg.SupportEmailAddress(); addr != "" {
		items = append(items, popup.TrayItem{Label: "E-mail support (" + addr + ")", Action: a.email})
	}
	if cfg.SupportWebsite() != "" {
		items = append(items, popup.TrayItem{Label: "Visit the support website", Action: a.website})
	}
	if cfg.SupportEmailAddress() == "" && cfg.SupportWebsite() == "" {
		items = append(items, popup.TrayItem{Label: "Contact support", Action: a.support})
	}
	return append(items, []popup.TrayItem{
		{Label: "Copy details for support", Action: a.copyDetails},
		{Label: "Open saved files", Action: func() { _ = winapi.ShellOpen(saved) }, Disabled: err != nil},
		{Label: "What does this mean?", Action: func() { a.explain(topicStatus) }},
		{Separator: true},
		{Label: cfg.Title() + " " + version.Version, Disabled: true},
	}...)
}

// supportMailto builds a mailto: link with a subject and a short, plain
// description of the problems (kept short: mail programs truncate long links).
// reportCopied adds a prompt to paste the full report from the clipboard.
func supportMailto(c config.Config, addr string, s Status, reportCopied bool) string {
	host, _ := os.Hostname()
	var b strings.Builder
	b.WriteString("Hello,\r\n\r\nOneDrive Helper on my computer reports a problem:\r\n\r\n")
	if s.NotRunning() {
		b.WriteString("- OneDrive is not running\r\n")
	}
	for i, o := range s.Orphans {
		if i == 10 {
			fmt.Fprintf(&b, "- ...and %d more folders\r\n", len(s.Orphans)-10)
			break
		}
		fmt.Fprintf(&b, "- Not syncing: %s\r\n", o.Path)
	}
	if s.Problems() == 0 {
		b.WriteString("- (no problems detected right now)\r\n")
	}
	fmt.Fprintf(&b, "\r\nComputer: %s\r\nUser: %s\r\n\r\n", host, winapi.CurrentUserName())
	if reportCopied {
		b.WriteString("Diagnostic details (press Ctrl+V here to paste them):\r\n\r\n")
	} else {
		b.WriteString("(For the full diagnostic report, use 'Copy details for support' on the OneDrive Helper icon and paste it here.)\r\n")
	}
	enc := func(v string) string { return strings.ReplaceAll(url.QueryEscape(v), "+", "%20") }
	subject := "OneDrive problem on " + host + ": " + s.summary()
	return "mailto:" + addr + "?subject=" + enc(subject) + "&body=" + enc(b.String())
}

// showStatus refreshes and shows the status card.
func (a *Agent) showStatus() {
	cfg := config.Load()
	s := a.collect(cfg)
	a.mu.Lock()
	a.last = s
	a.mu.Unlock()
	a.ui.Show(statusSpec(cfg, s, a.actions()))
}

// reshowAlerts brings back alerts the user dismissed or ignored.
func (a *Agent) reshowAlerts() {
	ClearSnooze(a.hive)
	a.mu.Lock()
	a.orphanKey = ""
	if !a.missingSince.IsZero() {
		a.missingSince = time.Now().Add(-24 * time.Hour) // skip the grace period
	}
	a.mu.Unlock()
	a.tick(config.Load(), time.Now())
}

func (a *Agent) copyDetails() {
	cfg := config.Load()
	s := a.collect(cfg)
	if err := a.ui.CopyText(Report(cfg, s, LogPath())); err != nil {
		winapi.Error(cfg.Title(), "Could not copy the details: "+err.Error())
		return
	}
	winapi.Info(cfg.Title(), "The details have been copied.\n\nPaste them (Ctrl+V) into your e-mail or support ticket.")
}

func (a *Agent) explain(topic string) {
	cfg := config.Load()
	a.mu.Lock()
	orphans := a.last.Orphans
	a.mu.Unlock()
	winapi.Info(explainTitle, Explain(cfg, topic, orphans))
}
