// Package migrate moves a user from Folder Redirection to OneDrive backup
// (Known Folder Move) once no redirection policy applies to them any more:
// it points every redirected folder back to its default place in the
// profile, leaves a note on the Desktop saying where the old files are,
// restarts Explorer and OneDrive, and OneDrive's KFMSilentOptIn policy (set
// by the service) then backs Desktop, Documents and Pictures up. Files are
// not copied: they stay on the share. It runs as the user; with
// RequireAdminApproval an administrator approves it first (UAC).
package migrate

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/dialog"
	"github.com/denismcapple/OneDriveHelper/internal/elog"
	"github.com/denismcapple/OneDriveHelper/internal/folders"
	"github.com/denismcapple/OneDriveHelper/internal/kfm"
	"github.com/denismcapple/OneDriveHelper/internal/logx"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
	"github.com/denismcapple/OneDriveHelper/internal/popup"
	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

// Offer is a migration that can be offered to the user.
type Offer struct {
	Folders []folders.Folder // pointed back by the move
	Manual  []folders.Folder // redirected too, but the tool doesn't know where they belong
}

// Key identifies the offer (a different set of folders is a new offer).
func (o *Offer) Key() string {
	var parts []string
	for _, f := range append(append([]folders.Folder{}, o.Folders...), o.Manual...) {
		parts = append(parts, f.Value+"="+f.Current)
	}
	return strings.Join(parts, "|")
}

// Names lists the folders users know ("Desktop, Documents and Pictures"),
// without the registry aliases.
func (o *Offer) Names() string {
	var names []string
	seen := map[string]bool{}
	for _, f := range o.Folders {
		if !seen[f.Name] {
			seen[f.Name] = true
			names = append(names, f.Name)
		}
	}
	return joinNames(names)
}

func joinNames(n []string) string {
	switch len(n) {
	case 0:
		return ""
	case 1:
		return n[0]
	}
	return strings.Join(n[:len(n)-1], ", ") + " and " + n[len(n)-1]
}

// env is the outside world of the migration (replaced in tests).
type env struct {
	shared     func() []string
	redirected func(h folders.Hive, profile string) ([]folders.Folder, error)
	gpos       func(sid string) ([]string, error)
	policy     func() string // tenant of OneDrive's KFMSilentOptIn ("" = not set)
	accounts   func(h onedrive.Hive) []onedrive.Account
}

var sys = env{shared: winapi.SharedHostReasons, redirected: folders.Redirected, gpos: folders.PolicyGPOs, policy: kfm.Tenant, accounts: onedrive.Accounts}

// Check decides whether the move can be offered to a user. It returns the
// offer, or nil and why not (for logs and "check").
func Check(cfg config.Config, sid string, fh folders.Hive, oh onedrive.Hive, profile string) (*Offer, string) {
	if !cfg.FolderRedirectionMigration {
		return nil, "the folder redirection migration is turned off"
	}
	if why := sys.shared(); len(why) > 0 {
		return nil, "not offered on shared or virtual desktop hosts: " + strings.Join(why, ", ")
	}
	all, err := sys.redirected(fh, profile)
	if err != nil {
		return nil, "the folder locations could not be read: " + err.Error()
	}
	o := &Offer{}
	for _, f := range all {
		if f.Known() {
			o.Folders = append(o.Folders, f)
		} else {
			o.Manual = append(o.Manual, f)
		}
	}
	if len(o.Folders) == 0 {
		return nil, "no folders point outside the profile"
	}
	gpos, err := sys.gpos(sid)
	switch {
	case err != nil:
		return nil, "can't tell whether a Folder Redirection policy applies: " + err.Error()
	case len(gpos) > 0:
		return nil, "a Folder Redirection policy still applies (" + strings.Join(gpos, ", ") + ")"
	}
	// A signed-in work account has its tenant: a Business1 entry with only a
	// folder is left over from a sign-in that never finished.
	business := false
	for _, a := range sys.accounts(oh) {
		business = business || (a.Business() && a.Configured() && kfm.ValidTenant(a.TenantID))
	}
	if !business {
		return nil, "no work account is signed in to OneDrive yet (OneDrive can only back folders up for a signed-in work account)"
	}
	if sys.policy() == "" {
		return nil, "OneDrive's KFMSilentOptIn policy is not set yet (the service sets it)"
	}
	return o, ""
}

