// Package folders finds a user's shell folders (Desktop, Documents, ...)
// that Folder Redirection pointed away from their profile, tells whether a
// Folder Redirection policy still applies to the user, and points the
// folders back to their default place in the profile.
package folders

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	userShellFolders = `Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders`
	shellFolders     = `Software\Microsoft\Windows\CurrentVersion\Explorer\Shell Folders`
)

// known is a folder Folder Redirection can redirect, as Windows stores it.
type known struct {
	value string // value name under User Shell Folders
	name  string // what users call it
	id    string // KNOWNFOLDERID ("" = only a registry alias of another folder)
	rel   string // default location under the profile
}

// knownFolders are the folders Folder Redirection can redirect, plus the
// "local" aliases Windows keeps for some of them (they follow their folder).
var knownFolders = []known{
	{"Desktop", "Desktop", "{B4BFCC3A-DB2C-424C-B029-7FE99A87C641}", `Desktop`},
	{"Personal", "Documents", "{FDD39AD0-238F-46AF-ADB4-6C85480369C7}", `Documents`},
	{"My Pictures", "Pictures", "{33E28130-4E1E-4676-835A-98395C3BC3BB}", `Pictures`},
	{"My Music", "Music", "{4BD8D571-6D19-48D3-BE97-422220080E43}", `Music`},
	{"My Video", "Videos", "{18989B1D-99B5-455B-841C-AB7C74E4DDFC}", `Videos`},
	{"{374DE290-123F-4565-9164-39C4925E467B}", "Downloads", "{374DE290-123F-4565-9164-39C4925E467B}", `Downloads`},
	{"Favorites", "Favorites", "{1777F761-68AD-4D8A-87BD-30B759FA33DD}", `Favorites`},
	{"{56784854-C6CB-462B-8169-88E350ACB882}", "Contacts", "{56784854-C6CB-462B-8169-88E350ACB882}", `Contacts`},
	{"{BFB9D5E0-C6A9-404C-B2B2-AE6DB6AF4968}", "Links", "{BFB9D5E0-C6A9-404C-B2B2-AE6DB6AF4968}", `Links`},
	{"{7D1D3A04-DEBB-4115-95CF-2F29DA2920DA}", "Searches", "{7D1D3A04-DEBB-4115-95CF-2F29DA2920DA}", `Searches`},
	{"{4C5C32FF-BB9D-43B0-B5B4-2D72E54EAAA4}", "Saved Games", "{4C5C32FF-BB9D-43B0-B5B4-2D72E54EAAA4}", `Saved Games`},
	{"AppData", "AppData (Roaming)", "{3EB685DB-65F9-4CF6-A03A-E3EF65729F3D}", `AppData\Roaming`},
	{"Start Menu", "Start Menu", "{625B53C3-AB48-4EC1-BA1F-A1EF4146FC19}", `AppData\Roaming\Microsoft\Windows\Start Menu`},
	{"Programs", "Start Menu programs", "{A77F5D77-2E2B-44C3-A6A2-ABA601054A51}", `AppData\Roaming\Microsoft\Windows\Start Menu\Programs`},
	{"Startup", "Startup", "{B97D20BB-F46A-4C97-BA10-5E3608430854}", `AppData\Roaming\Microsoft\Windows\Start Menu\Programs\Startup`},
	// Aliases: Windows keeps these next to Documents, Pictures, ... and Folder
	// Redirection points them at the same place.
	{"{F42EE2D3-909F-4907-8871-4C22FC0BF756}", "Documents", "", `Documents`},
	{"{0DDD015D-B06C-45D5-8C4C-F59713854639}", "Pictures", "", `Pictures`},
	{"{7D83EE9B-2244-4E70-B1F5-5393042AF1E4}", "Downloads", "", `Downloads`},
	{"{A0C69A99-21C8-4671-8703-7934162FCF1D}", "Music", "", `Music`},
	{"{35286A68-3C57-41A1-BBB1-0EAE73D76C95}", "Videos", "", `Videos`},
}

// Folder is one shell folder that points outside the user's profile.
type Folder struct {
	Value   string // value name under User Shell Folders
	Name    string // e.g. "Desktop"
	ID      string // KNOWNFOLDERID ("" = registry alias, or not a folder we know)
	Rel     string // default location under the profile ("" = unknown: left alone)
	Current string // where it points now, e.g. \\fs01\users\jsmith\Desktop
}

// Known reports whether the tool knows where the folder belongs (and so can
// point it back).
func (f Folder) Known() bool { return f.Rel != "" }

// Default is the folder's default location in profile.
func (f Folder) Default(profile string) string { return filepath.Join(profile, f.Rel) }

// Hive is where a user's settings are read: HKCU for the signed-in user,
// HKEY_USERS\<SID> for another user (the elevated approval step).
type Hive struct {
	Root   registry.Key
	Prefix string
}

// CurrentUser is the signed-in user's hive.
var CurrentUser = Hive{Root: registry.CURRENT_USER}

