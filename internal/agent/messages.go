package agent

import (
	"strings"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
	"github.com/denismcapple/OneDriveHelper/internal/popup"
	"github.com/denismcapple/OneDriveHelper/internal/remediate"
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
	defaultOrphan = "Files saved in these folders are NOT uploaded or backed up."

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

// orphanItems lists the folders by group (see remediate.Items). The "We
// will..." line under each heading is left out when fixing is turned off.
func orphanItems(c config.Config, orphans []onedrive.Orphan) ([]popup.Item, string) {
	items, more := remediate.Items(orphans, maxListed)
	if !c.RemediationEnabled {
		for i := range items {
			if items[i].Heading {
				items[i].Note = ""
			}
		}
	}
	return items, more
}

func orphanSpec(c config.Config, orphans []onedrive.Orphan, a Actions) popup.Spec {
	items, more := orphanItems(c, orphans)
	s := popup.Spec{
		ID: idOrphans, Severity: popup.Critical, Brand: c.Title(), Logo: c.LogoPath(), Sound: c.PopupSound, Accent: c.AccentColor,
		Title: orphanTitle, Body: or(c.OrphanMessage, defaultOrphan), Items: items, Footer: more,
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
		Body: "Moving the folders to a safe place, and saving a copy of your files. " +
			"Please wait while this completes and do not save any new work.",
	}
}

// SampleSpecs returns both alerts with the given actions (used by previews).
func SampleSpecs(c config.Config, orphans []onedrive.Orphan, a Actions) []popup.Spec {
	return []popup.Spec{orphanSpec(c, orphans, a), notRunningSpec(c, a, c.AutoRestartEnabled)}
}
