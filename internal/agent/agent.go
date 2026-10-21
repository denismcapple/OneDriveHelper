// Package agent is the per-user component. It runs in the user's session,
// checks OneDrive's health and shows OneDrive Helper's own alert popups.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/dialog"
	"github.com/denismcapple/OneDriveHelper/internal/elog"
	"github.com/denismcapple/OneDriveHelper/internal/ipc"
	"github.com/denismcapple/OneDriveHelper/internal/logx"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
	"github.com/denismcapple/OneDriveHelper/internal/popup"
	"github.com/denismcapple/OneDriveHelper/internal/remediate"
	"github.com/denismcapple/OneDriveHelper/internal/version"
	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

// LogPath returns the per-user agent log file.
func LogPath() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "OneDriveHelper", "agent.log")
}

type Agent struct {
	log     *log.Logger
	ev      *elog.Log
	ui      *popup.Manager
	hive    onedrive.Hive
	session uint32
	profile string
	appData string
	user    string
	launch  func() error // starts OneDrive (replaceable in tests)
	// stopping reports that the service has asked agents to exit (or is down):
	// no fix may start, and a running fix moves no further folders.
	stopping func() bool
	// procState, onEvent, userFlow and afterPanic are replaced in tests.
	procState  func() onedrive.ProcState
	onEvent    func(id uint32)
	userFlow   func(exe string, lg *log.Logger, busy func(bool), stop func() bool)
	afterPanic func()
	// wake asks the check loop to run now instead of at the next interval
	// (after a fix, or when the status card is opened). See refresh.
	wake chan struct{}

	mu            sync.Mutex // guards the fields below (buttons run on other goroutines)
	missingSince  time.Time
	outageLogged  bool
	restarts      []time.Time // automatic restart attempts (kept for autoRestartWindow)
	autoGaveUp    bool        // automatic restarts exhausted for the current outage
	lastRunning   time.Time   // last time OneDrive was seen running
	unknownStreak int         // failed process listings in a row
	orphanKey     string
	fixing        bool
	last          Status // latest check, for the tray menu and explanations
	staleSince    map[string]time.Time
}

// staleNavGrace is how long an Explorer entry must point at a missing folder
// before it is removed (avoids racing OneDrive while it sets a folder up).
const staleNavGrace = 5 * time.Minute

// cleanNavPane removes OneDrive Explorer side-panel entries whose folder has
// been missing for staleNavGrace.
func (a *Agent) cleanNavPane(stale []onedrive.NavEntry, now time.Time) {
	seen := map[string]time.Time{}
	removed := false
	for _, e := range stale {
		first, ok := a.staleSince[e.CLSID]
		if !ok {
			first = now
		}
		if now.Sub(first) < staleNavGrace {
			seen[e.CLSID] = first
			continue
		}
		if err := onedrive.RemoveNavEntry(a.hive, e); err != nil {
			a.log.Printf("remove Explorer entry %q (%s): %v", e.Name, e.Target, err)
			seen[e.CLSID] = first
			continue
		}
		removed = true
		a.log.Printf("removed Explorer entry %q: its folder %s no longer exists", e.Name, e.Target)
	}
	a.staleSince = seen
	if removed {
		onedrive.RefreshExplorer()
	}
}

