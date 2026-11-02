// OneDriveHelper.exe is the LocalSystem service and the admin command line.
// It also carries the event source's message table (eventmsgs_windows_*.syso).
package main

//go:generate go run ../../tools/eventmsgs

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/sys/windows/svc"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/folders"
	"github.com/denismcapple/OneDriveHelper/internal/kfm"
	"github.com/denismcapple/OneDriveHelper/internal/migrate"
	"github.com/denismcapple/OneDriveHelper/internal/odstatus"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
	"github.com/denismcapple/OneDriveHelper/internal/remediate"
	"github.com/denismcapple/OneDriveHelper/internal/service"
	"github.com/denismcapple/OneDriveHelper/internal/version"
	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

const usage = `OneDrive Helper %s

Usage: OneDriveHelper.exe <command>

Admin commands (run from an elevated prompt):
  install [--config <file.json>]
                          Copy to Program Files, register and start the service,
                          applying the settings file if given
  uninstall [--purge]     Remove the service (--purge also deletes configuration)
  start | stop            Start or stop the service
  config show             Show effective settings and where each comes from
  config defaults         Write any missing default settings to the registry
  config export <file>    Export HKLM\SOFTWARE\OneDriveHelper to JSON
  config import <file>    Validate and import settings from JSON
  config validate <file>  Check a settings file without applying it

Diagnostics (run as the user to check):
  check                   Show OneDrive status, orphaned library folders and
                          redirected folders (and whether moving them is offered)
  remediate --dry-run     Show what the fix would do (no changes)
  version
`

