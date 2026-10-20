package remediate

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/elog"
	"github.com/denismcapple/OneDriveHelper/internal/ipc"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

// How a fix held up by open files is finished.
const (
	modeRestart = "restart" // single-user workstation: restart Windows
	modeSignOut = "signout" // shared or virtual desktops (RDS, AVD, Citrix, FSLogix, ...): sign out
)

// Pending is {Profile}\ODHelper\pending-fix.json: folders an approved fix
// could not move because files in them were open. The user's agent finishes
// them at the next sign-in, as the user, on whichever computer or session
// host that is: the file lives in the profile, so it travels with roaming
// and FSLogix profiles together with the folders themselves.
//
// The file is the user's to change. That is acceptable: it can only make the
// agent fix real broken folders inside the user's own profile, as the user,
// with every safety check, which they could delete by hand anyway.
type Pending struct {
	User       string    `json:"user"`
	Paths      []string  `json:"paths"`
	Mode       string    `json:"mode"`
	Requested  time.Time `json:"requested"`
	LogonID    uint64    `json:"logonId"` // the sign-in the request was made in
	Approved   time.Time `json:"approved"`
	ApprovedOn string    `json:"approvedOn"` // computer where the administrator approved it
	Attempts   int       `json:"attempts"`   // finishing runs started after a sign-in
}

// approvalInfo is when and where the administrator approved a fix.
type approvalInfo struct {
	At time.Time
	On string
}

const (
	// pendingValidity is how long a fix waits for the next sign-in.
	pendingValidity = 72 * time.Hour
	// maxRestartAttempts limits how often a fix is retried after signing in again.
	maxRestartAttempts = 2
)

func pendingPath(profile string) string {
	return filepath.Join(profile, "ODHelper", "pending-fix.json")
}

// restartEnv is the outside world of the restart steps (replaced in tests).
var restartEnv = struct {
	blockers func() []string
	restart  func() error
	logOff   func() error
	logonID  func() (uint64, error)
	bootTime func() time.Time
	now      func() time.Time
}{
	blockers: winapi.RestartBlockers,
	restart:  winapi.Restart,
	logOff:   winapi.LogOff,
	logonID:  winapi.LogonID,
	bootTime: winapi.BootTime,
	now:      time.Now,
}

func loadPending(profile string) (*Pending, error) {
	b, err := os.ReadFile(winapi.LongPath(pendingPath(profile)))
	if err != nil {
		return nil, err
	}
	p := &Pending{}
	if err := json.Unmarshal(b, p); err != nil {
		return nil, err
	}
	return p, nil
}

func savePending(profile string, p *Pending) error {
	if err := os.MkdirAll(winapi.LongPath(filepath.Dir(pendingPath(profile))), 0o755); err != nil {
		return err
	}
	b, err := jsonIndent(p)
	if err != nil {
		return err
	}
	return writeFileAtomic(pendingPath(profile), b)
}

func clearPending(profile string) { _ = os.Remove(winapi.LongPath(pendingPath(profile))) }

// RestartPending reports whether a fix is waiting for the next sign-in (for the status window).
func RestartPending(profile string) bool {
	_, err := loadPending(profile)
	return err == nil
}

// finishMode decides how the user may finish a fix held up by open files:
// a restart on a single-user workstation, a sign-out anywhere else (Remote
// Desktop and multi-session hosts, Citrix, VMware Horizon, FSLogix, or a PC
// with someone else signed in). "" = don't offer either; why says why.
func finishMode(cfg config.Config, locked []string) (mode, why string) {
	switch {
	case len(locked) == 0:
		return "", ""
	case !cfg.RestartToFinish:
		return "", "finishing a fix after a restart or sign-out is turned off"
	case len(restartEnv.blockers()) == 0:
		return modeRestart, ""
	}
	return modeSignOut, ""
}