// Run runs the agent until ctx is cancelled. Unless standalone is set, the
// agent also exits when the OneDriveHelper service stops (e.g. uninstall or
// upgrade): immediately when stopEvent (passed by the service) is signalled,
// otherwise when polling finds the service stopped. A fix in progress is
// always allowed to finish first.
func Run(ctx context.Context, standalone bool, stopEvent string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if stopEvent != "" {
		n, _ := windows.UTF16PtrFromString(stopEvent)
		if ev, err := windows.OpenEvent(windows.SYNCHRONIZE, false, n); err == nil {
			go func() {
				defer windows.CloseHandle(ev)
				if r, _ := windows.WaitForSingleObject(ev, windows.INFINITE); r == windows.WAIT_OBJECT_0 {
					cancel()
				}
			}()
		}
	}
	session := winapi.CurrentSessionID()
	// The session's agent mutex lives in the session's own namespace, so a
	// user in another session cannot squat it (see internal/ipc).
	name, _ := windows.UTF16PtrFromString(ipc.AgentMutexLocal())
	mu, err := windows.CreateMutex(nil, false, name)
	if err != nil { // includes ERROR_ALREADY_EXISTS
		if mu != 0 {
			windows.CloseHandle(mu)
		}
		return fmt.Errorf("an agent is already running in session %d", session)
	}
	defer windows.CloseHandle(mu)

	profile, _ := os.UserHomeDir()
	a := &Agent{
		log:     logx.Open(LogPath(), false),
		ev:      elog.Open(),
		hive:    onedrive.CurrentUserHive(),
		session: session,
		profile: profile,
		appData: os.Getenv("LOCALAPPDATA"),
		user:    os.Getenv("USERNAME"),
		wake:    make(chan struct{}, 1),
	}
	a.stopping = func() bool { return ctx.Err() != nil || (!standalone && !serviceRunning()) }
	defer a.ev.Close()
	popup.SetPanicHandler(func(r any, stack []byte) { a.logPanic("popup action", r, stack) })
	if a.ui, err = popup.Shared(); err != nil {
		a.log.Printf("popup UI unavailable: %v", err)
	}
	defer a.ui.RemoveTray()

	a.log.Printf("agent %s started in session %d (standalone=%v)", version.Version, session, standalone)
	a.ev.Info(elog.AgentStarted, fmt.Sprintf("OneDrive Helper agent started for %s in session %d.", a.user, session))
	a.recoverFixes()
	// A fix held up by open files may be waiting for this restart: finish it
	// now, as the user, before they start opening files.
	go a.safe("resume after restart", a.resumeAfterRestart)()

	serviceDown := 0
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			a.log.Printf("service stopped; agent exiting")
			a.waitForFix()
			return nil
		case <-timer.C:
		case <-a.wake:
		}
		cfg := config.Load()
		a.tick(cfg, time.Now())

		if !standalone {
			if serviceRunning() {
				serviceDown = 0
			} else if serviceDown++; serviceDown >= 2 {
				a.log.Printf("service is not running; agent exiting")
				a.waitForFix()
				return nil
			}
		}
		interval := cfg.CheckInterval()
		if a.restartPending() && interval > autoRestartSpacing {
			interval = autoRestartSpacing // check back soon after an automatic restart
		}
		timer.Reset(interval)
	}
}

const (
	// autoRestartSpacing is the minimum gap between automatic restart attempts
	// (time for OneDrive to start, or for the next attempt).
	autoRestartSpacing = 30 * time.Second
	// autoRestartWindow limits automatic restarts to cfg.AutoRestartAttempts in
	// this period, so a OneDrive that keeps crashing ends in a warning rather
	// than an endless restart loop.
	autoRestartWindow = 30 * time.Minute
)

// refresh runs the next check now, so the tray icon and alerts catch up
// straight away (e.g. green as soon as a fix has removed the folders). It
// never blocks; requests made while a check is pending are merged.
func (a *Agent) refresh() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// restartPending reports an outage in which automatic restarts are still in progress.
func (a *Agent) restartPending() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.outageLogged && !a.autoGaveUp && len(a.restarts) > 0
}

// recentRestarts drops attempts older than the window and returns the rest.
func recentRestarts(times []time.Time, now time.Time) []time.Time {
	var out []time.Time
	for _, t := range times {
		if now.Sub(t) < autoRestartWindow {
			out = append(out, t)
		}
	}
	return out
}

// tryAutoRestart decides whether to restart OneDrive now. It returns true
// while automatic restarts are still being tried (so no warning is shown yet).
// Must be called with a.mu held.
func (a *Agent) tryAutoRestart(cfg config.Config, now time.Time) bool {
	if !cfg.AutoRestartEnabled || a.autoGaveUp {
		return false
	}
	a.restarts = recentRestarts(a.restarts, now)
	if n := len(a.restarts); n > 0 && now.Sub(a.restarts[n-1]) < autoRestartSpacing {
		return true // the last attempt may still be starting up
	}
	if len(a.restarts) >= cfg.AutoRestartAttempts {
		a.autoGaveUp = true
		a.log.Printf("OneDrive could not be restarted automatically (%d attempts in %s); showing the warning", len(a.restarts), autoRestartWindow)
		a.ev.Warning(elog.OneDriveAutoRestartFailed, fmt.Sprintf("OneDrive could not be restarted automatically for %s after %d attempts. The user has been warned.",
			a.user, len(a.restarts)))
		return false
	}
	a.restarts = append(a.restarts, now)
	attempt := len(a.restarts)
	launch := a.launch
	if launch == nil {
		launch = func() error { return onedrive.LaunchOneDrive(a.hive) }
	}
	if err := launch(); err != nil {
		a.log.Printf("automatic restart %d/%d failed: %v", attempt, cfg.AutoRestartAttempts, err)
	} else {
		a.log.Printf("OneDrive was not running; automatic restart %d/%d", attempt, cfg.AutoRestartAttempts)
	}
	return true
}

