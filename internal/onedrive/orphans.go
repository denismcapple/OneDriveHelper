package onedrive

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/windows/registry"

	"github.com/denismcapple/OneDriveHelper/internal/fsutil"
)

// Orphan is a library folder that exists on disk but that OneDrive is no
// longer syncing.
type Orphan struct {
	Path       string
	TenantRoot string
	Name       string
	Library    *Library // cached IDs (nil if never seen while healthy)
	WebURL     string   // best-effort SharePoint site URL
	// EmptyOrg marks a leftover organisation folder that contains no
	// libraries at all (Path == TenantRoot). There is nothing to re-sync.
	EmptyOrg bool
	// KnownFolders lists Windows folders (Desktop, Documents, ...) that are
	// redirected into this folder. Such folders are never removed automatically.
	KnownFolders []string
}

// Org is the organisation folder name, e.g. "Contoso".
func (o Orphan) Org() string { return filepath.Base(o.TenantRoot) }

// DisplayName is "Library (Org)" or "Org (empty organisation folder)".
func (o Orphan) DisplayName() string {
	if o.EmptyOrg {
		return o.Name + " (empty organisation folder)"
	}
	if o.TenantRoot == "" {
		return o.Name
	}
	return o.Name + " (" + o.Org() + ")"
}

// CanResync reports whether we hold the IDs required for an odopen:// re-sync.
func (o Orphan) CanResync() bool { return o.Library != nil && o.Library.UserEmail != "" }

// Inputs is everything orphan detection needs, gathered from the registry.
type Inputs struct {
	ProfileDir string
	Accounts   []Account
	Providers  []Provider
	SyncRoots  []string
	Known      []Library
	Policies   []Library // re-sync IDs recovered from ClientPolicy_*.ini (matched by URL)
	// ShellFolders are the user's Desktop/Documents/... locations.
	ShellFolders map[string]string // name -> path
	// ReadErrors lists sources that exist but could not be read. When it is
	// not empty the data may be incomplete, so IsActive treats every path as
	// active and nothing should be changed on the strength of it.
	ReadErrors []string
}

const userShellFoldersPath = `Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders`

// ShellFolders reads the user's known-folder locations (Desktop, Documents,
// Pictures, ...), expanding %USERPROFILE% to profileDir.
func ShellFolders(h Hive, profileDir string) map[string]string {
	return shellFolders(h, profileDir, nil)
}

func shellFolders(h Hive, profileDir string, r *readErrs) map[string]string {
	k, err := h.Open(userShellFoldersPath, registry.QUERY_VALUE)
	if err != nil {
		r.add(h.Prefix+userShellFoldersPath, err)
		return nil
	}
	defer k.Close()
	names, err := k.ReadValueNames(-1)
	r.add(h.Prefix+userShellFoldersPath, err)
	out := map[string]string{}
	for _, n := range names {
		v, _, err := k.GetStringValue(n)
		if err != nil || v == "" {
			continue
		}
		if i := strings.Index(strings.ToUpper(v), "%USERPROFILE%"); i >= 0 && profileDir != "" {
			v = v[:i] + profileDir + v[i+len("%USERPROFILE%"):]
		}
		if x, err := registry.ExpandString(v); err == nil {
			v = x
		}
		out[n] = v
	}
	return out
}

var shellFolderNames = map[string]string{
	"Personal": "Documents", "My Pictures": "Pictures", "My Music": "Music", "My Video": "Videos",
	"{374DE290-123F-4565-9164-39C4925E467B}": "Downloads",
}