// --- the move ---

// NoteName is the note left on the Desktop.
const NoteName = "Where are my files.txt"

const defaultNote = "Your folders have moved to OneDrive\r\n" +
	"===================================\r\n\r\n" +
	"Your Desktop, Documents and Pictures now live on this computer and are backed up to OneDrive, " +
	"instead of being kept on a network drive.\r\n\r\n" +
	"Nothing has been deleted. The files you had before are still in the network folders:\r\n\r\n" +
	"{folders}\r\n" +
	"Your support team will help move them across. You can also open those folders and copy what you need " +
	"into the matching folder on this computer.\r\n\r\n{support}\r\n"

// Note is the text of the Desktop note for these folders.
func Note(cfg config.Config, o *Offer) string {
	var list strings.Builder
	seen := map[string]bool{}
	for _, f := range append(append([]folders.Folder{}, o.Folders...), o.Manual...) {
		line := fmt.Sprintf("  %s:  %s\r\n", f.Name, f.Current)
		if !seen[line] {
			seen[line] = true
			list.WriteString(line)
		}
	}
	text := defaultNote
	if t := strings.TrimSpace(cfg.MigrationNoteText); t != "" {
		text = strings.ReplaceAll(strings.ReplaceAll(t, "\r\n", "\n"), "\n", "\r\n") + "\r\n"
	}
	text = strings.ReplaceAll(text, "{folders}", list.String())
	return strings.ReplaceAll(text, "{support}", cfg.SupportContact())
}

// flowEnv is what the user-facing flow does (replaced in tests).
type flowEnv struct {
	confirm  func(m dialog.Message) bool
	elevate  func(exe string, args []string, deadline time.Time) (uint32, error, <-chan struct{})
	repoint  func(f folders.Folder, profile string) error
	note     func(path, text string) error
	shortcut func(lnk, target, description string) error
	explorer func() error
	onedrive func() error
	desktop  func() (string, error) // where the Desktop is now
	result   func(m dialog.Message)
	event    func(id uint32, warn bool, msg string)
}

var flow = flowEnv{
	confirm: func(m dialog.Message) bool { return dialog.Show(m) == 1 },
	desktop: func() (string, error) { return windows.KnownFolderPath(windows.FOLDERID_Desktop, 0) },
	elevate: winapi.RunElevatedWithDeadline,
	repoint: folders.Repoint,
	note: func(path, text string) error {
		return os.WriteFile(path, append([]byte("\xef\xbb\xbf"), text...), 0o644) // UTF-8 with BOM: Notepad shows it right
	},
	shortcut: folders.CreateShortcut,
	result:   func(m dialog.Message) { dialog.Show(m) },
	event: func(id uint32, warn bool, msg string) {
		ev := elog.Open()
		defer ev.Close()
		if warn {
			ev.Warning(id, msg)
		} else {
			ev.Info(id, msg)
		}
	},
}

// SetSystem wires up what the flow needs from the agent: restarting Explorer
// and OneDrive in the user's session.
func SetSystem(explorer, restartOneDrive func() error) {
	flow.explorer, flow.onedrive = explorer, restartOneDrive
}

// approvalDeadline: the user is told nothing changed if nobody approves in time.
const approvalDeadline = 15 * time.Minute