func (a *Agent) warnEvent(id uint32, msg string) {
	a.ev.Warning(id, msg)
	if a.onEvent != nil {
		a.onEvent(id)
	}
}

// recoverFixes finishes or reports fixes interrupted by a crash, kill, power
// loss or logoff. It runs as the user only (never elevated).
func (a *Agent) recoverFixes() {
	t, err := remediate.CurrentUserTarget()
	if err != nil {
		a.log.Printf("recovery: %v", err)
		return
	}
	for _, i := range remediate.Recover(t, a.log, a.ev) {
		a.log.Printf("recovery: needs attention:\n%s", i.Text())
	}
}

// waitForFix blocks while a fix is running so files are never left half-moved.
func (a *Agent) waitForFix() {
	for {
		a.mu.Lock()
		f := a.fixing
		a.mu.Unlock()
		if !f {
			return
		}
		time.Sleep(time.Second)
	}
}

// serviceRunning queries the service with the minimal rights a standard user has.
func serviceRunning() bool {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return true // can't tell; keep running
	}
	defer windows.CloseServiceHandle(scm)
	name, _ := windows.UTF16PtrFromString(version.ServiceName)
	s, err := windows.OpenService(scm, name, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST)
	}
	defer windows.CloseServiceHandle(s)
	var st windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(s, &st); err != nil {
		return true
	}
	return st.CurrentState == windows.SERVICE_RUNNING || st.CurrentState == windows.SERVICE_START_PENDING
}

func (a *Agent) actions() Actions {
	return Actions{
		Relaunch:    a.safe("restart OneDrive", a.relaunch),
		Snooze:      func(alert, d string) { a.safe("ignore", func() { a.snooze(alert, d) })() },
		Support:     a.safe("contact support", a.support),
		Email:       a.safe("e-mail support", a.email),
		Website:     a.safe("support website", a.website),
		Fix:         a.safe("fix", a.fix),
		Explain:     func(topic string) { a.safe("explain", func() { a.explain(topic) })() },
		CopyDetails: a.safe("copy details", a.copyDetails),
	}
}

// safe wraps an action so a panic in it is logged with its stack trace and
// never stops the agent.
func (a *Agent) safe(name string, f func()) func() {
	return func() {
		defer func() {
			if r := recover(); r != nil {
				a.logPanic(name, r, debug.Stack())
			}
		}()
		f()
	}
}

func (a *Agent) logPanic(name string, r any, stack []byte) {
	if a.log != nil {
		a.log.Printf("panic in %s: %v\n%s", name, r, stack)
	}
}

func (a *Agent) tick(cfg config.Config, now time.Time) {
	defer func() {
		if r := recover(); r != nil {
			a.log.Printf("panic in check: %v", r)
		}
	}()
	s := a.collect(cfg)
	if libs := onedrive.ReadLibraries(a.appData, s.Accounts); len(libs) > 0 {
		if err := onedrive.SaveLibraries(a.hive, libs); err != nil {
			a.log.Printf("cache libraries: %v", err)
		}
	}
	if cfg.OneDriveCheckEnabled && s.Configured {
		a.checkOneDrive(cfg, now)
	} else {
		a.ui.Hide(idNotRunning)
	}
	if cfg.OrphanCheckEnabled {
		a.checkOrphans(cfg, now, s.Orphans)
		a.cleanNavPane(s.StaleNav, now)
	} else {
		a.ui.Hide(idOrphans)
	}
	a.mu.Lock()
	s.MissingSince = a.missingSince
	a.last = s
	a.mu.Unlock()
	a.updateTray(cfg, s)
}

