// Package remediate fixes orphaned library folders:
//
//  1. copy every locally-stored file (not online-only placeholders) to
//     <profile>\ODHelper\Saved\<date-time>\<Library>
//  2. delete the orphaned folder (moving anything undeletable aside)
//  3. ask OneDrive to re-sync the library.
//
// An administrator must approve the fix (UAC), but the file work itself runs
// as the user: everything happens inside the user's own profile, and writing
// there with admin rights would let a user redirect those writes elsewhere.
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

	"golang.org/x/sys/windows"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/elog"
	"github.com/denismcapple/OneDriveHelper/internal/fsutil"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

// Target is the user whose folders are being remediated.
type Target struct {
	SID        string
	UserName   string
	Hive       onedrive.Hive
	ProfileDir string
}

// CurrentUserTarget targets the user running this process.
func CurrentUserTarget() (Target, error) {
	sid, err := winapi.CurrentUserSID()
	if err != nil {
		return Target{}, err
	}
	profile, err := os.UserHomeDir()
	if err != nil {
		return Target{}, err
	}
	return Target{SID: sid, UserName: os.Getenv("USERDOMAIN") + `\` + os.Getenv("USERNAME"), Hive: onedrive.CurrentUserHive(), ProfileDir: profile}, nil
}

// TargetForSID targets another (logged-on) user; used by the elevated approval step.
func TargetForSID(sid string) (Target, error) {
	s, err := windows.StringToSid(sid)
	if err != nil {
		return Target{}, fmt.Errorf("invalid SID %q: %w", sid, err)
	}
	h := onedrive.UserHive(s.String())
	k, err := h.Open(`Software`, 0x20019) // KEY_READ
	if err != nil {
		return Target{}, fmt.Errorf("the registry of user %s is not loaded (is the user logged on?): %w", sid, err)
	}
	k.Close()
	profile := onedrive.ProfileDir(s.String())
	if profile == "" {
		return Target{}, fmt.Errorf("no profile directory found for %s", sid)
	}
	name := s.String()
	if acct, dom, _, err := s.LookupAccount(""); err == nil {
		name = dom + `\` + acct
	}
	return Target{SID: s.String(), UserName: name, Hive: h, ProfileDir: profile}, nil
}

// Item is one orphaned folder in a plan.
type Item struct {
	Orphan     onedrive.Orphan
	Stats      Stats
	OnlineOnly []string
	SaveAs     string // folder name under the run folder
}

// Plan describes what remediation will do.
type Plan struct {
	Target     Target
	Items      []Item
	Excluded   []string // orphans outside the profile; never touched automatically
	BackupDir  string   // resolved BackupRoot, e.g. C:\Users\jsmith\ODHelper\Saved
	LocalBytes int64
	FreeBytes  uint64
	Required   uint64
}

// BuildPlan detects orphans for t and measures the locally stored data.
func BuildPlan(t Target, cfg config.Config) (*Plan, error) {
	p := &Plan{Target: t, BackupDir: cfg.BackupDir(t.ProfileDir)}
	if !filepath.IsAbs(p.BackupDir) || len(p.BackupDir) <= 3 {
		return p, fmt.Errorf("BackupRoot %q must resolve to an absolute folder (not a drive root)", cfg.BackupRoot)
	}
	used := map[string]bool{}
	for _, o := range onedrive.FindOrphans(onedrive.Gather(t.Hive, t.ProfileDir), onedrive.OSFS) {
		if !insideProfile(o.Path, t.ProfileDir) {
			p.Excluded = append(p.Excluded, o.Path+" (outside the user profile)")
			continue
		}
		if len(o.KnownFolders) > 0 { // removing it would break the user's Desktop/Documents
			p.Excluded = append(p.Excluded, o.Path+" (contains the user's "+strings.Join(o.KnownFolders, "/")+" folder)")
			continue
		}
		if within(p.BackupDir, o.Path) {
			return p, fmt.Errorf("BackupRoot %s is inside the orphaned folder %s", p.BackupDir, o.Path)
		}
		st, online := Scan(o.Path)
		p.Items = append(p.Items, Item{Orphan: o, Stats: st, OnlineOnly: online, SaveAs: saveName(o, used)})
		p.LocalBytes += st.LocalBytes
	}
	p.Required = uint64(p.LocalBytes) + uint64(cfg.FreeSpaceMarginMB)<<20
	free, err := freeSpace(p.BackupDir)
	if err != nil {
		return p, fmt.Errorf("check free space for %s: %w", p.BackupDir, err)
	}
	p.FreeBytes = free
	return p, nil
}

// saveName returns the library folder name, adding " (<Org>)" if two orphans
// from different organisations share a name.
func saveName(o onedrive.Orphan, used map[string]bool) string {
	name := o.Name
	if used[strings.ToLower(name)] {
		name = fmt.Sprintf("%s (%s)", o.Name, filepath.Base(o.TenantRoot))
		for i := 2; used[strings.ToLower(name)]; i++ {
			name = fmt.Sprintf("%s (%s %d)", o.Name, filepath.Base(o.TenantRoot), i)
		}
	}
	used[strings.ToLower(name)] = true
	return name
}

// Paths returns the orphan paths of the plan (for comparing two plans).
func (p *Plan) Paths() string {
	var s []string
	for _, it := range p.Items {
		s = append(s, onedrive.Clean(it.Orphan.Path))
	}
	return strings.Join(s, "|")
}

func insideProfile(path, profile string) bool {
	c, pr := onedrive.Clean(path), onedrive.Clean(profile)
	return pr != "" && strings.HasPrefix(c, strings.TrimSuffix(pr, `\`)+`\`)
}

func within(child, parent string) bool {
	c, p := onedrive.Clean(child), onedrive.Clean(parent)
	return c == p || strings.HasPrefix(c, strings.TrimSuffix(p, `\`)+`\`)
}

// EnoughSpace reports whether the backup volume can hold the local data plus margin.
func (p *Plan) EnoughSpace() bool { return p.FreeBytes >= p.Required }

// Summary renders the plan for the confirmation dialogs / CLI.
func (p *Plan) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "User: %s\n\n", p.Target.UserName)
	for _, it := range p.Items {
		fmt.Fprintf(&b, "• %s\n    %d file(s) stored on this PC (%s) will be saved; %d online-only file(s) are already in the cloud.\n",
			it.Orphan.Path, it.Stats.LocalFiles, HumanBytes(it.Stats.LocalBytes), it.Stats.OnlineOnlyFiles)
		switch {
		case it.Orphan.EmptyOrg:
			b.WriteString("    Empty leftover organisation folder; it will just be removed.\n")
		case it.Orphan.Personal:
			b.WriteString("    Old personal OneDrive folder that no signed-in account syncs any more; nothing to re-sync.\n")
		case it.Orphan.CanResync():
			b.WriteString("    Will be re-synced automatically afterwards.\n")
		case it.Orphan.WebURL != "":
			b.WriteString("    The library web page will be opened so the user can click 'Sync'.\n")
		default:
			b.WriteString("    Cannot be re-synced automatically (library details unknown).\n")
		}
		if len(it.Stats.Errors) > 0 {
			fmt.Fprintf(&b, "    WARNING: %d item(s) could not be read; this folder will NOT be removed.\n", len(it.Stats.Errors))
		}
	}
	for _, e := range p.Excluded {
		fmt.Fprintf(&b, "• %s - skipped, fix manually\n", e)
	}
	fmt.Fprintf(&b, "\nFiles will be saved to: %s\\<date-time>\\<library>\nSpace needed: %s (incl. margin), available: %s\n",
		p.BackupDir, HumanBytes(int64(p.Required)), HumanBytes(int64(p.FreeBytes)))
	return b.String()
}

// Outcome is the result for one folder.
type Outcome struct {
	Path           string   `json:"path"`
	SavedTo        string   `json:"savedTo"`
	Copied         Stats    `json:"copied"`
	OnlineOnly     []string `json:"onlineOnlyFiles,omitempty"`
	Removed        bool     `json:"removed"`
	LeftoversMoved string   `json:"leftoversMovedTo,omitempty"`
	Error          string   `json:"error,omitempty"`
	ResyncURL      string   `json:"resyncUrl,omitempty"`
	WebURL         string   `json:"webUrl,omitempty"`
}

// Result is written to manifest.json in the run folder.
type Result struct {
	User       string    `json:"user"`
	SID        string    `json:"sid"`
	ApprovedBy string    `json:"approvedBy,omitempty"`
	Started    time.Time `json:"started"`
	Finished   time.Time `json:"finished"`
	RunDir     string    `json:"runDir"`
	Outcomes   []Outcome `json:"outcomes"`
	// ExplorerEntriesRemoved lists navigation-pane items that pointed at removed folders.
	ExplorerEntriesRemoved []string `json:"explorerEntriesRemoved,omitempty"`
}

// OK reports whether every folder was removed.
func (r *Result) OK() bool {
	for _, o := range r.Outcomes {
		if !o.Removed {
			return false
		}
	}
	return len(r.Outcomes) > 0
}

// execEnv holds the steps of Execute that touch the registry, Explorer or the
// file system in ways tests need to replace or make fail. The defaults are the
// real implementations.
type execEnv struct {
	active        func(t Target, path string) bool // is OneDrive syncing path (or can't we tell)?
	navCleanup    func(t Target, lg *log.Logger) []string
	tenantCleanup func(root string, t Target)
}

func defaultEnv() execEnv {
	return execEnv{
		active: func(t Target, path string) bool {
			return onedrive.Gather(t.Hive, t.ProfileDir).IsActive(path)
		},
		navCleanup:    removeStaleNavEntries,
		tenantCleanup: cleanupTenantRoot,
	}
}

// env is replaced by tests.
var env = defaultEnv()

// removeStaleNavEntries removes Explorer navigation-pane entries that now point at nothing.
func removeStaleNavEntries(t Target, lg *log.Logger) []string {
	var removed []string
	in := onedrive.Gather(t.Hive, t.ProfileDir)
	if stale := onedrive.StaleNavEntries(t.Hive, in, nil); len(stale) > 0 {
		for _, e := range stale {
			if err := onedrive.RemoveNavEntry(t.Hive, e); err != nil {
				lg.Printf("remove Explorer entry %s (%s): %v", e.Name, e.Target, err)
			} else {
				lg.Printf("removed Explorer entry %s (%s)", e.Name, e.Target)
				removed = append(removed, e.Name)
			}
		}
		onedrive.RefreshExplorer()
	}
	return removed
}

// Execute performs the plan as the current (target) user.
func Execute(p *Plan, lg *log.Logger, ev *elog.Log) (*Result, error) {
	res := &Result{User: p.Target.UserName, SID: p.Target.SID, Started: time.Now()}
	if !p.EnoughSpace() {
		return res, fmt.Errorf("not enough free space: need %s, have %s", HumanBytes(int64(p.Required)), HumanBytes(int64(p.FreeBytes)))
	}
	if err := os.MkdirAll(p.BackupDir, 0o755); err != nil {
		return res, fmt.Errorf("create %s: %w", p.BackupDir, err)
	}
	res.RunDir = filepath.Join(p.BackupDir, res.Started.Format("2006-01-02_150405"))
	if err := os.Mkdir(res.RunDir, 0o755); err != nil {
		return res, fmt.Errorf("create %s: %w", res.RunDir, err)
	}
	ev.Info(elog.RemediationStarted, fmt.Sprintf("Remediation started for %s; saving to %s.", p.Target.UserName, res.RunDir))

	for _, it := range p.Items {
		o := it.Orphan
		out := Outcome{Path: o.Path, SavedTo: filepath.Join(res.RunDir, it.SaveAs), WebURL: o.WebURL}
		if o.CanResync() {
			out.ResyncURL = o.Library.ResyncURL()
		}
		// Re-check right before acting: never touch a folder OneDrive has picked up again.
		if env.active(p.Target, o.Path) {
			out.Error = "folder is being synced again; left untouched"
			res.Outcomes = append(res.Outcomes, out)
			continue
		}
		lg.Printf("saving %s -> %s", o.Path, out.SavedTo)
		out.Copied, out.OnlineOnly = CopyLocal(o.Path, out.SavedTo)
		if len(out.Copied.Errors) > 0 {
			out.Error = fmt.Sprintf("%d item(s) could not be saved; original folder kept", len(out.Copied.Errors))
			lg.Printf("%s: %s: %v", o.Path, out.Error, out.Copied.Errors)
			res.Outcomes = append(res.Outcomes, out)
			continue
		}
		if err := DeleteTree(o.Path); err != nil {
			lg.Printf("delete %s: %v", o.Path, err)
			left := filepath.Join(res.RunDir, "_Leftovers", it.SaveAs)
			if mErr := moveDir(o.Path, left); mErr != nil {
				out.Error = fmt.Sprintf("could not remove folder: %v; could not move it aside: %v", err, mErr)
			} else {
				out.LeftoversMoved = left
			}
		}
		out.Removed = !exists(o.Path)
		res.Outcomes = append(res.Outcomes, out)
		env.tenantCleanup(o.TenantRoot, p.Target)
	}
	res.ExplorerEntriesRemoved = env.navCleanup(p.Target, lg)
	res.Finished = time.Now()
	if b, err := json.MarshalIndent(res, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(res.RunDir, "manifest.json"), b, 0o644)
	}
	msg := fmt.Sprintf("Remediation for %s finished. Run folder: %s\r\n%s", p.Target.UserName, res.RunDir, res.Text())
	if res.OK() {
		ev.Info(elog.RemediationCompleted, msg)
	} else {
		ev.Error(elog.RemediationFailed, msg)
	}
	return res, nil
}

// Text summarises the result.
func (r *Result) Text() string {
	var b strings.Builder
	for _, o := range r.Outcomes {
		status := "removed"
		if !o.Removed {
			status = "NOT removed"
		}
		fmt.Fprintf(&b, "• %s: %d file(s) saved, %s", o.Path, o.Copied.LocalFiles, status)
		if o.LeftoversMoved != "" {
			b.WriteString(" (undeletable items moved to _Leftovers)")
		}
		if o.Error != "" {
			b.WriteString(" - " + o.Error)
		}
		b.WriteString("\n")
	}
	if len(r.ExplorerEntriesRemoved) > 0 {
		b.WriteString("• Removed from Explorer's side panel: " + strings.Join(r.ExplorerEntriesRemoved, ", ") + "\n")
	}
	return b.String()
}

// cleanupTenantRoot removes the organisation folder if it is now empty apart
// from desktop.ini and nothing under it is synced.
func cleanupTenantRoot(root string, t Target) {
	if onedrive.Gather(t.Hive, t.ProfileDir).IsActive(root) || !insideProfile(root, t.ProfileDir) {
		return
	}
	entries, err := fsutil.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.EqualFold(e.Name, "desktop.ini") {
			return
		}
	}
	for _, e := range entries {
		_ = removeFile(filepath.Join(root, e.Name))
	}
	_ = removeDir(root)
}

func freeSpace(path string) (uint64, error) {
	// Walk up to the nearest existing ancestor (the backup folder may not exist yet).
	for p := path; ; p = filepath.Dir(p) {
		if _, err := os.Stat(p); err == nil {
			u, _ := windows.UTF16PtrFromString(p)
			var free uint64
			err := windows.GetDiskFreeSpaceEx(u, &free, nil, nil)
			return free, err
		}
		if filepath.Dir(p) == p {
			return 0, errors.New("no existing folder found")
		}
	}
}

func exists(p string) bool {
	_, err := os.Lstat(winapi.LongPath(p))
	return err == nil
}

// HumanBytes formats a byte count.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
