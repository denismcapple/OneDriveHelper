package remediate

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/dialog"
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
		dialog.Error("Administrator approval needed", "Administrator approval is required for this action.")
		return ExitError
	}
	t, err := TargetForSID(sid)
	if err != nil {
		lg.Printf("target: %v", err)
		dialog.Error("The fix can't be approved", err.Error())
		return ExitError
	}
	plan, err := BuildPlan(t, cfg)
	planEvents(ev, t, plan, err)
	if err != nil {
		lg.Printf("plan: %v", err)
		dialog.Error("The fix can't be approved", err.Error())
		return ExitError
	}
	if len(plan.Items) == 0 {
		msg := "No orphaned library folders were found that can be fixed automatically."
		if len(plan.Excluded) > 0 {
			msg += "\n\nThese need to be fixed manually:\n" + strings.Join(plan.Excluded, "\n")
		}
		dialog.Info("Nothing to fix", msg)
		return ExitNothingToDo
	}
	if !plan.EnoughSpace() {
		dialog.Error("Not enough disk space", "There is not enough free disk space to safely save the files.\n\n"+plan.Summary())
		ev.Error(elog.RemediationFailed, "Remediation refused (insufficient disk space) for "+t.UserName+"\r\n"+plan.Summary())
		return ExitNoSpace
	}
	admin := winapi.CurrentUserName()
	code, event := decideApproval(deadline, time.Now, func(limit time.Duration) (bool, bool) {
		return dialog.ConfirmWithin("Administrator approval: fix these folders?", plan.Summary()+
			"\nFiles stored on this PC are saved first; then the broken folders are removed and OneDrive is asked to sync the libraries again.", "Approve", limit)
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

// planEvents logs refusals found while planning: 5013 when OneDrive's data
// can't be trusted, 5009 for folders behind a junction or link.
func planEvents(ev *elog.Log, t Target, p *Plan, err error) {
	var refused *RefusedError
	if errors.As(err, &refused) {
		ev.Warning(elog.FixRefusedData, fmt.Sprintf("No OneDrive Helper fix was planned for %s: %s.", t.UserName, refused.Reason))
		env.onEvent(elog.FixRefusedData)
	}
	if p != nil && len(p.LinkRefused) > 0 {
		ev.Warning(elog.FixRefusedLink, fmt.Sprintf("OneDrive Helper will not fix these folders for %s because a junction or symbolic link is in the path:\r\n%s",
			t.UserName, strings.Join(p.LinkRefused, "\r\n")))
		env.onEvent(elog.FixRefusedLink)
	}
}

// flowEnv is the outside world of UserFlow (replaced in tests).
type flowEnv struct {
	target  func() (Target, error)
	recover func(t Target, lg *log.Logger)
	plan    func(t Target, cfg config.Config) (*Plan, error)
	elevate func(exe string, args []string, deadline time.Time) (uint32, error, <-chan struct{})
	execute func(p *Plan, lg *log.Logger, ev *elog.Log, stop func() bool) (*Result, error)
	resync  func(orphans []onedrive.Orphan, lg *log.Logger)
	info    func(heading, text string)
	success func(heading, text string)
	warn    func(heading, text string)
	fail    func(heading, text string)
	confirm func(heading, text string) bool
	// choose shows buttons (primary = accent button) and returns the index clicked.
	choose func(heading, text string, buttons []string, primary int) int
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
		info:    dialog.Info,
		success: dialog.Success,
		warn:    dialog.Warn,
		fail:    dialog.Error,
		confirm: func(heading, text string) bool { return dialog.Confirm(heading, text, "Continue") },
		choose: func(heading, text string, buttons []string, primary int) int {
			return dialog.Show(dialog.Message{Heading: heading, Text: text, Kind: dialog.KindWarning, Buttons: buttons, Primary: primary})
		},
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
		flow.fail("The fix can't start", err.Error())
		return
	}
	// Finish or report any earlier fix that was interrupted before planning a new one.
	flow.recover(t, lg)
	plan, err := flow.plan(t, cfg)
	func() {
		ev := elog.Open()
		defer ev.Close()
		planEvents(ev, t, plan, err)
	}()
	var refused *RefusedError
	if errors.As(err, &refused) {
		flow.warn("The fix can't start right now", err.Error()+"\n\n"+cfg.SupportContact())
		return
	}
	if err != nil {
		flow.fail("The fix can't start", err.Error()+"\n\n"+cfg.SupportContact())
		return
	}
	if len(plan.Interrupted) > 0 {
		var b strings.Builder
		for _, i := range plan.Interrupted {
			b.WriteString(i.Text())
		}
		flow.warn("An earlier fix did not finish", "These folders are not where they were:\n\n"+b.String()+
			"\nPlease contact support before fixing anything else. "+cfg.SupportContact())
		return
	}
	if len(plan.Items) == 0 {
		if len(plan.Excluded) > 0 {
			flow.warn("Your support team needs to fix these", strings.Join(plan.Excluded, "\n")+"\n\n"+cfg.SupportContact())
		} else {
			flow.success("Nothing needs fixing", "Good news: no broken library folders were found. Everything looks fine.")
		}
		return
	}
	if !cfg.RemediationEnabled {
		flow.warn("Automatic fixing is turned off", "Automatic fixing is disabled on this PC. "+cfg.SupportContact())
		return
	}
	if !plan.EnoughSpace() {
		flow.fail("Not enough disk space", "There is not enough free disk space to safely save your files first.\n\n"+plan.Summary()+"\n"+cfg.SupportContact())
		return
	}
	var list strings.Builder
	resync, manual := false, false
	for _, it := range plan.Items {
		list.WriteString("• " + it.Orphan.DisplayName() + "\n    " + ResyncNote(it.Orphan) + "\n")
		resync = resync || it.Orphan.NeedsResync()
		manual = manual || (it.Orphan.NeedsResync() && !it.Orphan.CanResync())
	}
	step3 := ""
	switch {
	case manual:
		step3 = "  3. Ask OneDrive to sync the libraries again. Where that can't be done automatically\n" +
			"      (see above), you will need to click 'Sync' on the library's web page.\n"
	case resync:
		step3 = "  3. Ask OneDrive to sync the libraries again\n"
	}
	if !flow.confirm("Fix these folders?", "These folders are no longer syncing with OneDrive:\n\n"+list.String()+
		"\nThe fix will:\n  1. Save a copy of the files stored on this PC to "+plan.BackupDir+"\n"+
		"  2. Remove the broken folders\n"+step3+"\n"+
		"An administrator must approve this. Your support team may need to enter their credentials.\n\n"+
		"If you are unsure, stop here. "+cfg.SupportContact()) {
		return
	}
	deadline := time.Now().Add(approvalDeadline)
	code, err, returned := flow.elevate(agentExe, []string{"remediate-approve", "--sid", t.SID,
		"--deadline", strconv.FormatInt(deadline.Unix(), 10)}, deadline)
	switch {
	case errors.Is(err, winapi.ErrTimedOut):
		lg.Printf("approval not answered within %s; nothing changed", approvalDeadline)
		flow.info("Nothing was changed", "Nobody approved the fix within 15 minutes, so nothing was changed.\n\n"+cfg.SupportContact())
		<-returned // keep "fixing" set until the stale UAC prompt is gone, so no second prompt appears
		return
	case errors.Is(err, winapi.ErrCancelled):
		flow.info("Cancelled", "Cancelled. No changes were made.\n\n"+cfg.SupportContact())
		return
	case err != nil:
		lg.Printf("elevate: %v", err)
		flow.fail("Administrator approval failed", "Could not request administrator approval: "+err.Error())
		return
	case code != ExitApproved:
		lg.Printf("approval step exited with %d", code)
		return
	}
	host, _ := os.Hostname()
	appr := approvalInfo{At: time.Now(), On: host}

	// Approved: re-check that nothing changed since the plan was shown.
	again, err := flow.plan(t, cfg)
	if err != nil || again.Paths() != plan.Paths() || !again.EnoughSpace() {
		flow.warn("Nothing was changed", "The folders changed while waiting for approval, so nothing was done. Please try again.")
		return
	}
	if stop() {
		flow.info("The fix was not started", notStarted)
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
		flow.info("The fix was not started", notStarted)
		return
	}
	busy(true)
	res, err := flow.execute(again, lg, ev, stop)
	busy(false)
	if err != nil {
		lg.Printf("execute: %v", err)
		flow.fail("The fix could not be completed", "The fix could not be completed:\n\n"+err.Error()+"\n\n"+cfg.SupportContact())
		return
	}
	afterExecute(t, cfg, again, res, lg, 0, appr)
}

// afterExecute tells the user the result (offering a restart or sign-out if
// folders were held up by open files) and re-syncs the folders that were
// fully fixed. attempts counts earlier sign-ins that tried to finish this fix.
func afterExecute(t Target, cfg config.Config, plan *Plan, res *Result, lg *log.Logger, attempts int, appr approvalInfo) {
	lg.Printf("result:\n%s", res.Text())
	handled := attempts < maxRestartAttempts && offerRestart(cfg, t, res, lg, attempts, appr)
	switch {
	case handled:
	case res.OK():
		flow.success("Folders fixed", resultMessage(res, cfg))
	case !res.Changed():
		flow.warn("Nothing was changed", resultMessage(res, cfg))
	default:
		flow.warn("The fix was only partly completed", resultMessage(res, cfg))
	}
	// Re-sync only folders that were fully fixed: rebuilding a library over a
	// folder that was put back or kept in staging would mix them up.
	done := res.Done()
	var orphans []onedrive.Orphan
	for _, it := range plan.Items {
		if done[onedrive.Clean(it.Orphan.Path)] {
			orphans = append(orphans, it.Orphan)
		}
	}
	flow.resync(orphans, lg)
}

// resultMessage is what the user is told after a fix. It only mentions the
// backup folder when something was actually saved there.
func resultMessage(res *Result, cfg config.Config) string {
	var s string
	switch {
	case res.OK():
		return res.Text() + "\nYour saved files are in:\n" + res.RunDir
	case !res.Changed():
		s = res.Text() + "\nYour folders are exactly where they were. " + cfg.SupportContact()
	default:
		s = res.Text() + "\nA copy of the files that were saved is in:\n" + res.RunDir + "\n\n" + cfg.SupportContact()
	}
	if res.Report != "" {
		s += "\n\nFor your support team, the details are in:\n" + res.Report
	}
	return s
}

const notStarted = "OneDrive Helper is being updated or stopped, so the fix was not started. No changes were made. Please try again in a few minutes."

// ResyncNote explains, in plain words, how a folder will be reconnected.
func ResyncNote(o onedrive.Orphan) string {
	switch {
	case len(o.KnownFolders) > 0:
		return "Contains your " + strings.Join(o.KnownFolders, "/") + " folder, so it can only be fixed by your support team."
	case o.EmptyOrg:
		return "Empty leftover folder - it will simply be removed."
	case o.Kind == onedrive.KindMoved:
		return "Old copy: this library now syncs to " + o.MovedTo + ". Your files here are saved, then this folder is removed."
	case o.Kind == onedrive.KindDisconnected:
		return "From an account that is no longer connected. Your files are saved, then the folder is removed (it isn't synced again)."
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
	var auto, manual, none []string
	for _, o := range orphans {
		if !o.NeedsResync() || exists(o.Path) {
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
	if len(auto)+len(manual)+len(none) == 0 {
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
	dialog.Info("Syncing the libraries again", b.String())
}
