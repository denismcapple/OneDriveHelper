package onedrive

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows/registry"
)

// Library describes a synced SharePoint library with the IDs needed to
// re-sync it via an odopen:// link.
type Library struct {
	ScopeID    string
	Account    string
	UserEmail  string
	SiteTitle  string
	ListTitle  string
	WebURL     string
	TenantID   string
	SiteID     string
	WebID      string
	ListID     string
	MountPoint string
	LastSeen   string
	// URLNamespace is only set for libraries recovered from ClientPolicy files.
	URLNamespace string
}

// ParseLibraryScope parses a "libraryScope = ..." line from
// %LOCALAPPDATA%\Microsoft\OneDrive\settings\BusinessN\<cid>.ini, e.g.
//
//	libraryScope = 2 <scopeId> 5 "Internal" "Documents" 4 "https://x.sharepoint.com/sites/internal"
//	  "<tenantId>" <siteId> <webId> <listId> <timestamp> "C:\Users\x\Org\Internal - Documents" ...
func ParseLibraryScope(line string) (Library, bool) {
	k, v, ok := strings.Cut(line, "=")
	if !ok || strings.TrimSpace(k) != "libraryScope" {
		return Library{}, false
	}
	f := tokenize(v)
	if len(f) < 13 {
		return Library{}, false
	}
	l := Library{
		ScopeID:    f[1],
		SiteTitle:  f[3],
		ListTitle:  f[4],
		WebURL:     f[6],
		TenantID:   f[7],
		SiteID:     f[8],
		WebID:      f[9],
		ListID:     f[10],
		MountPoint: f[12],
	}
	// Scope 0 / "MySite" is the user's own OneDrive, not a SharePoint library.
	if l.SiteTitle == "MySite" || f[5] == "2" || !isGUID(l.SiteID) || !isGUID(l.ListID) || l.MountPoint == "" {
		return Library{}, false
	}
	return l, true
}

