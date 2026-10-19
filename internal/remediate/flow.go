package remediate

import (
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/elog"
	"github.com/denismcapple/OneDriveHelper/internal/ipc"
	"github.com/denismcapple/OneDriveHelper/internal/logx"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

// Exit codes of the elevated approval process.
const (
	ExitApproved    = 0
	ExitError       = 1
	ExitCancelled   = 3
	ExitNothingToDo = 4
	ExitNoSpace     = 5
)

// Approval time limits (KTD12). The agent gives up waiting at approvalDeadline;
// the elevated approval dialog closes itself after approvalDialogLimit, or at
// the deadline if that comes first. Vars so tests can shorten them.
var (
	approvalDeadline    = 15 * time.Minute
	approvalDialogLimit = 10 * time.Minute
)

// ParseDeadline reads the --deadline argument (Unix seconds). A missing or
// malformed value falls back to approvalDeadline from start.
func ParseDeadline(arg string, start time.Time) time.Time {
	if n, err := strconv.ParseInt(strings.TrimSpace(arg), 10, 64); err == nil && n > 0 {
		return time.Unix(n, 0)
	}
	return start.Add(approvalDeadline)
}

// decideApproval shows the approval dialog (via confirm, which gets the
// dialog's time limit) and returns the exit code and the event to log.
// After either limit nothing is approved and 5001 is never logged.
func decideApproval(deadline time.Time, now func() time.Time, confirm func(limit time.Duration) (ok, timedOut bool)) (int, uint32) {
	limit := approvalDialogLimit
	if rem := deadline.Sub(now()); rem < limit {
		limit = rem
	}
	if limit <= 0 {
		return ExitCancelled, elog.ApprovalTimedOut
	}
	ok, timedOut := confirm(limit)
	switch {
	case timedOut || !now().Before(deadline):
		return ExitCancelled, elog.ApprovalTimedOut
	case !ok:
		return ExitCancelled, elog.RemediationCancelled
	}
	return ExitApproved, elog.RemediationStarted
}

// RunApproval is "OneDriveHelperAgent.exe remediate-approve --sid <SID>
// --deadline <unix>". It runs elevated (UAC), independently re-detects the
// user's orphaned folders, shows the plan and records the administrator's
// decision. It changes nothing.
func RunApproval(sid string, deadline time.Time) int {
	if winapi.IsElevated() {
		_ = logx.PrepareProgramDataDir()
	}
	lg := logx.Open(filepath.Join(logx.ProgramDataDir(), "remediation.log"), false)
	ev := elog.Open()
	defer ev.Close()
	cfg := config.Load()
	timedOut := func() int {
		lg.Printf("approval for %s timed out; nothing changed", sid)
		ev.Info(elog.ApprovalTimedOut, "Nobody answered the OneDrive Helper fix approval in time, so nothing was changed.")
		return ExitCancelled
	}
	if !time.Now().Before(deadline) {
		return timedOut() // approved too late: the user has already been told nothing changed
	}
	if !winapi.IsElevated() {
		winapi.Error(cfg.Title(), "Administrator approval is required for this action.")
		return ExitError
	}
	t, err := TargetForSID(sid)
	if err != nil {
		lg.Printf("target: %v", err)
		winapi.Error(cfg.Title(), err.Error())
		return ExitError
	}
	plan, err := BuildPlan(t, cfg)
	if err != nil {
		lg.Printf("plan: %v", err)
		winapi.Error(cfg.Title(), err.Error())
		return ExitError
	}
	if len(plan.Items) == 0 {
		msg := "No orphaned library folders were found that can be fixed automatically."
		if len(plan.Excluded) > 0 {
			msg += "\n\nThese need to be fixed manually:\n" + strings.Join(plan.Excluded, "\n")
		}
		winapi.Info(cfg.Title(), msg)
		return ExitNothingToDo
	}
	if !plan.EnoughSpace() {
		winapi.Error(cfg.Title(), "There is not enough free disk space to safely save the files.\n\n"+plan.Summary())
		ev.Error(elog.RemediationFailed, "Remediation refused (insufficient disk space) for "+t.UserName+"\r\n"+plan.Summary())
		return ExitNoSpace
	}
	admin := winapi.CurrentUserName()
	code, event := decideApproval(deadline, time.Now, func(limit time.Duration) (bool, bool) {
		return winapi.ConfirmWithin(cfg.Title()+" - administrator approval",
			"Approve fixing these library folders?\n\n"+plan.Summary()+
				"\nFiles stored on this PC are saved first; then the broken folders are removed and OneDrive is asked to sync the libraries again.", limit)
	})
	switch event {
	case elog.ApprovalTimedOut:
		return timedOut()
	case elog.RemediationCancelled:
		lg.Printf("%s declined remediation for %s", admin, t.UserName)
		ev.Info(elog.RemediationCancelled, fmt.Sprintf("%s declined remediation for %s.", admin, t.UserName))
	default:
		lg.Printf("%s approved remediation for %s:\n%s", admin, t.UserName, plan.Summary())
		ev.Info(elog.RemediationStarted, fmt.Sprintf("%s approved remediation for %s:\r\n%s", admin, t.UserName, plan.Summary()))
	}
	return code
}

// flowEnv is the outside world of UserFlow (replaced in tests).
type flowEnv struct {
	target  func() (Target, error)
	recover func(t Target, lg *log.Logger)
	plan    func(t Target, cfg config.Config) (*Plan, error)
	elevate func(exe string, args []string, deadline time.Time) (uint32, error, <-chan struct{})
	execute func(p *Plan, lg *log.Logger, ev *elog.Log, stop func() bool) (*Result, error)
	resync  func(orphans []onedrive.Orphan, lg *log.Logger)
	info    func(title, text string)
	warn    func(title, text string)
	fail    func(title, text string)
	confirm func(title, text string) bool
}

func defaultFlowEnv() flowEnv {
	return flowEnv{
		target: CurrentUserTarget,
		recover: func(t Target, lg *log.Logger) {
			ev := elog.Open()
			defer ev.Close()
			Recover(t, lg, ev)
		},
		plan:    BuildPlan,
		elevate: winapi.RunElevatedWithDeadline,
		execute: Execute,
		resync:  Resync,
		info:    winapi.Info,
		warn:    winapi.Warn,
		fail:    winapi.Error,
		confirm: winapi.Confirm,
	}
}

var flow = defaultFlowEnv()

// UserFlow runs in the user's agent when they click "Fix". busy(true/false)
// brackets the file work so the caller can show a progress popup. stop
// reports that OneDrive Helper is stopping (e.g. for an upgrade): no fix
// starts after that, and no further folder is moved.
func UserFlow(agentExe string, lg *log.Logger, busy func(bool), stop func() bool) {
	cfg := config.Load()
	t, err := flow.target()
	if err != nil {
		flow.fail(cfg.Title(), err.Error())
		return
	}
	// Finish or report any earlier fix that was interrupted before planning a new one.
	flow.recover(t, lg)
	plan, err := flow.plan(t, cfg)
	if err != nil {
		flow.fail(cfg.Title(), err.Error()+"\n\n"+cfg.SupportContact())
		return
	}
	if len(plan.Interrupted) > 0 {
		var b strings.Builder
		for _, i := range plan.Interrupted {
			b.WriteString(i.Text())
		}
		flow.warn(cfg.Title(), "An earlier fix did not finish, and these folders are not where they were:\n\n"+b.String()+
			"\nPlease contact support before fixing anything else. "+cfg.SupportContact())
		return
	}
	if len(plan.Items) == 0 {
		if len(plan.Excluded) > 0 {
			flow.warn(cfg.Title(), "These folders need to be fixed by your support team:\n\n"+strings.Join(plan.Excluded, "\n")+"\n\n"+cfg.SupportContact())
		} else {
			flow.info(cfg.Title(), "Good news: no broken library folders were found. Everything looks fine.")
		}
		return
	}
	if !cfg.RemediationEnabled {
		flow.warn(cfg.Title(), "Automatic fixing is disabled on this PC. "+cfg.SupportContact())
		return
	}
	if !plan.EnoughSpace() {
		flow.fail(cfg.Title(), "There is not enough free disk space to safely save your files first.\n\n"+plan.Summary()+"\n"+cfg.SupportContact())
		return
	}
	var list strings.Builder
	manual := false
	for _, it := range plan.Items {
		list.WriteString("• " + it.Orphan.DisplayName() + "\n    " + ResyncNote(it.Orphan) + "\n")
		manual = manual || (!it.Orphan.CanResync() && !it.Orphan.EmptyOrg && !it.Orphan.Personal)
	}
	step3 := "  3. Ask OneDrive to sync the libraries again\n"
	if manual {
		step3 = "  3. Ask OneDrive to sync the libraries again. Where that can't be done automatically\n" +
			"      (see above), you will need to click 'Sync' on the library's web page.\n"
	}
	if !flow.confirm(cfg.Title(), "These folders are no longer syncing with OneDrive:\n\n"+list.String()+
		"\nThe fix will:\n  1. Save a copy of the files stored on this PC to "+plan.BackupDir+"\n"+
		"  2. Remove the broken folders\n"+step3+"\n"+
		"An administrator must approve this. Your support team may need to enter their credentials.\n\n"+
		"If you are unsure, stop here. "+cfg.SupportContact()+"\n\nContinue?") {
		return
	}
	deadline := time.Now().Add(approvalDeadline)
	code, err, returned := flow.elevate(agentExe, []string{"remediate-approve", "--sid", t.SID,
		"--deadline", strconv.FormatInt(deadline.Unix(), 10)}, deadline)
	switch {
	case errors.Is(err, winapi.ErrTimedOut):
		lg.Printf("approval not answered within %s; nothing changed", approvalDeadline)
		flow.info(cfg.Title(), "Nobody approved the fix within 15 minutes, so nothing was changed.\n\n"+cfg.SupportContact())
		<-returned // keep "fixing" set until the stale UAC prompt is gone, so no second prompt appears
		return
	case errors.Is(err, winapi.ErrCancelled):
		flow.info(cfg.Title(), "Cancelled. No changes were made.\n\n"+cfg.SupportContact())
		return
	case err != nil:
		lg.Printf("elevate: %v", err)
		flow.fail(cfg.Title(), "Could not request administrator approval: "+err.Error())
		return
	case code != ExitApproved:
		lg.Printf("approval step exited with %d", code)
		return
	}

	// Approved: re-check that nothing changed since the plan was shown.
	again, err := flow.plan(t, cfg)
	if err != nil || again.Paths() != plan.Paths() || !again.EnoughSpace() {
		flow.warn(cfg.Title(), "The folders changed while waiting for approval, so nothing was done. Please try again.")
		return
	}
	if stop() {
		flow.info(cfg.Title(), notStarted)
		return
	}
	ev := elog.Open()
	defer ev.Close()
	// Tell installers a fix is running, so an upgrade waits for it. The lock
	// covers the file work only (not the dialogs or the UAC wait).
	if release, err := ipc.HoldFixLock(); err != nil {
		lg.Printf("fix lock: %v (continuing without it)", err)
	} else {
		defer release()
	}
	if stop() {
		flow.info(cfg.Title(), notStarted)
		return
	}
	busy(true)
	res, err := flow.execute(again, lg, ev, stop)
	busy(false)
	if err != nil {
		lg.Printf("execute: %v", err)
		flow.fail(cfg.Title(), "The fix could not be completed:\n\n"+err.Error()+"\n\n"+cfg.SupportContact())
		return
	}
	lg.Printf("result:\n%s", res.Text())
	if res.OK() {
		flow.info(cfg.Title(), "Folders fixed:\n\n"+res.Text()+"\nYour saved files are in:\n"+res.RunDir)
	} else {
		flow.warn(cfg.Title(), "The fix was only partly completed:\n\n"+res.Text()+"\nYour saved files are in:\n"+res.RunDir+"\n\n"+cfg.SupportContact())
	}
	// Re-sync only folders that were fully fixed: rebuilding a library over a
	// folder that was put back or kept in staging would mix them up.
	done := res.Done()
	var orphans []onedrive.Orphan
	for _, it := range again.Items {
		if done[onedrive.Clean(it.Orphan.Path)] {
			orphans = append(orphans, it.Orphan)
		}
	}
	flow.resync(orphans, lg)
}

const notStarted = "OneDrive Helper is being updated or stopped, so the fix was not started. No changes were made. Please try again in a few minutes."

// ResyncNote explains, in plain words, how a folder will be reconnected.
func ResyncNote(o onedrive.Orphan) string {
	switch {
	case len(o.KnownFolders) > 0:
		return "Contains your " + strings.Join(o.KnownFolders, "/") + " folder, so it can only be fixed by your support team."
	case o.EmptyOrg:
		return "Empty leftover folder - it will simply be removed."
	case o.Personal:
		return "Old copy of a personal OneDrive that is no longer being synced (for example after your organisation was renamed, or an account was removed). Anything saved here is not backed up."
	case o.CanResync():
		return "Will be synced again automatically."
	case o.WebURL != "":
		return "Can't be re-synced automatically - its web page will open so you can click 'Sync'."
	default:
		return "Can't be re-synced automatically - your support team will need to re-add it."
	}
}

// Resync asks OneDrive to sync again every orphan whose folder is gone.
func Resync(orphans []onedrive.Orphan, lg *log.Logger) {
	cfg := config.Load()
	ev := elog.Open()
	defer ev.Close()
	var auto, manual, none, personal []string
	for _, o := range orphans {
		if o.EmptyOrg || exists(o.Path) {
			continue
		}
		if o.Personal {
			personal = append(personal, o.Name)
			continue
		}
		switch {
		case o.CanResync():
			u := o.Library.ResyncURL()
			lg.Printf("resync %s via %s", o.Path, u)
			if err := winapi.ShellOpen(u); err != nil {
				lg.Printf("resync %s: %v", o.Path, err)
				none = append(none, o.DisplayName())
				continue
			}
			auto = append(auto, o.DisplayName())
			time.Sleep(3 * time.Second) // let OneDrive process one request at a time
		case o.WebURL != "":
			_ = winapi.ShellOpen(o.WebURL)
			manual = append(manual, o.DisplayName())
		default:
			none = append(none, o.DisplayName())
		}
	}
	if len(auto)+len(manual)+len(none)+len(personal) == 0 {
		return
	}
	ev.Info(elog.ResyncRequested, fmt.Sprintf("Re-sync requested. Automatic: %v; manual: %v; unknown: %v", auto, manual, none))
	var b strings.Builder
	if len(auto) > 0 {
		b.WriteString("OneDrive has been asked to sync these libraries again (confirm if OneDrive asks):\n• " + strings.Join(auto, "\n• ") + "\n\n")
	}
	if len(manual) > 0 {
		b.WriteString("The web page for these libraries has been opened. Click 'Sync' on each page:\n• " + strings.Join(manual, "\n• ") + "\n\n")
	}
	if len(none) > 0 {
		b.WriteString("These libraries must be re-added manually. " + cfg.SupportContact() + "\n• " + strings.Join(none, "\n• ") + "\n\n")
	}
	if len(personal) > 0 {
		b.WriteString("These old personal OneDrive folders were removed (your files were saved first). Files that were already in the cloud are still there:\n• " +
			strings.Join(personal, "\n• ") + "\n")
	}
	winapi.Info(cfg.Title(), b.String())
}
