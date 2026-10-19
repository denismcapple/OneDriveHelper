package onedrive

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// Hive addresses a user's registry hive, either via HKCU (when running as the
// user) or via HKEY_USERS\<SID> (when running elevated / as another account).
type Hive struct {
	Root   registry.Key
	Prefix string
}

func CurrentUserHive() Hive { return Hive{Root: registry.CURRENT_USER} }

func UserHive(sid string) Hive { return Hive{Root: registry.USERS, Prefix: sid + `\`} }

func (h Hive) Open(path string, access uint32) (registry.Key, error) {
	return registry.OpenKey(h.Root, h.Prefix+path, access)
}

func (h Hive) Create(path string) (registry.Key, error) {
	k, _, err := registry.CreateKey(h.Root, h.Prefix+path, registry.ALL_ACCESS)
	return k, err
}

func (h Hive) String(path, name string) string {
	k, err := h.Open(path, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	s, _, _ := k.GetStringValue(name)
	return s
}

func (h Hive) SubKeys(path string) []string {
	k, err := h.Open(path, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil
	}
	defer k.Close()
	names, _ := k.ReadSubKeyNames(-1)
	return names
}

// ProfileDir returns the profile directory of a user SID from the ProfileList.
func ProfileDir(sid string) string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList\`+sid, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	s, _, _ := k.GetStringValue("ProfileImagePath")
	if x, err := registry.ExpandString(s); err == nil {
		s = x
	}
	return s
}

// Clean normalises a path for case-insensitive comparison.
func Clean(p string) string {
	if p == "" {
		return ""
	}
	return strings.ToLower(filepath.Clean(p))
}

// within reports whether child is equal to or below parent (both Clean()ed).
func within(child, parent string) bool {
	if child == parent {
		return true
	}
	return strings.HasPrefix(child, strings.TrimSuffix(parent, `\`)+`\`)
}
