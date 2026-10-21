package onedrive

import (
	"net/url"
	"strings"
)

// identity is everything known about the library an orphaned folder held.
type identity struct {
	keys       map[string]bool // site ID + list ID (a list ID alone is not unique: default libraries share one)
	namespaces []string        // SharePoint URL of the library
	tenants    map[string]bool
	hosts      map[string]bool // SharePoint host, e.g. contoso.sharepoint.com
}

func (id identity) empty() bool { return len(id.tenants) == 0 && len(id.hosts) == 0 }

// libKey identifies a library by its site and list IDs ("" if either is missing).
func libKey(l Library) string {
	s, li := normGUID(l.SiteID), normGUID(l.ListID)
	if !isGUID(s) || !isGUID(li) {
		return ""
	}
	return s + "/" + li
}

func normGUID(s string) string {
	return strings.ToLower(strings.Trim(strings.ReplaceAll(strings.TrimSpace(s), "-", ""), "{}"))
}

// spHost returns the SharePoint host of an https URL, with OneDrive's "-my"
// suffix removed, so contoso-my.sharepoint.com and contoso.sharepoint.com match.
func spHost(u string) string {
	p, err := url.Parse(strings.TrimSpace(u))
	if err != nil || !strings.EqualFold(p.Scheme, "https") || p.Host == "" {
		return ""
	}
	h := strings.ToLower(p.Hostname())
	if first, rest, ok := strings.Cut(h, "."); ok {
		h = strings.TrimSuffix(first, "-my") + "." + rest
	}
	return h
}

// identify collects what the cache, OneDrive's (stale) provider entries and
// its leftover ClientPolicy files say about the library that was at c.
func (in Inputs) identify(c string) identity {
	id := identity{keys: map[string]bool{}, tenants: map[string]bool{}, hosts: map[string]bool{}}
	add := func(l Library) {
		if k := libKey(l); k != "" {
			id.keys[k] = true
		}
		if t := normGUID(l.TenantID); t != "" {
			id.tenants[t] = true
		}
		for _, u := range []string{l.WebURL, l.URLNamespace} {
			if h := spHost(u); h != "" {
				id.hosts[h] = true
			}
		}
		if l.URLNamespace != "" {
			id.namespaces = append(id.namespaces, l.URLNamespace)
		}
	}
	for _, l := range in.Known {
		if Clean(l.MountPoint) == c {
			add(l)
		}
	}
	for _, p := range in.Providers {
		if Clean(p.MountPoint) != c {
			continue
		}
		add(Library{WebURL: p.WebURL, URLNamespace: p.URLNamespace})
		for _, pol := range in.Policies {
			if sameNamespace(pol.URLNamespace, p.URLNamespace) {
				add(pol)
			}
		}
	}
	if id.empty() && len(id.keys) == 0 && len(id.namespaces) == 0 {
		for _, l := range in.byFolderName(c) {
			add(l)
		}
	}
	return id
}

// byFolderName is the last resort when nothing records which library was at
// c: OneDrive names library folders "<site title> - <library title>", and
// its current settings and leftover ClientPolicy files carry both titles.
// It returns nothing unless exactly one library has that name.
func (in Inputs) byFolderName(c string) []Library {
	name := c[strings.LastIndex(c, `\`)+1:] // c is Clean (lower case)
	var found []Library
	ids := map[string]bool{}
	for _, libs := range [][]Library{in.Current, in.Policies} {
		for _, l := range libs {
			if l.SiteTitle == "" || l.ListTitle == "" || Clean(l.MountPoint) == c ||
				strings.ToLower(l.SiteTitle+" - "+l.ListTitle) != name {
				continue
			}
			key := libKey(l)
			if key == "" {
				key = strings.ToLower(strings.TrimRight(l.URLNamespace, "/"))
			}
			if key == "" {
				continue
			}
			ids[key] = true
			found = append(found, l)
		}
	}
	if len(ids) != 1 {
		return nil // unknown, or ambiguous (e.g. same site and library name in two organisations)
	}
	return found
}

// classify says what became of the library that was at c (Clean), given the
// active paths. A library counts as moved only if the same library (same
// site and list IDs, or same SharePoint URL) is synced to a folder OneDrive
// is actively syncing now.
func (in Inputs) classify(c string, active []string) (Kind, string) {
	id := in.identify(c)
	elsewhere := func(p string) bool {
		cp := Clean(p)
		if cp == "" || within(cp, c) || within(c, cp) {
			return false
		}
		for _, a := range active {
			if within(cp, a) {
				return true
			}
		}
		return false
	}
	for _, libs := range [][]Library{in.Current, in.Known} {
		for _, l := range libs {
			if k := libKey(l); k != "" && id.keys[k] && elsewhere(l.MountPoint) {
				return KindMoved, l.MountPoint
			}
		}
	}
	for _, p := range in.Providers {
		if t := strings.ToLower(p.LibraryType); t == "personal" || t == "mysite" || !elsewhere(p.MountPoint) {
			continue
		}
		for _, ns := range id.namespaces {
			if sameNamespace(ns, p.URLNamespace) {
				return KindMoved, p.MountPoint
			}
		}
	}
	return in.connected(id, active), ""
}

// connected compares the library's organisation with the signed-in accounts:
// by tenant ID where both sides have one, else by SharePoint host.
func (in Inputs) connected(id identity, active []string) Kind {
	if id.empty() {
		return KindUnknown
	}
	tenants, hosts, business := map[string]bool{}, map[string]bool{}, false
	for _, a := range in.Accounts {
		if a.Configured() && a.Business() {
			business = true
			if t := normGUID(a.TenantID); t != "" {
				tenants[t] = true
			}
		}
	}
	isActive := func(p string) bool {
		for _, a := range active {
			if c := Clean(p); c != "" && within(c, a) {
				return true
			}
		}
		return false
	}
	for _, l := range in.Current {
		if h := spHost(l.WebURL); h != "" && isActive(l.MountPoint) {
			hosts[h] = true
		}
	}
	for _, p := range in.Providers {
		if !isActive(p.MountPoint) {
			continue
		}
		for _, u := range []string{p.URLNamespace, p.WebURL} {
			if h := spHost(u); h != "" {
				hosts[h] = true
			}
		}
	}
	match := func(have, want map[string]bool) bool {
		for k := range have {
			if want[k] {
				return true
			}
		}
		return false
	}
	if match(id.tenants, tenants) || match(id.hosts, hosts) {
		return KindStopped
	}
	comparable := (len(id.tenants) > 0 && len(tenants) > 0) || (len(id.hosts) > 0 && len(hosts) > 0)
	if comparable || !business {
		return KindDisconnected
	}
	return KindUnknown
}
