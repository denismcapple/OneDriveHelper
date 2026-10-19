package remediate

import (
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/elog"
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

// RunApproval is "OneDriveHelperAgent.exe remediate-approve --sid <SID>". It
// runs elevated (UAC), independently re-detects the user's orphaned folders,
// shows the plan and records the administrator's decision. It changes nothing.
func RunApproval(sid string) int {
	if winapi.IsElevated() {
		_ = logx.PrepareProgramDataDir()
	}
	lg := logx.Open(filepath.Join(logx.ProgramDataDir(), "remediation.log"), false)
	ev := elog.Open()
	defer ev.Close()
	cfg := config.Load()
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
	if !winapi.Confirm(cfg.Title()+" - administrator approval",
		"Approve fixing these library folders?\n\n"+plan.Summary()+
			"\nFiles stored on this PC are saved first; then the broken folders are removed and OneDrive is asked to sync the libraries again.") {
		lg.Printf("%s declined remediation for %s", admin, t.UserName)
		ev.Info(elog.RemediationCancelled, fmt.Sprintf("%s declined remediation for %s.", admin, t.UserName))
		return ExitCancelled
	}
	lg.Printf("%s approved remediation for %s:\n%s", admin, t.UserName, plan.Summary())
	ev.Info(elog.RemediationStarted, fmt.Sprintf("%s approved remediation for %s:\r\n%s", admin, t.UserName, plan.Summary()))
	return ExitApproved
}

// UserFlow runs in the user's agent when they click "Fix". busy(true/false)
// brackets the file work so the caller can show a progress popup.
func UserFlow(agentExe string, lg *log.Logger, busy func(bool)) {
	cfg := config.Load()
	t, err := CurrentUserTarget()
	if err != nil {
		winapi.Error(cfg.Title(), err.Error())
		return
	}
	plan, err := BuildPlan(t, cfg)
	if err != nil {
		winapi.Error(cfg.Title(), err.Error()+"\n\n"+cfg.SupportContact())
		return
	}
	if len(plan.Items) == 0 {
		if len(plan.Excluded) > 0 {
			winapi.Warn(cfg.Title(), "These folders need to be fixed by your support team:\n\n"+strings.Join(plan.Excluded, "\n")+"\n\n"+cfg.SupportContact())
		} else {
			winapi.Info(cfg.Title(), "Good news: no broken library folders were found. Everything looks fine.")
		}
		return
	}
	if !cfg.RemediationEnabled {
		winapi.Warn(cfg.Title(), "Automatic fixing is disabled on this PC. "+cfg.SupportContact())
		return
	}
	if !plan.EnoughSpace() {
		winapi.Error(cfg.Title(), "There is not enough free disk space to safely save your files first.\n\n"+plan.Summary()+"\n"+cfg.SupportContact())
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
	if !winapi.Confirm(cfg.Title(), "These folders are no longer syncing with OneDrive:\n\n"+list.String()+
		"\nThe fix will:\n  1. Save a copy of the files stored on this PC to "+plan.BackupDir+"\n"+
		"  2. Remove the broken folders\n"+step3+"\n"+
		"An administrator must approve this. Your support team may need to enter their credentials.\n\n"+
		"If you are unsure, stop here. "+cfg.SupportContact()+"\n\nContinue?") {
		return
	}
	code, err := winapi.RunElevatedAndWait(agentExe, []string{"remediate-approve", "--sid", t.SID})
	switch {
	case errors.Is(err, winapi.ErrCancelled):
		winapi.Info(cfg.Title(), "Cancelled. No changes were made.\n\n"+cfg.SupportContact())
		return
	case err != nil:
		lg.Printf("elevate: %v", err)
		winapi.Error(cfg.Title(), "Could not request administrator approval: "+err.Error())
		return
	case code != ExitApproved:
		lg.Printf("approval step exited with %d", code)
		return
	}

	// Approved: re-check that nothing changed since the plan was shown.
	again, err := BuildPlan(t, cfg)
	if err != nil || again.Paths() != plan.Paths() || !again.EnoughSpace() {
		winapi.Warn(cfg.Title(), "The folders changed while waiting for approval, so nothing was done. Please try again.")
		return
	}
	ev := elog.Open()
	defer ev.Close()
	busy(true)
	res, err := Execute(again, lg, ev)
	busy(false)
	if err != nil {
		lg.Printf("execute: %v", err)
		winapi.Error(cfg.Title(), "The fix could not be completed:\n\n"+err.Error()+"\n\n"+cfg.SupportContact())
		return
	}
	lg.Printf("result:\n%s", res.Text())
	if res.OK() {
		winapi.Info(cfg.Title(), "Folders fixed:\n\n"+res.Text()+"\nYour saved files are in:\n"+res.RunDir)
	} else {
		winapi.Warn(cfg.Title(), "The fix was only partly completed:\n\n"+res.Text()+"\nYour saved files are in:\n"+res.RunDir+"\n\n"+cfg.SupportContact())
	}
	orphans := make([]onedrive.Orphan, len(again.Items))
	for i, it := range again.Items {
		orphans[i] = it.Orphan
	}
	Resync(orphans, lg)
}

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
