package agent

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/dialog"
	"github.com/denismcapple/OneDriveHelper/internal/elog"
	"github.com/denismcapple/OneDriveHelper/internal/odstatus"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
	"github.com/denismcapple/OneDriveHelper/internal/popup"
	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

const (
	idIssues    = "onedrive-issues"
	issuesTitle = "OneDrive needs your attention"

	// shortGrace: errors and a hung OneDrive must last this long (two or
	// more checks) before the user hears about them.
	shortGrace = 2 * time.Minute
	// statusTimeout: OneDrive normally answers within a second.
	statusTimeout = 10 * time.Second

	textNotResponding = "OneDrive is running but has stopped responding. Your files are not being synced, and files that are only in the cloud won't open."
	textDisconnected  = "OneDrive isn't connected to this. It may need you to sign in again."
	textProviderError = "OneDrive reported a problem syncing this."
	textOffline       = "OneDrive can't connect to the internet, so nothing is being synced."
	textPaused        = "Syncing is paused."
)

// Observation is one thing seen wrong in a single check, before grace
// periods are applied.
type Observation struct {
	Key     string // what is wrong, the same for every library it affects
	Text    string // what to tell the user (OneDrive's own words where it gives them)
	Library string // the affected library or OneDrive folder ("" = OneDrive as a whole)
	RootID  string // sync root whose OneDrive command fixes it
	Action  string // label of OneDrive's own button for it ("" = none)
	Restart bool   // offer "Restart OneDrive" (it has stopped responding)
	Slow    bool   // paused / offline / not connected: wait for the longer grace
}

// Problem is what the user is told: one per distinct issue, with the
// libraries it affects.
type Problem struct {
	Key, Text, RootID, Action string
	Restart                   bool
	Libraries                 []string
}

// observe reads OneDrive's status for the user: OneDrive's own status on
// Windows 11 / Server 2025, otherwise the Cloud Files driver's view of each
// of the user's sync roots.
func observe(sid string) []Observation {
	roots, err := odstatus.Read(sid, statusTimeout)
	switch {
	case errors.Is(err, odstatus.ErrTimeout):
		return []Observation{{Key: "not-responding", Text: textNotResponding, Restart: true}}
	case err == nil:
		return fromStatus(roots)
	}
	var out []Observation
	for _, p := range onedrive.RegisteredSyncRoots(sid) {
		if info, err := winapi.SyncRootInfo(p); err == nil {
			out = append(out, fromProvider(p, info.Status)...)
		}
	}
	return out
}

// fromStatus turns OneDrive's own status into observations.
func fromStatus(roots []odstatus.Root) []Observation {
	var out []Observation
	for _, r := range roots {
		if r.Err != "" {
			continue
		}
		o := Observation{Library: r.Name, RootID: r.ID, Action: r.Action}
		text := strings.TrimSpace(r.Message)
		if text == "" {
			text = strings.TrimSpace(r.Status)
		}
		switch r.State {
		case odstatus.Error, odstatus.Warning:
			o.Text = or(text, textProviderError)
		case odstatus.Paused:
			o.Text, o.Slow = or(text, textPaused), true
		case odstatus.Offline:
			o.Text, o.Slow = or(text, textOffline), true
		default:
			continue
		}
		o.Key = r.Account() + "|" + r.State.String() + "|" + o.Text
		out = append(out, o)
	}
	return out
}

// Cloud Files provider status (CF_SYNC_PROVIDER_STATUS) values that matter.
const (
	providerDisconnected     = 0x0
	providerConnectivityLost = 0x40
	providerTerminated       = 0xFFFFFFFE
	providerError            = 0xFFFFFFFF
)

// fromProvider is the fallback without OneDrive's own status (Windows 10,
// Server 2019 / 2022): what the Cloud Files driver knows about a sync root.
func fromProvider(path string, status uint32) []Observation {
	o := Observation{Library: libraryName(path)}
	switch {
	case status == providerDisconnected:
		o.Key, o.Text, o.Slow = "disconnected", textDisconnected, true
	case status == providerError || status == providerTerminated:
		o.Key, o.Text = "provider-error", textProviderError
	case status&providerConnectivityLost != 0:
		o.Key, o.Text, o.Slow = "offline", textOffline, true
	default:
		return nil
	}
	return []Observation{o}
}

