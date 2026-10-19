// Package remediate fixes orphaned library folders:
//
//  1. move the folder aside to <profile>\ODHelper\Fixing\<run-id>
//  2. copy every locally-stored file (not online-only placeholders) to
//     <BackupRoot>\<date-time>\<Library>, recording each one
//  3. delete only the copies that were verified and are unchanged (otherwise
//     put the folder back, or keep it in staging)
//  4. ask OneDrive to re-sync the library.
//
// An administrator must approve the fix (UAC), but the file work itself runs
// as the user: everything happens inside the user's own profile, and writing
// there with admin rights would let a user redirect those writes elsewhere.
package remediate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/denismcapple/OneDriveHelper/internal/config"
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
	// Interrupted lists unfinished earlier fixes (shown, never acted on here).
	Interrupted []Interrupted
}

// BuildPlan detects orphans for t and measures the locally stored data.
func BuildPlan(t Target, cfg config.Config) (*Plan, error) {
	p := &Plan{Target: t, BackupDir: cfg.BackupDir(t.ProfileDir), Interrupted: ListInterrupted(t.ProfileDir)}
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

func indent(s, prefix string) string {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString(prefix + l + "\n")
	}
	return b.String()
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
	if len(p.Interrupted) > 0 {
		b.WriteString("\nAn earlier fix did not finish:\n")
		for _, i := range p.Interrupted {
			b.WriteString(indent(i.Text(), "    "))
		}
	}
	fmt.Fprintf(&b, "\nFiles will be saved to: %s\\<date-time>\\<library>\nSpace needed: %s (incl. margin), available: %s\n",
		p.BackupDir, HumanBytes(int64(p.Required)), HumanBytes(int64(p.FreeBytes)))
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