// offerRestart asks the user whether to restart (or sign out) so the locked
// folders are finished at their next sign-in, and records the request if
// they agree. It reports whether it handled the result message.
func offerRestart(cfg config.Config, t Target, res *Result, lg *log.Logger, attempts int, appr approvalInfo) bool {
	locked := res.LockedPaths()
	mode, why := finishMode(cfg, locked)
	if mode == "" {
		if why != "" {
			lg.Printf("restart/sign-out not offered: %s", why)
		}
		return false
	}
	heading, buttons, how := "Restart to finish the fix?", []string{"Not now", "Restart later", "Restart now"},
		"A restart closes every program. If you restart, OneDrive Helper finishes the fix by itself when you sign in again, before you open anything."
	if mode == modeSignOut {
		heading, buttons, how = "Sign out to finish the fix?", []string{"Not now", "Sign out later", "Sign out now"},
			"Signing out closes your programs. When you sign in again, on this computer or any other one you use, OneDrive Helper finishes the fix by itself before you open anything."
	}
	text := resultMessage(res, cfg) + "\n\nThese folders could not be fixed because files in them are in use:\n• " + strings.Join(locked, "\n• ") +
		"\n\n" + how + " No administrator is needed again.\n\nSave your work and close your programs first."
	choice := flow.choose(heading, text, buttons, 2)
	if choice < 1 {
		lg.Printf("user declined to %s to finish the fix", mode)
		return true
	}
	p := &Pending{User: t.UserName, Paths: locked, Mode: mode, Requested: restartEnv.now(), Approved: appr.At, ApprovedOn: appr.On, Attempts: attempts}
	p.LogonID, _ = restartEnv.logonID()
	if err := savePending(t.ProfileDir, p); err != nil {
		lg.Printf("save the request to finish the fix: %v", err)
		flow.fail("The fix could not be scheduled", "The fix could not be scheduled for your next sign-in: "+err.Error()+"\n\n"+cfg.SupportContact())
		return true
	}
	ev := elog.Open()
	defer ev.Close()
	when := map[string]string{modeRestart: "after the next restart", modeSignOut: "when they sign out and in again"}[mode]
	ev.Info(elog.FixRestartRequested, fmt.Sprintf("Folders for %s were in use; the fix will finish %s:\r\n%s", t.UserName, when, strings.Join(locked, "\r\n")))
	env.onEvent(elog.FixRestartRequested)
	if choice == 1 {
		lg.Printf("fix will finish %s: %v", when, locked)
		next := "The next time this PC restarts and you sign in"
		if mode == modeSignOut {
			next = "The next time you sign out and sign in again"
		}
		flow.info("The fix will finish at your next sign-in", next+" (within 3 days), OneDrive Helper finishes fixing these folders:\n• "+strings.Join(locked, "\n• "))
		return true
	}
	lg.Printf("%s now to finish the fix: %v", mode, locked)
	act, failed := restartEnv.restart, "Windows could not be restarted (%v). Please restart it yourself: the fix will finish when you sign in again."
	if mode == modeSignOut {
		act, failed = restartEnv.logOff, "You could not be signed out (%v). Please sign out yourself: the fix will finish when you sign in again."
	}
	if err := act(); err != nil {
		lg.Printf("%s: %v", mode, err)
		flow.warn("That didn't work", fmt.Sprintf(failed, err))
	}
	return true
}

// signedInAgain reports whether this is a new sign-in since p was requested
// (after a sign-out, a restart, or on another session host).
func signedInAgain(p *Pending) bool {
	if id, err := restartEnv.logonID(); err == nil && p.LogonID != 0 {
		return id != p.LogonID
	}
	return !restartEnv.bootTime().Before(p.Requested) // can't tell the sign-in apart: fall back to a restart
}

