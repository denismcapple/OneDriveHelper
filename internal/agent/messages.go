package agent

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
	"github.com/denismcapple/OneDriveHelper/internal/popup"
)

const (
	idNotRunning = "onedrive-not-running"
	idOrphans    = "orphaned-libraries"
	idFixing     = "fixing"

	defaultNotRunning = "Your files are NOT being synced or backed up. OneDrive has closed, possibly after an update.\n\n" +
		"Click 'Restart OneDrive' to fix this now."
	defaultAutoRestartFailed = "Your files are NOT being synced or backed up. OneDrive has closed, and it didn't start again " +
		"when we tried to restart it automatically.\n\n" +
		"Click 'Restart OneDrive' to try again. If it still won't start, please contact support."
	defaultOrphan = "Some folders on this PC are no longer syncing. Files saved in them are NOT uploaded or backed up."

	orphanTitle = "Problems found with your OneDrive / SharePoint sync"

	maxListed = 6
)

// Actions are the handlers behind the popup buttons.
type Actions struct {
	Relaunch    func()
	Snooze      func(alert, d string) // ignore one alert for a while
	Support     func()                // show the support contact details
	Email       func()                // start an e-mail to support
	Website     func()                // open the support website
	Fix         func()
	Explain     func(topic string)
	CopyDetails func()
}

