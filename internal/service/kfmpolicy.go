package service

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/sys/windows/registry"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/elog"
	"github.com/denismcapple/OneDriveHelper/internal/kfm"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

// kfmEnv is the outside world of manageKFM (replaced in tests).
type kfmEnv struct {
	shared  func() []string // why this is a shared host (empty = workstation)
	tenants func() []string // tenants of work accounts in the signed-in users' OneDrive
	apply   func(tenant string) (bool, error)
	remove  func() (bool, error)
}

var kfmDefault = kfmEnv{shared: winapi.SharedHostReasons, tenants: signedInTenants, apply: kfm.Apply, remove: kfm.Remove}

// manageKFM keeps OneDrive's KFMSilentOptIn policy in line with the folder
// redirection migration setting: set on workstations while it is on, removed
// (only if OneDrive Helper set it) when it is off or on a shared host.
// last is the previous outcome, so each one is logged once.
func (h *handler) manageKFM(cfg config.Config, env kfmEnv) {
	outcome := func(s string, id uint32, warn bool, msg string) {
		if s == h.kfmLast {
			return
		}
		h.kfmLast = s
		h.log.Printf("%s", msg)
		if warn {
			h.ev.Warning(id, msg)
		} else {
			h.ev.Info(id, msg)
		}
	}
	why := env.shared()
	if !cfg.FolderRedirectionMigration || len(why) > 0 {
		changed, err := env.remove()
		switch {
		case changed:
			msg := "Removed OneDrive's KFMSilentOptIn policy that OneDrive Helper had set (the folder redirection migration is off"
			if len(why) > 0 {
				msg += ", or this is not a workstation: " + strings.Join(why, ", ")
			}
			outcome("removed", elog.KFMPolicyRemoved, false, msg+").")
		case err != nil:
			h.log.Printf("remove KFMSilentOptIn: %v", err)
		}
		h.kfmLast = ""
		return
	}
	tenant := strings.TrimSpace(cfg.KFMTenantID)
	if tenant == "" {
		ts := env.tenants()
		switch len(ts) {
		case 0:
			outcome("no-tenant", elog.KFMPolicyNotSet, false, "OneDrive's KFMSilentOptIn policy was not set yet: no work account is signed in to OneDrive on this PC (or set KFMTenantID).")
			return
		case 1:
			tenant = ts[0]
		default:
			outcome("tenants", elog.KFMPolicyNotSet, true, fmt.Sprintf("OneDrive's KFMSilentOptIn policy was not set: users from several tenants (%s) are signed in to OneDrive here. Set KFMTenantID to choose one.", strings.Join(ts, ", ")))
			return
		}
	}
	changed, err := env.apply(tenant)
	switch {
	case errors.Is(err, kfm.ErrOtherPolicy):
		outcome("other", elog.KFMPolicyNotSet, false, "OneDrive's KFMSilentOptIn policy is already set by another policy (GPO, Intune or a script); OneDrive Helper leaves it alone.")
	case err != nil:
		outcome("error:"+err.Error(), elog.KFMPolicyNotSet, true, "OneDrive's KFMSilentOptIn policy could not be set: "+err.Error())
	case changed:
		outcome("set:"+tenant, elog.KFMPolicySet, false, "Set OneDrive's KFMSilentOptIn policy for tenant "+tenant+" (folder redirection migration). OneDrive backs up Desktop, Documents and Pictures for users whose folders are in their profile.")
	default:
		h.kfmLast = "set:" + tenant
	}
}

// signedInTenants lists the tenants of the work accounts in the OneDrive
// settings of every user whose registry is loaded (signed in), sorted.
func signedInTenants() []string {
	k, err := registry.OpenKey(registry.USERS, "", registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil
	}
	sids, _ := k.ReadSubKeyNames(-1)
	k.Close()
	seen := map[string]bool{}
	for _, sid := range sids {
		l := strings.ToLower(sid)
		if strings.HasSuffix(l, "_classes") || !(strings.HasPrefix(l, "s-1-5-21-") || strings.HasPrefix(l, "s-1-12-1-")) {
			continue
		}
		for _, a := range onedrive.Accounts(onedrive.UserHive(sid)) {
			if t := strings.ToLower(strings.Trim(a.TenantID, "{} ")); a.Business() && a.Configured() && kfm.ValidTenant(t) {
				seen[t] = true
			}
		}
	}
	var out []string
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