// Run offers and, if accepted, does the move for the signed-in user.
func Run(cfg config.Config, o *Offer, user, sid, profile, agentExe string, lg *log.Logger) {
	items := []popup.Item{{Title: "These folders will be on this computer and backed up to OneDrive", Heading: true}}
	for _, f := range o.Folders {
		if f.ID == "" {
			continue // an alias of a folder already listed
		}
		items = append(items, popup.Item{Title: "•  " + f.Name, Path: "now: " + f.Current})
	}
	if len(o.Manual) > 0 {
		items = append(items, popup.Item{Title: "Left as they are (your support team will look at these)", Heading: true})
		for _, f := range o.Manual {
			items = append(items, popup.Item{Title: "•  " + f.Name, Path: f.Current})
		}
	}
	footer := "Your files are not moved: they stay on the network drive, and a note on your Desktop will say where.\n" +
		"Your File Explorer windows will close and the taskbar will reload."
	if cfg.RequireAdminApproval {
		footer += "\nAn administrator needs to approve this."
	}
	if !flow.confirm(dialog.Message{Heading: "Move your folders to OneDrive?", Text: "Your " + o.Names() + " are still kept on a network drive. " +
		"They can now live on this computer and be backed up to OneDrive.", Items: items, Footer: footer,
		Kind: dialog.KindInfo, Buttons: []string{"Not now", "Move to OneDrive"}, Primary: 1, Wide: true}) {
		return
	}
	how := "approved by an administrator"
	if cfg.RequireAdminApproval {
		if !approve(cfg, sid, agentExe, lg) {
			return
		}
	} else {
		how = "confirmed by the user (administrator approval is turned off)"
	}
	// Re-check: nothing may have changed while waiting for approval.
	again, why := Check(cfg, sid, folders.CurrentUser, onedrive.CurrentUserHive(), profile)
	if again == nil || again.Key() != o.Key() {
		lg.Printf("migration: things changed while waiting (%s); nothing done", why)
		flow.result(dialog.Message{Heading: "Nothing was changed", Text: "Your folders changed while waiting, so nothing was done. Please try again.", Kind: dialog.KindWarning})
		return
	}
	Do(cfg, o, user, profile, how, lg)
}

func approve(cfg config.Config, sid, agentExe string, lg *log.Logger) bool {
	deadline := time.Now().Add(approvalDeadline)
	code, err, returned := flow.elevate(agentExe, []string{"migrate-approve", "--sid", sid, "--deadline", strconv.FormatInt(deadline.Unix(), 10)}, deadline)
	switch {
	case errors.Is(err, winapi.ErrTimedOut):
		flow.result(dialog.Message{Heading: "Nothing was changed", Text: "Nobody approved the move within 15 minutes, so nothing was changed.\n\n" + cfg.SupportContact(), Kind: dialog.KindInfo})
		<-returned
		return false
	case errors.Is(err, winapi.ErrCancelled):
		return false
	case err != nil:
		lg.Printf("migration approval: %v", err)
		flow.result(dialog.Message{Heading: "Administrator approval failed", Text: "Could not request administrator approval: " + err.Error(), Kind: dialog.KindError})
		return false
	}
	return code == 0
}

