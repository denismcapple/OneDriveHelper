package agent

import (
	"os"
	"strings"
	"time"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/folders"
	"github.com/denismcapple/OneDriveHelper/internal/migrate"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
	"github.com/denismcapple/OneDriveHelper/internal/popup"
	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

const idMigrate = "move-to-onedrive"

// checkMigration offers to move the user's redirected folders to OneDrive
// once no Folder Redirection policy applies to them any more.
func (a *Agent) checkMigration(cfg config.Config, now time.Time) *migrate.Offer {
	check := a.migrationCheck
	if check == nil {
		check = func(cfg config.Config) (*migrate.Offer, string) {
			return migrate.Check(cfg, a.hive.SID, folders.CurrentUser, a.hive, a.profile)
		}
	}
	o, why := check(cfg)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fixing {
		return o
	}
	if why != a.migrateWhy {
		a.migrateWhy = why
		if why != "" && cfg.FolderRedirectionMigration {
			a.log.Printf("moving folders to OneDrive not offered: %s", why)
		}
	}
	if o == nil {
		a.migrateKey = ""
		a.ui.Hide(idMigrate)
		return nil
	}
	changed := o.Key() != a.migrateKey
	if changed {
		if a.migrateKey != "" {
			ClearSnooze(a.hive, idMigrate)
		}
		a.migrateKey = o.Key()
		a.log.Printf("offering to move %s to OneDrive", o.Names())
	}
	if Snoozed(a.hive, idMigrate, now, winapi.BootTime()) {
		a.ui.Hide(idMigrate)
		return o
	}
	if !a.ui.Visible(idMigrate) || changed {
		a.ui.Show(migrateSpec(cfg, o, a.actions()))
	}
	return o
}

func migrateSpec(c config.Config, o *migrate.Offer, a Actions) popup.Spec {
	s := popup.Spec{
		ID: idMigrate, Severity: popup.Info, Brand: c.Title(), Logo: c.LogoPath(), Sound: c.PopupSound, Accent: c.AccentColor,
		Title: "Back up your folders to OneDrive",
		Body:  "Your " + o.Names() + " are still kept on a network drive. They can now be moved to this computer and backed up to OneDrive.",
	}
	if b, ok := ignoreButton(c, a, idMigrate); ok {
		s.Buttons = append(s.Buttons, b)
	}
	s.Buttons = append(s.Buttons, supportButton(c, a), migrateButton(c, a))
	return s
}

func migrateButton(c config.Config, a Actions) popup.Button {
	label := "Move to OneDrive"
	if c.RequireAdminApproval {
		label += " (admin)"
	}
	return popup.Button{Label: label, Primary: true, Dismiss: true, Action: a.Migrate}
}

// migrate runs the move (one fix at a time, like the folder fix).
func (a *Agent) migrate() {
	a.runFix("move folders to OneDrive", func(busy func(bool), _ func() bool) {
		cfg := config.Load()
		o, why := migrate.Check(cfg, a.hive.SID, folders.CurrentUser, a.hive, a.profile)
		if o == nil {
			a.log.Printf("move folders to OneDrive: %s", why)
			return
		}
		exe, err := os.Executable()
		if err != nil {
			a.log.Printf("move folders to OneDrive: %v", err)
			return
		}
		migrate.SetSystem(a.restartExplorer, a.restartOneDriveForMigration)
		migrate.Run(cfg, o, a.user, a.hive.SID, a.profile, exe, a.log)
		a.mu.Lock()
		a.migrateKey = ""
		a.mu.Unlock()
	})
}

func (a *Agent) restartExplorer() error {
	pids := a.explorerPIDs()
	return folders.RestartExplorer(pids, func() bool { return len(a.explorerPIDs()) > 0 })
}

func (a *Agent) explorerPIDs() []uint32 {
	procs, _ := onedrive.Processes()
	var pids []uint32
	for _, p := range procs {
		if p.Session == a.session && strings.EqualFold(p.Name, "explorer.exe") {
			pids = append(pids, p.PID)
		}
	}
	return pids
}

// restartOneDriveForMigration makes OneDrive read its policy again, so it
// backs up the folders now instead of at the next sign-in.
func (a *Agent) restartOneDriveForMigration() error {
	if a.oneDriveState() == onedrive.StateRunning {
		return onedrive.RestartOneDrive(a.hive, a.session)
	}
	return onedrive.LaunchOneDrive(a.hive)
}