// UserHive is another user's loaded hive.
func UserHive(sid string) Hive { return Hive{Root: registry.USERS, Prefix: sid + `\`} }

// Redirected lists the shell folders that point outside profile, folders
// the tool knows first (in the order users think of them), then any others.
func Redirected(h Hive, profile string) ([]Folder, error) {
	k, err := registry.OpenKey(h.Root, h.Prefix+userShellFolders, registry.QUERY_VALUE)
	if err != nil {
		return nil, err
	}
	defer k.Close()
	names, err := k.ReadValueNames(-1)
	if err != nil {
		return nil, err
	}
	byValue := map[string]known{}
	for _, f := range knownFolders {
		byValue[strings.ToLower(f.value)] = f
	}
	var out, unknown []Folder
	for _, n := range names {
		raw, _, err := k.GetStringValue(n)
		if err != nil || strings.TrimSpace(raw) == "" {
			continue
		}
		cur := expand(raw, profile)
		if inside(cur, profile) {
			continue
		}
		if kf, ok := byValue[strings.ToLower(n)]; ok {
			out = append(out, Folder{Value: n, Name: kf.name, ID: kf.id, Rel: kf.rel, Current: cur})
		} else {
			unknown = append(unknown, Folder{Value: n, Name: n, Current: cur})
		}
	}
	order := map[string]int{}
	for i, f := range knownFolders {
		order[strings.ToLower(f.value)] = i
	}
	sort.SliceStable(out, func(i, j int) bool {
		return order[strings.ToLower(out[i].Value)] < order[strings.ToLower(out[j].Value)]
	})
	sort.Slice(unknown, func(i, j int) bool { return unknown[i].Value < unknown[j].Value })
	return append(out, unknown...), nil
}

// expand resolves %USERPROFILE% (for any user) and other variables.
func expand(raw, profile string) string {
	s := raw
	if i := strings.Index(strings.ToLower(s), "%userprofile%"); i >= 0 {
		s = s[:i] + profile + s[i+len("%userprofile%"):]
	}
	if x, err := registry.ExpandString(s); err == nil {
		s = x
	}
	return filepath.Clean(s)
}

func inside(path, profile string) bool {
	p, prof := strings.ToLower(filepath.Clean(path)), strings.ToLower(filepath.Clean(profile))
	return prof != "." && (p == prof || strings.HasPrefix(p, strings.TrimSuffix(prof, `\`)+`\`))
}

// (whether a Folder Redirection policy still applies: see policy.go)

// --- pointing folders back ---

var procSHSetKnownFolderPath = windows.NewLazySystemDLL("shell32.dll").NewProc("SHSetKnownFolderPath")

// Repoint points the signed-in user's folder back to its default location
// in profile, creating it if needed. Files are not moved. It must run as the
// user.
func Repoint(f Folder, profile string) error {
	if !f.Known() {
		return fmt.Errorf("%s: its default location is not known", f.Name)
	}
	dst := f.Default(profile)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	if f.ID != "" {
		id, err := windows.GUIDFromString(f.ID)
		if err != nil {
			return err
		}
		p, err := windows.UTF16PtrFromString(dst)
		if err != nil {
			return err
		}
		// Flags 0: Windows stores the path as %USERPROFILE%\... and tells Explorer.
		if r, _, _ := procSHSetKnownFolderPath.Call(uintptr(unsafe.Pointer(&id)), 0, 0, uintptr(unsafe.Pointer(p))); int32(r) < 0 {
			return fmt.Errorf("set %s to %s: %w", f.Name, dst, windows.Errno(r))
		}
		return nil
	}
	return setAlias(f.Value, `%USERPROFILE%\`+f.Rel, dst)
}

// setAlias points a registry-only alias at the default location.
func setAlias(value, unexpanded, expanded string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, userShellFolders, registry.SET_VALUE)
	if err != nil {
		return err
	}
	err = k.SetExpandStringValue(value, unexpanded)
	k.Close()
	if err != nil {
		return err
	}
	if sk, err := registry.OpenKey(registry.CURRENT_USER, shellFolders, registry.SET_VALUE); err == nil {
		_ = sk.SetStringValue(value, expanded)
		sk.Close()
	}
	return nil
}

// --- Explorer ---

var procTerminateProcess = windows.NewLazySystemDLL("kernel32.dll").NewProc("TerminateProcess")

// RestartExplorer closes the user's Explorer (taskbar and open windows) and
// makes sure it starts again, so it uses the new folder locations. pids
// lists the explorer.exe processes in the user's session; running reports
// whether one is running there again.
func RestartExplorer(pids []uint32, running func() bool) error {
	for _, pid := range pids {
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
		if err != nil {
			continue
		}
		procTerminateProcess.Call(uintptr(h), 1)
		_, _ = windows.WaitForSingleObject(h, 5000)
		windows.CloseHandle(h)
	}
	// Windows usually starts the shell again by itself; only start it if not.
	for i := 0; i < 10; i++ {
		time.Sleep(500 * time.Millisecond)
		if running() {
			return nil
		}
	}
	return startExplorer()
}

var startExplorer = func() error {
	win, err := windows.GetWindowsDirectory()
	if err != nil {
		return err
	}
	cmd := exec.Command(filepath.Join(win, "explorer.exe"))
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// ErrNoFolders means nothing points outside the profile.
var ErrNoFolders = errors.New("no folders are redirected")