// unknownLimit failed process listings in a row are logged once (3007).
const unknownLimit = 5

// oneDriveState reads OneDrive's process state (replaceable in tests).
func (a *Agent) oneDriveState() onedrive.ProcState {
	if a.procState != nil {
		return a.procState()
	}
	return onedrive.OneDriveState(a.session)
}

func (a *Agent) checkOneDrive(cfg config.Config, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.oneDriveState()
	if state == onedrive.StateUnknown {
		// Can't tell: change nothing (no restart, no alert, no clock change).
		if a.unknownStreak++; a.unknownStreak == unknownLimit {
			a.log.Printf("the process list could not be read %d times in a row", unknownLimit)
			a.warnEvent(elog.OneDriveStateUnknown, fmt.Sprintf("OneDrive Helper could not read the process list for %s %d checks in a row, so it cannot tell whether OneDrive is running.", a.user, unknownLimit))
		}
		return
	}
	afterUnknown := a.unknownStreak > 0
	a.unknownStreak = 0
	if state == onedrive.StateRunning {
		a.lastRunning = now
		if a.outageLogged {
			if n := len(recentRestarts(a.restarts, now)); n > 0 && !a.autoGaveUp {
				a.ev.Info(elog.OneDriveAutoRestarted, fmt.Sprintf("OneDrive had stopped for %s and was restarted automatically (attempt %d).", a.user, n))
				a.log.Printf("OneDrive restarted automatically (attempt %d)", n)
			} else {
				a.ev.Info(elog.OneDriveRecovered, "OneDrive is running again for "+a.user+".")
				a.log.Printf("OneDrive is running again")
			}
		}
		a.missingSince, a.outageLogged, a.autoGaveUp = time.Time{}, false, false
		a.ui.Hide(idNotRunning)
		return
	}
	if onedrive.UpdateInProgress() {
		a.missingSince = now // give OneDrive the full grace period after the update finishes
		return
	}
	if a.missingSince.IsZero() {
		if afterUnknown && !a.lastRunning.IsZero() {
			// It stopped at some point while we couldn't look: count from the
			// last time it was seen running, as the normal timeline would.
			a.missingSince = a.lastRunning
		} else {
			a.missingSince = now
			return
		}
	}
	if now.Sub(a.missingSince) < cfg.OneDriveGrace() {
		return
	}
	if !a.outageLogged {
		a.outageLogged = true
		a.log.Printf("OneDrive not running since %s", a.missingSince.Format(time.RFC3339))
		a.ev.Warning(elog.OneDriveNotRunning, fmt.Sprintf("OneDrive is not running for %s (session %d) since %s.",
			a.user, a.session, a.missingSince.Format(time.RFC3339)))
	}
	// "Ignore" means the user may have closed OneDrive on purpose: leave it alone.
	if Snoozed(a.hive, idNotRunning, now, winapi.BootTime()) {
		return
	}
	if a.tryAutoRestart(cfg, now) || a.ui.Visible(idNotRunning) {
		return
	}
	a.ui.Show(notRunningSpec(cfg, a.actions(), cfg.AutoRestartEnabled && a.autoGaveUp))
}

func (a *Agent) checkOrphans(cfg config.Config, now time.Time, orphans []onedrive.Orphan) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(orphans) == 0 {
		if a.orphanKey != "" {
			a.ev.Info(elog.OrphansResolved, "No orphaned OneDrive library folders remain for "+a.user+".")
		}
		a.orphanKey = ""
		a.ui.Hide(idOrphans)
		return
	}
	if a.fixing {
		return
	}
	paths := make([]string, len(orphans))
	for i, o := range orphans {
		paths[i] = o.Path
	}
	sort.Strings(paths)
	key := strings.Join(paths, "|")
	changed := key != a.orphanKey
	if changed {
		// A different set of folders is a new problem: it overrides "Ignore".
		// (The first check after start-up only adopts the current set, so a
		// restart doesn't undo the user's choice.)
		if a.orphanKey != "" {
			ClearSnooze(a.hive, idOrphans)
		}
		a.orphanKey = key
		a.log.Printf("orphaned library folders: %s", strings.Join(paths, "; "))
		a.ev.Warning(elog.OrphanedLibraries, fmt.Sprintf("OneDrive library folders exist on disk for %s but are not being synced:\r\n%s",
			a.user, strings.Join(paths, "\r\n")))
	}
	if Snoozed(a.hive, idOrphans, now, winapi.BootTime()) {
		a.ui.Hide(idOrphans)
		return
	}
	if a.ui.Visible(idOrphans) && !changed {
		return
	}
	a.ui.Show(orphanSpec(cfg, orphans, a.actions()))
}