// knownFoldersIn returns friendly names of shell folders located in p.
func (in Inputs) knownFoldersIn(p string) []string {
	var out []string
	for name, path := range in.ShellFolders {
		if within(Clean(path), Clean(p)) {
			if f, ok := shellFolderNames[name]; ok {
				name = f
			}
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// FS abstracts directory access for testing.
type FS interface {
	ReadDir(dir string) ([]fsutil.Entry, error)
	ReadFile(path string) ([]byte, error)
}

type osFS struct{}

func (osFS) ReadDir(d string) ([]fsutil.Entry, error) { return fsutil.ReadDir(d) }
func (osFS) ReadFile(p string) ([]byte, error)        { return os.ReadFile(p) }

// OSFS is the real file system.
var OSFS FS = osFS{}

// Gather collects detection inputs for the user behind h.
//
// Sources that exist but cannot be read are listed in ReadErrors. The
// ClientPolicy files are not: they only supply re-sync IDs, never decide
// whether a folder is active.
func Gather(h Hive, profileDir string) Inputs {
	r := &readErrs{}
	accts := accounts(h, r)
	in := Inputs{
		ProfileDir: profileDir,
		Accounts:   accts,
		Providers:  providers(h, r),
		SyncRoots:  registeredSyncRoots(Hive{Root: registry.LOCAL_MACHINE}, h.SID, r),
		Known:      loadLibraries(h, r),
	}
	if profileDir != "" {
		in.Policies = ReadClientPolicies(filepath.Join(profileDir, "AppData", "Local"), accts)
	}
	in.ShellFolders = shellFolders(h, profileDir, r)
	in.ReadErrors = r.list
	return in
}

// ActivePaths returns every folder that any source says OneDrive is syncing.
// A folder counts as active if ANY source lists it; this errs on the side of
// never flagging (or remediating) a live library.
func (in Inputs) ActivePaths() []string {
	var out []string
	for _, a := range in.Accounts {
		out = append(out, a.UserFolder)
		out = append(out, a.SyncedFolders...)
	}
	out = append(out, in.SyncRoots...)
	return cleanAll(out)
}

// IsActive reports whether p is, contains, or is inside an active sync folder.
// If any OneDrive data could not be read, every path counts as active.
func (in Inputs) IsActive(p string) bool {
	return len(in.ReadErrors) > 0 || overlaps(Clean(p), in.ActivePaths())
}

func overlaps(p string, active []string) bool {
	for _, a := range active {
		if within(p, a) || within(a, p) {
			return true
		}
	}
	return false
}

// FindOrphans returns orphaned library folders, sorted by path.
func FindOrphans(in Inputs, fs FS) []Orphan {
	active := in.ActivePaths()
	profile := Clean(in.ProfileDir)

	history := map[string]bool{}
	for _, p := range in.Providers {
		history[Clean(p.MountPoint)] = true
	}
	for _, l := range in.Known {
		history[Clean(l.MountPoint)] = true
	}
	delete(history, "")

	roots := map[string]string{} // clean -> original
	addRoot := func(p string) {
		c := Clean(p)
		if c == "" || isVolumeRoot(c) || (profile != "" && within(profile, c)) {
			return // never scan a drive root, the profile itself or its ancestors
		}
		for _, a := range active {
			if within(c, a) {
				return // inside a synced folder (e.g. the personal OneDrive)
			}
		}
		roots[c] = filepath.Clean(p)
	}
	for _, a := range in.Accounts {
		if !a.Business() {
			continue
		}
		for _, f := range a.SyncedFolders {
			addRoot(filepath.Dir(f))
		}
	}
	for _, p := range in.Providers {
		if t := strings.ToLower(p.LibraryType); p.MountPoint != "" && t != "personal" && t != "mysite" {
			addRoot(filepath.Dir(p.MountPoint))
		}
	}
	for _, l := range in.Known {
		if l.MountPoint != "" {
			addRoot(filepath.Dir(l.MountPoint))
		}
	}
	var out []Orphan
	// Folders in the profile carrying OneDrive's desktop.ini: plain ones are
	// organisation folders. "OneDrive..." ones are personal OneDrive folders,
	// which are never reported (a fresh PC always has an unused, empty
	// "OneDrive" folder) and never scanned as organisation folders.
	if in.ProfileDir != "" {
		entries, _ := fs.ReadDir(in.ProfileDir)
		for _, e := range entries {
			if !e.IsDir() || e.IsLink() {
				continue
			}
			dir := filepath.Join(in.ProfileDir, e.Name)
			b, err := fs.ReadFile(filepath.Join(dir, "desktop.ini"))
			odIni := err == nil && strings.Contains(strings.ToLower(decodeText(b)), "onedrive.exe")
			switch {
			case strings.HasPrefix(strings.ToLower(e.Name), "onedrive") && (e.IsCloud() || odIni):
				continue
			case odIni && e.Attr&fsutil.AttrReparsePoint == 0:
				addRoot(dir)
			}
		}
	}

	for c, root := range roots {
		entries, err := fs.ReadDir(root)
		if err != nil {
			continue
		}
		// A folder carrying OneDrive's organisation-folder desktop.ini is
		// OneDrive's own; anything in it that isn't syncing is orphaned,
		// even if it has lost its placeholder attributes.
		orgFolder := false
		if b, err := fs.ReadFile(filepath.Join(root, "desktop.ini")); err == nil {
			orgFolder = strings.Contains(strings.ToLower(decodeText(b)), "onedrive.exe")
		}
		if orgFolder && !overlaps(c, active) && onlyDesktopIni(entries) {
			out = append(out, Orphan{Path: root, TenantRoot: root, Name: filepath.Base(root), EmptyOrg: true})
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || e.IsLink() || e.Attr&hiddenSystem == hiddenSystem {
				continue
			}
			p := filepath.Join(root, e.Name)
			c := Clean(p)
			if overlaps(c, active) || !(orgFolder || e.IsCloud() || history[c]) {
				continue
			}
			o := Orphan{Path: p, TenantRoot: root, Name: e.Name, Library: in.knownFor(c)}
			if o.Library != nil {
				o.WebURL = webURL(o.Library.WebURL)
				if o.WebURL == "" || o.Library.UserEmail == "" {
					o.Library = nil // not enough to build an odopen:// link
				}
			}
			for _, pr := range in.Providers {
				if o.WebURL == "" && Clean(pr.MountPoint) == c {
					o.WebURL = webURL(pr.WebURL)
				}
			}
			out = append(out, o)
		}
	}
	for i := range out {
		out[i].KnownFolders = in.knownFoldersIn(out[i].Path)
	}
	sort.Slice(out, func(i, j int) bool { return Clean(out[i].Path) < Clean(out[j].Path) })
	return out
}

// knownFor finds re-sync details for a folder: our own cache first, then a
// ClientPolicy file whose URL matches the folder's (stale) SyncEngines entry.
func (in Inputs) knownFor(c string) *Library {
	var best *Library
	for i := range in.Known {
		l := &in.Known[i]
		if Clean(l.MountPoint) == c && (best == nil || l.LastSeen > best.LastSeen) {
			best = l
		}
	}
	if best != nil {
		return best
	}
	for _, p := range in.Providers {
		if Clean(p.MountPoint) != c {
			continue
		}
		for _, pol := range in.Policies {
			if sameNamespace(pol.URLNamespace, p.URLNamespace) {
				l := pol
				l.MountPoint = p.MountPoint
				if l.WebURL == "" {
					l.WebURL = p.WebURL
				}
				return &l
			}
		}
	}
	return nil
}

const hiddenSystem = 0x2 | 0x4 // FILE_ATTRIBUTE_HIDDEN | FILE_ATTRIBUTE_SYSTEM

// webURL accepts only https:// site addresses. These values come from the
// user's registry and are later opened with the shell, so anything else
// (file paths, other protocols) is dropped.
func webURL(u string) string {
	u = strings.TrimSpace(u)
	if strings.HasPrefix(strings.ToLower(u), "https://") && !strings.ContainsAny(u, " \t\r\n\"") {
		return u
	}
	return ""
}

func onlyDesktopIni(entries []fsutil.Entry) bool {
	for _, e := range entries {
		if !strings.EqualFold(e.Name, "desktop.ini") {
			return false
		}
	}
	return true
}

func isVolumeRoot(c string) bool {
	return filepath.Dir(c) == c || len(strings.Trim(c, `\`)) <= 2
}

func cleanAll(in []string) []string {
	var out []string
	for _, p := range in {
		if c := Clean(p); c != "" {
			out = append(out, c)
		}
	}
	return out
}
