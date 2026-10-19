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
func Accounts(h Hive) []Account { return accounts(h, nil) }

func accounts(h Hive, r *readErrs) []Account {
	var out []Account
	for _, key := range h.subKeys(accountsPath, r) {
		base := accountsPath + `\` + key
		a := Account{
			Key:         key,
			UserEmail:   h.str(base, "UserEmail", r),
			UserFolder:  h.str(base, "UserFolder", r),
			DisplayName: h.str(base, "DisplayName", r),
			TenantID:    h.str(base, "ConfiguredTenantId", r),
		}
		for _, tenant := range h.subKeys(base+`\Tenants`, r) {
			names, _ := h.values(base+`\Tenants\`+tenant, r)
			a.SyncedFolders = append(a.SyncedFolders, names...)
		}
		_, cached := h.values(base+`\ScopeIdToMountPointPathCache`, r)
		a.SyncedFolders = dedupe(append(a.SyncedFolders, cached...))
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

func Providers(h Hive) []Provider { return providers(h, nil) }

func providers(h Hive, r *readErrs) []Provider {
	var out []Provider
	for _, key := range h.subKeys(providersPath, r) {
		p := providersPath + `\` + key
		out = append(out, Provider{
			Key:          key,
			MountPoint:   h.str(p, "MountPoint", r),
			URLNamespace: h.str(p, "UrlNamespace", r),
			WebURL:       h.str(p, "WebUrl", r),
			LibraryType:  h.str(p, "LibraryType", r),
		})
	}
	return out
}

// RegisteredSyncRoots returns every folder registered with the Windows Cloud
// Files API by OneDrive (for all users), from HKLM SyncRootManager.
func RegisteredSyncRoots() []string { return registeredSyncRoots(nil) }

func registeredSyncRoots(r *readErrs) []string {
	lm := Hive{Root: registry.LOCAL_MACHINE}
	var out []string
	for _, n := range lm.subKeys(syncRootPath, r) {
		if !strings.HasPrefix(strings.ToLower(n), "onedrive!") {
			continue
		}
		_, vals := lm.values(syncRootPath+`\`+n+`\UserSyncRoots`, r)
		out = append(out, vals...)
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
