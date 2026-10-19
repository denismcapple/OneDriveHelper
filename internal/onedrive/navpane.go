package onedrive

import (
	"os"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// OneDrive registers each personal/organisation folder as an Explorer
// navigation-pane item: HKCU\...\Explorer\Desktop\NameSpace\{CLSID} plus
// HKCU\Software\Classes\CLSID\{CLSID}. All of OneDrive's CLSIDs start with
// this prefix. These entries are left behind when a folder goes away.
const (
	navPrefix       = "{04271989-c4d2-"
	navNamespace    = `Software\Microsoft\Windows\CurrentVersion\Explorer\Desktop\NameSpace`
	navHideIcons    = `Software\Microsoft\Windows\CurrentVersion\Explorer\HideDesktopIcons\NewStartPanel`
	navClassesCLSID = `Software\Classes\CLSID\`
	navClassesWow   = `Software\Classes\Wow6432Node\CLSID\`
)

// NavEntry is a OneDrive item in Explorer's navigation pane.
type NavEntry struct {
	CLSID  string
	Name   string
	Target string
}

// NavEntries lists OneDrive's navigation-pane items for the user.
func NavEntries(h Hive) []NavEntry {
	var out []NavEntry
	for _, id := range h.SubKeys(navNamespace) {
		if !strings.HasPrefix(strings.ToLower(id), navPrefix) {
			continue
		}
		out = append(out, NavEntry{
			CLSID:  id,
			Name:   h.String(navNamespace+`\`+id, ""),
			Target: h.String(navClassesCLSID+id+`\Instance\InitPropertyBag`, "TargetFolderPath"),
		})
	}
	return out
}

// StaleNavEntries returns items whose target folder no longer exists and is
// not in use by any OneDrive account.
func StaleNavEntries(h Hive, in Inputs, exists func(string) bool) []NavEntry {
	if exists == nil {
		exists = func(p string) bool { _, err := os.Stat(p); return err == nil }
	}
	if len(in.ReadErrors) > 0 {
		return nil // can't tell which folders are in use
	}
	return staleFrom(NavEntries(h), in.ActivePaths(), exists)
}

func staleFrom(entries []NavEntry, active []string, exists func(string) bool) []NavEntry {
	var out []NavEntry
	for _, e := range entries {
		if e.Target == "" || exists(e.Target) || overlaps(Clean(e.Target), active) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// RemoveNavEntry deletes a navigation-pane item (only OneDrive's own CLSIDs).
func RemoveNavEntry(h Hive, e NavEntry) error {
	if !strings.HasPrefix(strings.ToLower(e.CLSID), navPrefix) {
		return nil
	}
	if k, err := h.Open(navHideIcons, registry.SET_VALUE); err == nil {
		_ = k.DeleteValue(e.CLSID)
		k.Close()
	}
	_ = deleteTree(h, navClassesWow+e.CLSID)
	if err := deleteTree(h, navClassesCLSID+e.CLSID); err != nil && err != registry.ErrNotExist {
		return err
	}
	if err := deleteTree(h, navNamespace+`\`+e.CLSID); err != nil && err != registry.ErrNotExist {
		return err
	}
	return nil
}

func deleteTree(h Hive, path string) error {
	for _, sub := range h.SubKeys(path) {
		_ = deleteTree(h, path+`\`+sub)
	}
	return registry.DeleteKey(h.Root, h.Prefix+path)
}

var procSHChangeNotify = windows.NewLazySystemDLL("shell32.dll").NewProc("SHChangeNotify")

// RefreshExplorer asks Explorer to reload namespace items (navigation pane).
func RefreshExplorer() {
	procSHChangeNotify.Call(0x08000000, 0, 0, 0) // SHCNE_ASSOCCHANGED, SHCNF_IDLIST
}