// --- button actions (run on their own goroutines) ---

func (a *Agent) relaunch() {
	if a.oneDriveState() != onedrive.StateNotRunning {
		return // running, or we can't tell: never start a second copy
	}
	if err := onedrive.LaunchOneDrive(a.hive); err != nil {
		a.log.Printf("relaunch: %v", err)
		cfg := config.Load()
		dialog.Error("OneDrive could not be started", "OneDrive could not be started: "+err.Error()+"\n\n"+cfg.SupportContact())
		return
	}
	ClearSnooze(a.hive, idNotRunning)
	a.log.Printf("OneDrive restarted by user")
	a.ev.Info(elog.OneDriveRelaunched, "OneDrive was restarted by "+a.user+" from the OneDrive Helper alert.")
}

var alertNames = map[string]string{idNotRunning: "OneDrive not running", idOrphans: "folders not syncing"}

func (a *Agent) snooze(alert, d string) {
	if err := SetSnooze(a.hive, alert, d, time.Now()); err != nil {
		a.log.Printf("snooze %s: %v", alert, err)
		return
	}
	if alert == idOrphans {
		a.ui.Hide(idOrphans)
	}
	a.log.Printf("alert %q ignored (%s)", alert, d)
	a.ev.Info(elog.OneDriveAlertSnoozed, fmt.Sprintf("%s chose to ignore the '%s' alert (%s).", a.user, alertNames[alert], config.IgnoreLabel(d)))
}

// support shows the support contact details.
func (a *Agent) support() {
	cfg := config.Load()
	dialog.Info("Contact support", "To get help with OneDrive on this computer:\n\n"+cfg.SupportContact()+
		"\n\nTip: right-click the OneDrive Helper icon next to the clock and choose 'Copy details for support' to include the details.")
}

// email starts a new e-mail to support containing the full diagnostic report.
//
// Preferred: an unsent draft (.eml) opened in the user's mail program, with
// the report in a small monospace font. Fallback (no desktop mail program,
// or SupportEmailMethod=link): a mailto: link with a short summary, the full
// report on the clipboard and a prompt to paste it.
func (a *Agent) email() {
	cfg := config.Load()
	addr := cfg.SupportEmailAddress()
	if addr == "" {
		a.support()
		return
	}
	s := a.collect(cfg)
	report := Report(cfg, s, LogPath())
	method := strings.ToLower(cfg.SupportEmailMethod)
	if method == "draft" || (method != "link" && winapi.HasFileHandler(".eml")) {
		path, err := writeSupportEML(supportEML(cfg, addr, s, report))
		if err == nil {
			if err = winapi.ShellOpen(path); err == nil {
				a.log.Printf("opened support e-mail draft %s", path)
				return
			}
		}
		a.log.Printf("support e-mail draft: %v; falling back to a mailto: link", err)
	}
	copied := a.ui.CopyText(report) == nil
	if err := winapi.ShellOpen(supportMailto(cfg, addr, s, copied)); err != nil {
		a.log.Printf("open e-mail: %v", err)
		dialog.Info("No e-mail program found", "No e-mail program could be opened. Please e-mail "+addr+".\n\n"+cfg.SupportContact())
		return
	}
	if copied {
		dialog.Info("E-mail started", "A new e-mail to "+addr+" has been started.\n\n"+
			"The full details for support have been copied: click into the e-mail where it says so and press Ctrl+V to paste them.")
	}
}

// website opens the support website.
func (a *Agent) website() {
	cfg := config.Load()
	if u := cfg.SupportWebsite(); u == "" || winapi.ShellOpen(u) != nil {
		a.support()
	}
}

func (a *Agent) fix() {
	a.runFix("fix", func(busy func(bool), stopping func() bool) {
		exe, err := os.Executable()
		if err != nil {
			a.log.Printf("fix: %v", err)
			return
		}
		userFlow := a.userFlow
		if userFlow == nil {
			userFlow = remediate.UserFlow
		}
		userFlow(exe, a.log, busy, stopping)
	})
}