// ResumeAfterRestart finishes a fix that was waiting for the user to restart
// or sign out. The user's agent calls it at start-up (after Recover), on any
// computer the user signs in to. It does nothing in the same sign-in the
// request was made in. retry is true while OneDrive is still starting and
// no plan can be made yet (call again shortly).
func ResumeAfterRestart(lg *log.Logger, busy func(bool), stop func() bool) (retry bool) {
	t, err := flow.target()
	if err != nil {
		return false
	}
	p, err := loadPending(t.ProfileDir)
	if err != nil {
		if !os.IsNotExist(err) {
			lg.Printf("pending fix unreadable (%v); removed", err)
			clearPending(t.ProfileDir)
		}
		return false
	}
	if !signedInAgain(p) {
		return false
	}
	cfg := config.Load()
	switch {
	case p.Approved.IsZero() || restartEnv.now().Sub(p.Requested) > pendingValidity:
		lg.Printf("pending fix from %s expired; removed", p.Requested.Format(time.RFC3339))
		clearPending(t.ProfileDir)
		return false
	case p.Attempts >= maxRestartAttempts:
		clearPending(t.ProfileDir)
		flow.warn("The fix could not be finished", "These folders were still in use after signing in again, so they were not fixed:\n• "+
			strings.Join(p.Paths, "\n• ")+"\n\n"+cfg.SupportContact())
		return false
	}
	plan, err := flow.plan(t, cfg)
	var refused *RefusedError
	if errors.As(err, &refused) {
		return true // OneDrive is still starting: try again in a moment
	}
	if err != nil {
		lg.Printf("finish pending fix: plan: %v", err)
		clearPending(t.ProfileDir)
		return false
	}
	// Only folders that are still broken now, on this computer, and were in the request.
	var items []Item
	plan.LocalBytes = 0
	for _, it := range plan.Items {
		c := onedrive.Clean(it.Orphan.Path)
		for _, want := range p.Paths {
			if onedrive.Clean(want) == c {
				items = append(items, it)
				plan.LocalBytes += it.Stats.LocalBytes
			}
		}
	}
	plan.Items = items
	plan.Required = uint64(plan.LocalBytes) + uint64(cfg.FreeSpaceMarginMB)<<20
	switch {
	case len(plan.Items) == 0:
		lg.Printf("finish pending fix: nothing left to fix")
		clearPending(t.ProfileDir)
		return false
	case len(plan.Interrupted) > 0:
		clearPending(t.ProfileDir)
		flow.warn("The fix could not be finished", "The fix that was waiting for you to sign in again was not finished, because an earlier fix did not finish.\n\n"+cfg.SupportContact())
		return false
	case !plan.EnoughSpace():
		clearPending(t.ProfileDir)
		flow.warn("The fix could not be finished", "The fix that was waiting for you to sign in again was not finished: there is not enough free disk space to save the files first.\n\n"+cfg.SupportContact())
		return false
	case stop():
		return false
	}
	// Count the attempt before starting, so a run that keeps crashing stops.
	p.Attempts++
	if err := savePending(t.ProfileDir, p); err != nil {
		lg.Printf("finish pending fix: %v", err)
		return false
	}
	ev := elog.Open()
	defer ev.Close()
	ev.Info(elog.FixResumed, fmt.Sprintf("Finishing a OneDrive Helper fix for %s after signing in again (approved %s on %s):\r\n%s",
		t.UserName, p.Approved.Local().Format("2006-01-02 15:04"), p.ApprovedOn, plan.Paths()))
	env.onEvent(elog.FixResumed)
	lg.Printf("finishing the pending fix (approved %s on %s): %s", p.Approved.Format(time.RFC3339), p.ApprovedOn, plan.Paths())
	if release, err := ipc.HoldFixLock(); err != nil {
		lg.Printf("fix lock: %v (continuing without it)", err)
	} else {
		defer release()
	}
	busy(true)
	res, err := flow.execute(plan, lg, ev, stop)
	busy(false)
	clearPending(t.ProfileDir)
	if err != nil {
		lg.Printf("execute: %v", err)
		flow.fail("The fix could not be finished", "The fix could not be completed:\n\n"+err.Error()+"\n\n"+cfg.SupportContact())
		return false
	}
	afterExecute(t, cfg, plan, res, lg, p.Attempts, approvalInfo{At: p.Approved, On: p.ApprovedOn})
	return false
}