func main() {
	if isSvc, err := svc.IsWindowsService(); err == nil && isSvc {
		if err := service.Run(); err != nil {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) < 2 {
		fmt.Printf(usage, version.Version)
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func logf(f string, a ...any) { fmt.Printf(f+"\n", a...) }

func needAdmin() error {
	if !winapi.IsElevated() {
		return fmt.Errorf("this command must be run from an elevated (Run as administrator) prompt")
	}
	return nil
}

func run(cmd string, args []string) error {
	switch strings.ToLower(cmd) {
	case "install":
		fs := flag.NewFlagSet("install", flag.ExitOnError)
		cfgFile := fs.String("config", "", "JSON settings file to apply (from OneDriveHelperConfig.exe)")
		_ = fs.Parse(args)
		if err := needAdmin(); err != nil {
			return err
		}
		return service.Install(*cfgFile, logf)
	case "uninstall":
		fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
		purge := fs.Bool("purge", false, "also delete configuration")
		_ = fs.Parse(args)
		if err := needAdmin(); err != nil {
			return err
		}
		return service.Uninstall(*purge, logf)
	case "start":
		return service.Start()
	case "stop":
		return service.Stop()
	case "config":
		return configCmd(args)
	case "check":
		return check()
	case "remediate":
		fs := flag.NewFlagSet("remediate", flag.ExitOnError)
		dry := fs.Bool("dry-run", false, "show the plan only")
		sid := fs.String("sid", "", "target another logged-on user (requires admin)")
		_ = fs.Parse(args)
		if !*dry {
			return fmt.Errorf("only --dry-run is supported here; users fix folders via the alert's 'Fix' button")
		}
		return dryRun(*sid)
	case "version", "--version", "-v":
		fmt.Println(version.Version)
		return nil
	case "help", "--help", "-h", "/?":
		fmt.Printf(usage, version.Version)
		return nil
	}
	return fmt.Errorf("unknown command %q (try 'help')", cmd)
}

func configCmd(args []string) error {
	if len(args) == 0 {
		args = []string{"show"}
	}
	switch args[0] {
	case "show":
		_, settings := config.LoadDetailed()
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "SETTING\tVALUE\tSOURCE\tDESCRIPTION")
		for _, s := range settings {
			fmt.Fprintf(w, "%s\t%v\t%s\t%s\n", s.Name, s.Value, s.Source, s.Help)
		}
		return w.Flush()
	case "defaults":
		if err := needAdmin(); err != nil {
			return err
		}
		return config.WriteDefaults()
	case "export", "import", "validate":
		if len(args) < 2 {
			return fmt.Errorf("usage: config %s <file.json>", args[0])
		}
		switch args[0] {
		case "export":
			return config.Export(args[1])
		case "validate":
			if _, _, err := config.ReadFile(args[1]); err != nil {
				return err
			}
			fmt.Println("Settings file is valid.")
			return nil
		}
		if err := needAdmin(); err != nil {
			return err
		}
		return config.Import(args[1])
	}
	return fmt.Errorf("unknown config command %q", args[0])
}

// checkStatus prints OneDrive's own status for each sync root (Windows 11 /
// Server 2025), or the Cloud Files driver's provider status elsewhere.
func checkStatus(sid string) {
	fmt.Printf("\nOneDrive status (as OneDrive reports it):\n")
	if winapi.IsElevated() {
		fmt.Printf("  NOTE: this prompt is elevated. OneDrive answers an elevated program from a separate copy of itself, so\n" +
			"  the status below may say OneDrive isn't running. Run 'check' from a normal prompt for the real status.\n")
	}
	roots, err := odstatus.Read(sid, 10*time.Second)
	if err == nil && len(roots) == 0 {
		fmt.Printf("  no OneDrive folders are registered for this user (OneDrive isn't signed in to an account yet)\n")
		return
	}
	if err == nil {
		for _, r := range roots {
			if r.Err != "" {
				fmt.Printf("  %s: %s\n", r.Name, r.Err)
				continue
			}
			fmt.Printf("  %s: %s - %q", r.Name, r.State, r.Status)
			if r.Message != "" {
				fmt.Printf(", %q", r.Message)
			}
			if r.Action != "" {
				fmt.Printf(" [%s]", r.Action)
			}
			fmt.Println()
		}
		return
	}
	fmt.Printf("  %v\n", err)
	if errors.Is(err, odstatus.ErrUnsupported) {
		fmt.Printf("  Cloud Files provider status instead (0 = not connected, 1 = idle, 0x40 = no connection, 0xFFFFFFFF = error):\n")
		for _, p := range onedrive.RegisteredSyncRoots(sid) {
			if info, err := winapi.SyncRootInfo(p); err == nil {
				fmt.Printf("    %s: %#x\n", p, info.Status)
			}
		}
	}
}

// checkRedirection prints what decides whether moving redirected folders to
// OneDrive backup is offered.
func checkRedirection(h onedrive.Hive, profile string) {
	fmt.Printf("\nFolder Redirection:\n")
	fs, err := folders.Redirected(folders.CurrentUser, profile)
	if err != nil {
		fmt.Printf("  folder locations could not be read: %v\n", err)
	}
	if len(fs) == 0 && err == nil {
		fmt.Printf("  no folders point outside the profile\n")
	}
	for _, f := range fs {
		name, note := f.Name, ""
		switch {
		case !f.Known():
			note = "  (unknown folder: left alone)"
		case f.ID == "":
			name = f.Name + " (2nd entry)" // Windows' "local" duplicate of the folder
		}
		fmt.Printf("  %-22s %s%s\n", name, f.Current, note)
	}
	gpos, err := folders.PolicyGPOs("")
	switch {
	case err != nil:
		fmt.Printf("  Folder Redirection policies applied: can't tell (%v)\n", err)
	case len(gpos) == 0:
		fmt.Printf("  Folder Redirection policies applied: none\n")
	default:
		fmt.Printf("  Folder Redirection policies applied: %s\n", strings.Join(gpos, ", "))
	}
	if why := winapi.SharedHostReasons(); len(why) > 0 {
		fmt.Printf("  Shared or virtual desktop host: %s\n", strings.Join(why, ", "))
	}
	t := kfm.Tenant()
	if t == "" {
		t = "not set"
	}
	fmt.Printf("  OneDrive KFMSilentOptIn policy: %s\n", t)
	work := "none"
	for _, a := range onedrive.Accounts(h) {
		if a.Business() && a.Configured() {
			if kfm.ValidTenant(a.TenantID) {
				work = fmt.Sprintf("%s (%s, tenant %s)", a.Key, a.UserEmail, a.TenantID)
				break
			}
			work = a.Key + " is set up but not signed in (no tenant)"
		}
	}
	fmt.Printf("  OneDrive work account: %s\n", work)
	if o, why := migrate.Check(config.Load(), h.SID, folders.CurrentUser, h, profile); o != nil {
		fmt.Printf("  Moving %s to OneDrive backup: OFFERED\n", o.Names())
	} else {
		fmt.Printf("  Moving folders to OneDrive backup: not offered (%s)\n", why)
	}
}

func check() error {
	h := onedrive.CurrentUserHive()
	session := winapi.CurrentSessionID()
	accts := onedrive.Accounts(h)
	fmt.Printf("Session %d\n\nAccounts:\n", session)
	for _, a := range accts {
		if a.Configured() {
			fmt.Printf("  %-10s %s  (%s)\n", a.Key, a.UserEmail, a.UserFolder)
			for _, f := range a.SyncedFolders {
				fmt.Printf("               synced: %s\n", f)
			}
		}
	}
	exe := onedrive.OneDriveExe(h)
	fmt.Printf("\nOneDrive.exe: %s\nRunning in this session: %v\nUpdate in progress: %v\n",
		exe, onedrive.OneDriveState(session), onedrive.UpdateInProgress())

	libs := onedrive.ReadLibraries(os.Getenv("LOCALAPPDATA"), accts)
	fmt.Printf("\nLibraries known to OneDrive (with re-sync IDs): %d\n", len(libs))
	for _, l := range libs {
		fmt.Printf("  %s - %s  ->  %s\n", l.SiteTitle, l.ListTitle, l.MountPoint)
	}

	profile, _ := os.UserHomeDir()
	in := onedrive.Gather(h, profile)
	orphans := onedrive.FindOrphans(in, onedrive.OSFS)
	if stale := onedrive.StaleNavEntries(h, in, nil); len(stale) > 0 {
		fmt.Printf("\nExplorer side-panel entries pointing at missing folders: %d (the agent removes these after 5 minutes)\n", len(stale))
		for _, e := range stale {
			fmt.Printf("  %s -> %s\n", e.Name, e.Target)
		}
	}
	fmt.Printf("\nOrphaned library folders: %d\n", len(orphans))
	for _, o := range orphans {
		fmt.Printf("  %s\n", o.Path)
		switch {
		case len(o.KnownFolders) > 0:
			fmt.Printf("    contains the user's %s folder - will not be fixed automatically\n", strings.Join(o.KnownFolders, "/"))
		case o.EmptyOrg:
			fmt.Printf("    empty leftover organisation folder (nothing to re-sync)\n")
		case o.Kind == onedrive.KindMoved:
			fmt.Printf("    old copy: the library now syncs to %s (not re-synced)\n", o.MovedTo)
		case o.Kind == onedrive.KindDisconnected:
			fmt.Printf("    from an account that is no longer connected (not re-synced)\n")
		case o.CanResync():
			fmt.Printf("    re-sync: %s\n", o.Library.ResyncURL())
		case o.WebURL != "":
			fmt.Printf("    re-sync IDs unknown; site: %s\n", o.WebURL)
		default:
			fmt.Printf("    re-sync IDs unknown\n")
		}
	}
	checkStatus(h.SID)
	checkRedirection(h, profile)
	if len(in.ReadErrors) > 0 {
		fmt.Printf("\nOneDrive data that could not be read (no folder will be fixed until this is resolved):\n")
		for _, e := range in.ReadErrors {
			fmt.Printf("  %s\n", e)
		}
	}
	if list := remediate.ListInterrupted(profile); len(list) > 0 {
		fmt.Printf("\nUnfinished fixes: %d\n", len(list))
		for _, i := range list {
			for _, l := range strings.Split(strings.TrimSpace(i.Text()), "\n") {
				fmt.Printf("  %s\n", l)
			}
		}
	}
	return nil
}

func dryRun(sid string) error {
	t, err := remediate.CurrentUserTarget()
	if sid != "" {
		t, err = remediate.TargetForSID(sid)
	}
	if err != nil {
		return err
	}
	plan, err := remediate.BuildPlan(t, config.Load())
	if err != nil {
		return err
	}
	if len(plan.Items) == 0 && len(plan.Excluded) == 0 && len(plan.Interrupted) == 0 {
		fmt.Println("No orphaned library folders found.")
		return nil
	}
	fmt.Print(plan.Summary())
	fmt.Printf("Enough space: %v\n", plan.EnoughSpace())
	for _, it := range plan.Items {
		for _, e := range it.Stats.Errors {
			fmt.Printf("  read error: %s\n", e)
		}
	}
	return nil
}