// resumeAfterRestart finishes a fix that was waiting for a restart. While
// OneDrive is still starting no plan can be made, so it tries again every
// 20 seconds for up to 15 minutes.
func (a *Agent) resumeAfterRestart() {
	a.runFix("resume after restart", func(busy func(bool), stopping func() bool) {
		for deadline := time.Now().Add(15 * time.Minute); remediate.ResumeAfterRestart(a.log, busy, stopping); {
			if stopping() || time.Now().After(deadline) {
				a.log.Printf("resume after restart: OneDrive did not finish starting; will try at the next start")
				return
			}
			time.Sleep(20 * time.Second)
		}
	})
}

// runFix runs f as the agent's one fix at a time: busy(true/false) shows the
// progress card, stopping reports that the agent is shutting down.
func (a *Agent) runFix(name string, f func(busy func(bool), stopping func() bool)) {
	a.mu.Lock()
	if a.fixing {
		a.mu.Unlock()
		return
	}
	a.fixing = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.fixing, a.orphanKey = false, "" // re-evaluate (and re-alert if needed) on the next check
		a.mu.Unlock()
		a.refresh()
	}()
	// A panic inside the fix has already unwound through Execute (releasing
	// its locks, deleting nothing more): put back or report what it left at
	// once, without waiting for the agent to restart.
	defer func() {
		if r := recover(); r != nil {
			a.logPanic(name, r, debug.Stack())
			a.ui.Hide(idFixing)
			if a.afterPanic != nil {
				a.afterPanic()
			} else {
				a.recoverFixes()
			}
		}
	}()
	cfg := config.Load()
	stopping := a.stopping
	if stopping == nil {
		stopping = func() bool { return false }
	}
	f(func(busy bool) {
		if busy {
			a.ui.Show(fixingSpec(cfg))
		} else {
			a.ui.Hide(idFixing)
			a.refresh() // the file work is done; don't wait for the result card to be closed
		}
	}, stopping)
}

// RunSample shows both alerts with logging-only actions until both are closed
// (or 10 minutes pass). Used by "OneDriveHelperAgent.exe test-popup".
func RunSample() error {
	ui, err := popup.Shared()
	if err != nil {
		return err
	}
	profile, _ := os.UserHomeDir()
	orphans := onedrive.FindOrphans(onedrive.Gather(onedrive.CurrentUserHive(), profile), onedrive.OSFS)
	wg := ShowPreview(ui, config.Load(), orphans)
	time.Sleep(time.Second)
	for deadline := time.Now().Add(10 * time.Minute); time.Now().Before(deadline) && ui.AnyVisible(); {
		time.Sleep(500 * time.Millisecond)
	}
	time.Sleep(time.Second)
	wg.Wait() // let any "you clicked" box be read
	return nil
}

// ShowPreview shows both alerts for cfg with buttons that only report what
// they would do. If orphans is empty, example library names are used.
func ShowPreview(ui *popup.Manager, cfg config.Config, orphans []onedrive.Orphan) *sync.WaitGroup {
	if len(orphans) == 0 {
		orphans = []onedrive.Orphan{{Name: "Finance - Documents"}, {Name: "Projects - Documents"}}
	}
	wg := &sync.WaitGroup{}
	note := func(what string) func() {
		return func() {
			wg.Add(1)
			defer wg.Done()
			dialog.Info("Preview", "You clicked '"+what+"'. No action is taken in preview mode.")
		}
	}
	acts := Actions{
		Relaunch: note("Restart OneDrive"),
		Snooze:   func(_, d string) { note(config.IgnoreLabel(d))() },
		Support:  note("Contact support: " + cfg.SupportContact()),
		Email:    note("Send an e-mail to " + cfg.SupportEmailAddress()),
		Website:  note("Visit the support website: " + cfg.SupportWebsite()),
		Fix:      note("Fix (admin)"),
		Explain: func(topic string) {
			wg.Add(1)
			defer wg.Done()
			dialog.Info(explainTitle, Explain(cfg, topic, orphans))
		},
		CopyDetails: note("Copy details"),
	}
	for _, s := range SampleSpecs(cfg, orphans, acts) {
		ui.Show(s)
	}
	return wg
}