func libraryName(path string) string {
	path = strings.TrimRight(path, `\`)
	if i := strings.LastIndex(path, `\`); i >= 0 {
		return path[i+1:]
	}
	return path
}

// tracker applies the grace periods: an observation becomes a problem once
// it has been seen in every check for long enough.
type tracker struct {
	since map[string]time.Time // observation key + library -> first seen
}

func obsID(o Observation) string { return o.Key + "\x00" + o.Library }

// update records this check's observations and returns the problems that
// are due, grouped by issue, in a stable order.
func (t *tracker) update(obs []Observation, now time.Time, slowGrace time.Duration) []Problem {
	seen := map[string]time.Time{}
	byKey := map[string]*Problem{}
	var order []string
	for _, o := range obs {
		id := obsID(o)
		first, ok := t.since[id]
		if !ok {
			first = now
		}
		seen[id] = first
		grace := shortGrace
		if o.Slow {
			grace = slowGrace
		}
		if now.Sub(first) < grace {
			continue
		}
		p := byKey[o.Key]
		if p == nil {
			p = &Problem{Key: o.Key, Text: o.Text, RootID: o.RootID, Action: o.Action, Restart: o.Restart}
			byKey[o.Key] = p
			order = append(order, o.Key)
		}
		if o.Library != "" {
			p.Libraries = append(p.Libraries, o.Library)
		}
	}
	t.since = seen
	sort.Strings(order)
	out := make([]Problem, 0, len(order))
	for _, k := range order {
		sort.Strings(byKey[k].Libraries)
		out = append(out, *byKey[k])
	}
	return out
}

func problemsKey(ps []Problem) string {
	var parts []string
	for _, p := range ps {
		parts = append(parts, p.Key+"="+strings.Join(p.Libraries, ","))
	}
	return strings.Join(parts, "|")
}

// checkIssues alerts about problems OneDrive reports. It only runs while
// OneDrive is running (the "not running" alert covers the rest).
func (a *Agent) checkIssues(cfg config.Config, now time.Time, running bool) []Problem {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !cfg.OneDriveIssueAlerts || !running || a.fixing {
		a.issues.since = nil
		if a.issueKey != "" && !a.fixing {
			a.issueKey = ""
			a.ui.Hide(idIssues)
		}
		return nil
	}
	read := a.observe
	if read == nil {
		read = observe
	}
	problems := a.issues.update(read(a.hive.SID), now, cfg.IssueGrace())
	key := problemsKey(problems)
	if len(problems) == 0 {
		if a.issueKey != "" {
			a.ev.Info(elog.OneDriveProblemsResolved, "OneDrive no longer reports a problem for "+a.user+".")
			a.log.Printf("OneDrive problems resolved")
		}
		a.issueKey = ""
		a.ui.Hide(idIssues)
		return nil
	}
	changed := key != a.issueKey
	if changed {
		if a.issueKey != "" {
			ClearSnooze(a.hive, idIssues) // a new problem overrides "Ignore"
		}
		a.issueKey = key
		for _, p := range problems {
			id, what := uint32(elog.OneDriveProblem), "OneDrive reports a problem"
			if p.Restart {
				id, what = elog.OneDriveNotResponding, "OneDrive is running but not responding"
			}
			msg := fmt.Sprintf("%s for %s: %s", what, a.user, p.Text)
			if len(p.Libraries) > 0 {
				msg += "\r\nAffected: " + strings.Join(p.Libraries, ", ")
			}
			a.log.Printf("%s", strings.ReplaceAll(msg, "\r\n", "; "))
			a.warnEvent(id, msg)
		}
	}
	if Snoozed(a.hive, idIssues, now, winapi.BootTime()) {
		a.ui.Hide(idIssues)
		return problems
	}
	if !a.ui.Visible(idIssues) || changed {
		a.ui.Show(issuesSpec(cfg, problems, a.actions()))
	}
	return problems
}

// issueItems lists each problem with the libraries it affects.
func issueItems(problems []Problem) []popup.Item {
	var items []popup.Item
	for _, p := range problems {
		it := popup.Item{Title: "•  " + p.Text, Plain: true}
		if len(p.Libraries) > 0 {
			it.Note = strings.Join(p.Libraries, ", ")
		}
		items = append(items, it)
	}
	return items
}

func issuesSpec(c config.Config, problems []Problem, a Actions) popup.Spec {
	s := popup.Spec{
		ID: idIssues, Severity: popup.Warning, Brand: c.Title(), Logo: c.LogoPath(), Sound: c.PopupSound, Accent: c.AccentColor,
		Title: issuesTitle, Body: "OneDrive has a problem that it is only showing on its own icon:", Items: issueItems(problems),
		OnHelp: func() { a.Explain(topicIssues) },
	}
	if b, ok := ignoreButton(c, a, idIssues); ok {
		s.Buttons = append(s.Buttons, b)
	}
	s.Buttons = append(s.Buttons, supportButton(c, a))
	if b, ok := issueButton(problems, a); ok {
		s.Buttons = append(s.Buttons, b)
	}
	return s
}

// issueButton is the fix for the first problem: restart a hung OneDrive,
// run OneDrive's own command (e.g. "Sign in", "View sync issues"), or open
// OneDrive so it can show what it wants.
func issueButton(problems []Problem, a Actions) (popup.Button, bool) {
	if len(problems) == 0 || a.OneDrive == nil {
		return popup.Button{}, false
	}
	for _, p := range problems {
		if p.Restart {
			return popup.Button{Label: "Restart OneDrive", Primary: true, Dismiss: true, Action: func() { a.OneDrive(p) }}, true
		}
	}
	p := problems[0]
	label := p.Action
	if label == "" {
		label = "Open OneDrive"
	}
	return popup.Button{Label: label, Primary: true, Dismiss: true, Action: func() { a.OneDrive(p) }}, true
}

// fixIssue runs the problem alert's button.
func (a *Agent) fixIssue(p Problem) {
	switch {
	case p.Restart:
		a.log.Printf("restarting OneDrive (not responding), asked by the user")
		if err := onedrive.RestartOneDrive(a.hive, a.session); err != nil {
			a.log.Printf("restart OneDrive: %v", err)
			dialog.Error("OneDrive could not be restarted", "OneDrive could not be restarted: "+err.Error()+"\n\n"+config.Load().SupportContact())
			return
		}
		a.ev.Info(elog.OneDriveRestartedByUser, "OneDrive was restarted by "+a.user+" from the OneDrive Helper alert because it had stopped responding.")
	case p.RootID != "":
		if ran, err := odstatus.Invoke(a.hive.SID, p.RootID, statusTimeout); err == nil && ran {
			return
		} else if err != nil {
			a.log.Printf("OneDrive command for %s: %v", p.RootID, err)
		}
		fallthrough
	default:
		if err := onedrive.OpenOneDrive(a.hive); err != nil {
			a.log.Printf("open OneDrive: %v", err)
		}
	}
	a.refresh()
}