func or(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// ignoreButton offers the configured durations; choosing one ignores the
// given alerts. ok is false when ignoring is disabled (no IgnoreOptions).
func ignoreButton(c config.Config, a Actions, alerts ...string) (popup.Button, bool) {
	choices := c.IgnoreChoices()
	if len(choices) == 0 {
		return popup.Button{}, false
	}
	var menu []popup.MenuItem
	for _, t := range choices {
		menu = append(menu, popup.MenuItem{Label: config.IgnoreLabel(t), Action: func() {
			for _, id := range alerts {
				a.Snooze(id, t)
			}
		}})
	}
	return popup.Button{Label: "Ignore", Menu: menu, Dismiss: true}, true
}

// supportButton asks how to contact support when there is more than one way.
func supportButton(c config.Config, a Actions) popup.Button {
	var menu []popup.MenuItem
	if addr := c.SupportEmailAddress(); addr != "" {
		menu = append(menu, popup.MenuItem{Label: "Send an e-mail to " + addr, Action: a.Email})
	}
	if c.SupportWebsite() != "" {
		menu = append(menu, popup.MenuItem{Label: "Visit the support website", Action: a.Website})
	}
	if c.SupportPhone != "" {
		menu = append(menu, popup.MenuItem{Label: "Call " + c.SupportPhone, Action: a.Support})
	}
	switch len(menu) {
	case 0:
		return popup.Button{Label: "Contact support", Action: a.Support}
	case 1:
		return popup.Button{Label: "Contact support", Action: menu[0].Action}
	}
	return popup.Button{Label: "Contact support", Menu: menu} // the card stays open
}

// notRunningSpec is the "OneDrive is not running" alert. autoFailed says the
// automatic restarts were tried and didn't work.
func notRunningSpec(c config.Config, a Actions, autoFailed bool) popup.Spec {
	body := defaultNotRunning
	if autoFailed {
		body = defaultAutoRestartFailed
	}
	s := popup.Spec{
		ID: idNotRunning, Severity: popup.Warning, Brand: c.Title(), Logo: c.LogoPath(), Sound: c.PopupSound, Accent: c.AccentColor,
		Title:  "OneDrive is not running",
		Body:   or(c.NotRunningMessage, body),
		OnHelp: func() { a.Explain(topicNotRunning) },
	}
	if b, ok := ignoreButton(c, a, idNotRunning); ok {
		s.Buttons = append(s.Buttons, b)
	}
	s.Buttons = append(s.Buttons, popup.Button{Label: "Restart OneDrive", Primary: true, Dismiss: true, Action: a.Relaunch})
	return s
}

// orphanGroup is one group of folders on the cards, e.g. "Old copies".
type orphanGroup struct {
	heading string
	orphans []onedrive.Orphan
}

// orphanGroups sorts folders by what became of their library. Folders whose
// library can't be identified are shown as stopped (as before 0.10).
func orphanGroups(orphans []onedrive.Orphan) []orphanGroup {
	var stopped, moved, gone, empty []onedrive.Orphan
	for _, o := range orphans {
		switch {
		case o.EmptyOrg:
			empty = append(empty, o)
		case o.Kind == onedrive.KindMoved:
			moved = append(moved, o)
		case o.Kind == onedrive.KindDisconnected:
			gone = append(gone, o)
		default:
			stopped = append(stopped, o)
		}
	}
	movedHeading := "Old copies: these libraries now sync to a new folder"
	if len(moved) > 0 {
		org := filepath.Base(filepath.Dir(moved[0].MovedTo))
		for _, o := range moved[1:] {
			if !strings.EqualFold(filepath.Base(filepath.Dir(o.MovedTo)), org) {
				org = ""
			}
		}
		if org != "" && org != "." && org != `\` {
			movedHeading = fmt.Sprintf("Old copies: these libraries now sync to %q", org)
		}
	}
	var out []orphanGroup
	for _, g := range []orphanGroup{
		{"Stopped syncing", stopped},
		{movedHeading, moved},
		{"From an account that is no longer connected", gone},
		{"Empty leftover folders", empty},
	} {
		if len(g.orphans) > 0 {
			g.heading = fmt.Sprintf("%s (%d)", g.heading, len(g.orphans))
			out = append(out, g)
		}
	}
	return out
}

// orphanItems lists folders by group: a heading, then name + path (+ note)
// for each folder, at most maxListed folders in all.
func orphanItems(orphans []onedrive.Orphan, note func(onedrive.Orphan) string) ([]popup.Item, string) {
	var items []popup.Item
	listed := 0
	for _, g := range orphanGroups(orphans) {
		if listed == maxListed {
			break
		}
		items = append(items, popup.Item{Title: g.heading, Heading: true})
		for _, o := range g.orphans {
			if listed == maxListed {
				break
			}
			it := popup.Item{Title: "•  " + o.Name, Path: o.Path}
			if note != nil {
				it.Note = note(o)
			}
			items = append(items, it)
			listed++
		}
	}
	if listed < len(orphans) {
		return items, fmt.Sprintf("…and %d more.", len(orphans)-listed)
	}
	return items, ""
}

func orphanSpec(c config.Config, orphans []onedrive.Orphan, a Actions) popup.Spec {
	items, more := orphanItems(orphans, nil)
	footer := c.SupportContact()
	if more != "" {
		footer = more + "\n\n" + footer
	}
	s := popup.Spec{
		ID: idOrphans, Severity: popup.Critical, Brand: c.Title(), Logo: c.LogoPath(), Sound: c.PopupSound, Accent: c.AccentColor,
		Title: orphanTitle, Body: or(c.OrphanMessage, defaultOrphan), Items: items, Footer: footer,
		OnHelp: func() { a.Explain(topicOrphans) },
	}
	if b, ok := ignoreButton(c, a, idOrphans); ok {
		s.Buttons = append(s.Buttons, b)
	}
	s.Buttons = append(s.Buttons, supportButton(c, a))
	if c.RemediationEnabled {
		s.Buttons = append(s.Buttons, popup.Button{Label: "Fix (admin)", Primary: true, Dismiss: true, Action: a.Fix})
	}
	return s
}

func fixingSpec(c config.Config) popup.Spec {
	return popup.Spec{
		ID: idFixing, Severity: popup.Info, Brand: c.Title(), Logo: c.LogoPath(), Accent: c.AccentColor,
		Title: "Fixing your library folders…",
		Body: "Moving the folders to a safe place and saving a copy of your files. " +
			"Please save new work somewhere else until this finishes.",
	}
}

// SampleSpecs returns both alerts with the given actions (used by previews).
func SampleSpecs(c config.Config, orphans []onedrive.Orphan, a Actions) []popup.Spec {
	return []popup.Spec{orphanSpec(c, orphans, a), notRunningSpec(c, a, c.AutoRestartEnabled)}
}