// tokenize splits on whitespace, honouring double-quoted fields (which may be empty).
func tokenize(s string) []string {
	var out []string
	var cur strings.Builder
	inQ, have := false, false
	for _, r := range s {
		switch {
		case r == '"':
			inQ = !inQ
			have = true
		case (r == ' ' || r == '\t') && !inQ:
			if have {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteRune(r)
			have = true
		}
	}
	if have {
		out = append(out, cur.String())
	}
	return out
}

func isGUID(s string) bool {
	s = strings.Trim(strings.ReplaceAll(s, "-", ""), "{}")
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

// FormatGUID returns s as {XXXXXXXX-XXXX-XXXX-XXXX-XXXXXXXXXXXX}.
func FormatGUID(s string) string {
	h := strings.ToUpper(strings.Trim(strings.ReplaceAll(s, "-", ""), "{}"))
	if len(h) != 32 {
		return s
	}
	return fmt.Sprintf("{%s-%s-%s-%s-%s}", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// ResyncURL builds the odopen:// link SharePoint's "Sync" button uses.
func (l Library) ResyncURL() string {
	q := func(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }
	parts := []string{
		"userEmail=" + q(l.UserEmail),
		"isSiteAdmin=0",
		"siteId=" + q(FormatGUID(l.SiteID)),
		"webId=" + q(FormatGUID(l.WebID)),
		"webTitle=" + q(l.SiteTitle),
		"webUrl=" + q(l.WebURL),
		"onPrem=0",
		"libraryType=4",
		"listId=" + q(FormatGUID(l.ListID)),
		"listTitle=" + q(l.ListTitle),
		"scope=OPENLIST",
	}
	return "odopen://sync/?" + strings.Join(parts, "&")
}

// ReadLibraries parses the OneDrive settings .ini files for every business account.
func ReadLibraries(localAppData string, accts []Account) []Library {
	var out []Library
	for _, a := range accts {
		if !a.Business() {
			continue
		}
		files, _ := filepath.Glob(filepath.Join(localAppData, `Microsoft\OneDrive\settings`, a.Key, "*.ini"))
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			for _, line := range strings.Split(decodeText(b), "\n") {
				if l, ok := ParseLibraryScope(strings.TrimRight(line, "\r")); ok {
					l.Account, l.UserEmail = a.Key, a.UserEmail
					out = append(out, l)
				}
			}
		}
	}
	return out
}

// ReadClientPolicies reads the ClientPolicy_<listId>_<siteId>.ini files that
// OneDrive leaves behind in settings\BusinessN. They usually survive after a
// library is dropped, so they are a fallback source for re-sync IDs. The
// returned libraries have URLNamespace set but no MountPoint.
func ReadClientPolicies(localAppData string, accts []Account) []Library {
	var out []Library
	for _, a := range accts {
		if !a.Business() {
			continue
		}
		files, _ := filepath.Glob(filepath.Join(localAppData, `Microsoft\OneDrive\settings`, a.Key, "ClientPolicy_*_*.ini"))
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			if l, ok := parseClientPolicy(filepath.Base(f), decodeText(b)); ok {
				l.Account, l.UserEmail = a.Key, a.UserEmail
				out = append(out, l)
			}
		}
	}
	return out
}

func parseClientPolicy(name, text string) (Library, bool) {
	kv := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), "="); ok {
			kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	l := Library{
		SiteTitle: kv["SiteTitle"], ListTitle: kv["LibraryTitle"],
		SiteID: kv["SiteID"], WebID: kv["WebID"], URLNamespace: kv["DavUrlNamespace"],
	}
	// ListID: from ".../_api/SPFileSync/sync/<listId>/RootFolder", else the file name.
	if _, rest, ok := strings.Cut(kv["SkyDriveURI"], "/sync/"); ok {
		l.ListID, _, _ = strings.Cut(rest, "/")
	}
	if !isGUID(l.ListID) {
		parts := strings.Split(strings.TrimSuffix(name, ".ini"), "_")
		if len(parts) == 3 {
			l.ListID = parts[1]
		}
	}
	if !isGUID(l.SiteID) || !isGUID(l.WebID) || !isGUID(l.ListID) || l.URLNamespace == "" {
		return Library{}, false
	}
	l.ScopeID = "policy:" + strings.ToLower(strings.Trim(l.ListID, "{}")+"_"+strings.Trim(l.SiteID, "{}"))
	return l, true
}

func sameNamespace(a, b string) bool {
	n := func(s string) string { return strings.ToLower(strings.TrimRight(strings.TrimSpace(s), "/")) }
	return a != "" && n(a) == n(b)
}

// decodeText decodes UTF-16LE (with or without BOM) or UTF-8 text.
func decodeText(b []byte) string {
	if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE {
		b = b[2:]
	} else if len(b) < 2 || b[1] != 0 {
		return strings.TrimPrefix(string(b), "\ufeff")
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return string(utf16.Decode(u))
}

const cachePath = `Software\OneDriveHelper\KnownLibraries`

// SaveLibraries records library IDs in the user's hive so a library can still
// be re-synced after OneDrive has forgotten it. Unchanged entries are only
// rewritten once a day to keep registry churn low.
func SaveLibraries(h Hive, libs []Library) error {
	existing := map[string]Library{}
	for _, l := range LoadLibraries(h) {
		existing[l.ScopeID] = l
	}
	now := time.Now().UTC()
	for _, l := range libs {
		if old, ok := existing[l.ScopeID]; ok {
			seen, _ := time.Parse(time.RFC3339, old.LastSeen)
			old.LastSeen, l.LastSeen = "", ""
			if old == l && now.Sub(seen) < 24*time.Hour {
				continue
			}
		}
		k, err := h.Create(cachePath + `\` + strings.ReplaceAll(l.ScopeID, `\`, "_"))
		if err != nil {
			return err
		}
		for name, val := range map[string]string{
			"ScopeID": l.ScopeID, "Account": l.Account, "UserEmail": l.UserEmail,
			"SiteTitle": l.SiteTitle, "ListTitle": l.ListTitle, "WebURL": l.WebURL,
			"TenantID": l.TenantID, "SiteID": l.SiteID, "WebID": l.WebID, "ListID": l.ListID,
			"MountPoint": l.MountPoint, "LastSeen": now.Format(time.RFC3339),
		} {
			_ = k.SetStringValue(name, val)
		}
		k.Close()
	}
	return nil
}

// LoadLibraries returns the cached library records.
func LoadLibraries(h Hive) []Library {
	var out []Library
	for _, key := range h.SubKeys(cachePath) {
		k, err := h.Open(cachePath+`\`+key, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		g := func(n string) string { s, _, _ := k.GetStringValue(n); return s }
		out = append(out, Library{
			ScopeID: g("ScopeID"), Account: g("Account"), UserEmail: g("UserEmail"),
			SiteTitle: g("SiteTitle"), ListTitle: g("ListTitle"), WebURL: g("WebURL"),
			TenantID: g("TenantID"), SiteID: g("SiteID"), WebID: g("WebID"), ListID: g("ListID"),
			MountPoint: g("MountPoint"), LastSeen: g("LastSeen"),
		})
		k.Close()
	}
	return out
}
