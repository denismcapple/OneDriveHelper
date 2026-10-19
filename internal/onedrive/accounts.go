package onedrive

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

const (
	accountsPath  = `Software\Microsoft\OneDrive\Accounts`
	providersPath = `Software\SyncEngines\Providers\OneDrive`
	syncRootPath  = `SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\SyncRootManager`
)

// Account is a signed-in (or previously signed-in) OneDrive account.
type Account struct {
	Key         string // Business1, Business2, Personal
	UserEmail   string
	UserFolder  string // the OneDrive personal folder, e.g. C:\Users\x\OneDrive - Org
	DisplayName string // org name, e.g. "Contoso"
	TenantID    string
	// SyncedFolders are the library folders OneDrive is actively syncing,
	// from Accounts\<key>\Tenants\<name> and ScopeIdToMountPointPathCache.
	SyncedFolders []string
}

// Configured reports whether the account is set up on this machine.
func (a Account) Configured() bool { return a.UserFolder != "" }

func (a Account) Business() bool { return strings.HasPrefix(strings.ToLower(a.Key), "business") }

// Accounts reads all OneDrive accounts from the user's hive.
func Accounts(h Hive) []Account {
	var out []Account
	for _, key := range h.SubKeys(accountsPath) {
		base := accountsPath + `\` + key
		a := Account{
			Key:         key,
			UserEmail:   h.String(base, "UserEmail"),
			UserFolder:  h.String(base, "UserFolder"),
			DisplayName: h.String(base, "DisplayName"),
			TenantID:    h.String(base, "ConfiguredTenantId"),
		}
		for _, tenant := range h.SubKeys(base + `\Tenants`) {
			a.SyncedFolders = append(a.SyncedFolders, valueNames(h, base+`\Tenants\`+tenant)...)
		}
		a.SyncedFolders = dedupe(append(a.SyncedFolders, stringValues(h, base+`\ScopeIdToMountPointPathCache`)...))
		out = append(out, a)
	}
	return out
}

// AnyConfigured reports whether at least one account is set up.
func AnyConfigured(accts []Account) bool {
	for _, a := range accts {
		if a.Configured() {
			return true
		}
	}
	return false
}

// Provider is an entry from Software\SyncEngines\Providers\OneDrive. These
// entries are NOT removed when a library stops syncing, so they are used as
// history only, never as proof that a library is active.
type Provider struct {
	Key          string
	MountPoint   string
	URLNamespace string
	WebURL       string
	LibraryType  string
}

func Providers(h Hive) []Provider {
	var out []Provider
	for _, key := range h.SubKeys(providersPath) {
		p := providersPath + `\` + key
		out = append(out, Provider{
			Key:          key,
			MountPoint:   h.String(p, "MountPoint"),
			URLNamespace: h.String(p, "UrlNamespace"),
			WebURL:       h.String(p, "WebUrl"),
			LibraryType:  h.String(p, "LibraryType"),
		})
	}
	return out
}

// RegisteredSyncRoots returns every folder registered with the Windows Cloud
// Files API by OneDrive (for all users), from HKLM SyncRootManager.
func RegisteredSyncRoots() []string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, syncRootPath, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil
	}
	defer k.Close()
	names, _ := k.ReadSubKeyNames(-1)
	var out []string
	for _, n := range names {
		if !strings.HasPrefix(strings.ToLower(n), "onedrive!") {
			continue
		}
		sk, err := registry.OpenKey(registry.LOCAL_MACHINE, syncRootPath+`\`+n+`\UserSyncRoots`, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		vals, _ := sk.ReadValueNames(-1)
		for _, v := range vals {
			if s, _, err := sk.GetStringValue(v); err == nil && s != "" {
				out = append(out, s)
			}
		}
		sk.Close()
	}
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range in {
		if c := Clean(p); c != "" && !seen[c] {
			seen[c] = true
			out = append(out, p)
		}
	}
	return out
}

func valueNames(h Hive, path string) []string {
	k, err := h.Open(path, registry.QUERY_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()
	names, _ := k.ReadValueNames(-1)
	return names
}

func stringValues(h Hive, path string) []string {
	k, err := h.Open(path, registry.QUERY_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()
	names, _ := k.ReadValueNames(-1)
	var out []string
	for _, n := range names {
		if s, _, err := k.GetStringValue(n); err == nil && s != "" {
			out = append(out, s)
		}
	}
	return out
}