// Do points the folders back, leaves the note, restarts Explorer and OneDrive
// and reports the result. how says who approved it (for the event).
func Do(cfg config.Config, o *Offer, user, profile, how string, lg *log.Logger) {
	var done, failed []string
	for _, f := range o.Folders {
		if err := flow.repoint(f, profile); err != nil {
			lg.Printf("migration: %s: %v", f.Name, err)
			failed = append(failed, fmt.Sprintf("%s (%v)", f.Name, err))
			continue
		}
		if f.ID != "" { // a second entry of a folder already listed
			done = append(done, fmt.Sprintf("%s: %s -> %s", f.Name, f.Current, f.Default(profile)))
		}
	}
	desktop := filepath.Join(profile, "Desktop")
	if flow.desktop != nil {
		if d, err := flow.desktop(); err == nil && d != "" {
			desktop = d // e.g. already in OneDrive, if Desktop wasn't redirected
		}
	}
	notePath := filepath.Join(desktop, NoteName)
	if err := flow.note(notePath, Note(cfg, o)); err != nil {
		lg.Printf("migration: note: %v", err)
		notePath = ""
	}
	shortcut := ""
	if file := folders.ShortcutFile(cfg.MigrationShortcutName); file != "" && strings.TrimSpace(cfg.MigrationShortcutTarget) != "" {
		target := folders.ExpandTarget(cfg.MigrationShortcutTarget)
		if err := flow.shortcut(filepath.Join(desktop, file), target, "Your files from before the move to OneDrive"); err != nil {
			lg.Printf("migration: shortcut to %s: %v", target, err)
		} else {
			shortcut = strings.TrimSuffix(file, ".lnk")
			lg.Printf("migration: Desktop shortcut %q -> %s", shortcut, target)
		}
	}
	if flow.explorer != nil {
		if err := flow.explorer(); err != nil {
			lg.Printf("migration: restart Explorer: %v", err)
		}
	}
	if flow.onedrive != nil {
		if err := flow.onedrive(); err != nil {
			lg.Printf("migration: restart OneDrive: %v", err)
		}
	}
	msg := fmt.Sprintf("Folders of %s moved from Folder Redirection to their profile for OneDrive backup (%s):\r\n%s", user, how, strings.Join(done, "\r\n"))
	if len(o.Manual) > 0 {
		var left []string
		for _, f := range o.Manual {
			left = append(left, f.Name+": "+f.Current)
		}
		msg += "\r\nLeft as they are (unknown folders, fix by hand):\r\n" + strings.Join(left, "\r\n")
	}
	lg.Printf("%s", strings.ReplaceAll(msg, "\r\n", "; "))
	if len(failed) > 0 {
		flow.event(elog.FolderMigrationFailed, true, msg+"\r\nCould not be changed:\r\n"+strings.Join(failed, "\r\n"))
		flow.result(dialog.Message{Heading: "Some folders could not be moved", Text: "These folders still point to the network drive:\n" +
			strings.Join(failed, "\n") + "\n\n" + cfg.SupportContact(), Kind: dialog.KindWarning, Copy: true})
		return
	}
	flow.event(elog.FoldersMigrated, false, msg)
	text := "Your " + o.Names() + " are now on this computer. OneDrive will start backing them up within a few minutes."
	switch {
	case shortcut != "":
		text += "\n\nYour old files are still on the network drive: open '" + shortcut + "' on your Desktop to get to them."
	case notePath != "":
		text += "\n\nYour old files are still on the network drive. The note '" + NoteName + "' on your Desktop says where."
	}
	flow.result(dialog.Message{Heading: "Your folders have moved to OneDrive", Text: text, Kind: dialog.KindSuccess})
}

// --- the elevated approval step ---

// RunApproval is "OneDriveHelperAgent.exe migrate-approve --sid <SID>
// --deadline <unix>". It runs elevated (UAC), checks the user's folders
// again itself, and records the administrator's decision. It changes nothing.
func RunApproval(sid string, deadline time.Time, profile string) int {
	lg := logx.Open(filepath.Join(logx.ProgramDataDir(), "remediation.log"), false)
	cfg := config.Load()
	if !time.Now().Before(deadline) || !winapi.IsElevated() {
		return 3
	}
	o, why := Check(cfg, sid, folders.UserHive(sid), onedrive.UserHive(sid), profile)
	if o == nil {
		dialog.Info("Nothing to move", "The folders can't be moved now: "+why+".")
		return 4
	}
	var lines []string
	for _, f := range o.Folders {
		if f.ID != "" {
			lines = append(lines, "•  "+f.Name+":  "+f.Current+"  ->  "+f.Default(profile))
		}
	}
	limit := time.Until(deadline)
	if limit > 10*time.Minute {
		limit = 10 * time.Minute
	}
	i, timedOut := dialog.ShowWithin(dialog.Message{Heading: "Administrator approval: move folders to OneDrive?",
		Text: "User's folders will be pointed back to their profile and backed up to OneDrive. Files stay on the network drive.\n\n" + strings.Join(lines, "\n"),
		Kind: dialog.KindWarning, Buttons: []string{"Cancel", "Approve"}, Primary: 1, Wide: true}, limit)
	admin := winapi.CurrentUserName()
	if timedOut || i != 1 || !time.Now().Before(deadline) {
		lg.Printf("%s did not approve moving folders for %s", admin, sid)
		return 3
	}
	lg.Printf("%s approved moving folders for %s: %s", admin, sid, strings.Join(lines, "; "))
	flow.event(elog.RemediationStarted, false, fmt.Sprintf("%s approved moving the redirected folders of %s to OneDrive:\r\n%s", admin, sid, strings.Join(lines, "\r\n")))
	return 0
}
