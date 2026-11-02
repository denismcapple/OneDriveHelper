// Package kfm writes and removes the machine-wide OneDrive policy that turns
// on folder backup (Known Folder Move) silently: KFMSilentOptIn. The service
// manages it when the folder redirection migration is on. A value set by
// someone else (GPO, Intune, a script) is never changed or removed.
package kfm

import (
	"errors"
	"regexp"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const (
	ValueOptIn  = "KFMSilentOptIn"
	ValueNotify = "KFMSilentOptInWithNotification"
)

// Where the policy lives, and where the service records what it wrote
// (replaced in tests).
var (
	root        = registry.LOCAL_MACHINE
	policyPath  = `SOFTWARE\Policies\Microsoft\OneDrive`
	managedPath = `SOFTWARE\OneDriveHelper\Managed\OneDrivePolicy`
)

var guid = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidTenant reports whether s looks like a tenant ID (a GUID).
func ValidTenant(s string) bool { return guid.MatchString(strings.Trim(strings.TrimSpace(s), "{}")) }

func norm(s string) string { return strings.ToLower(strings.Trim(strings.TrimSpace(s), "{}")) }

// ErrOtherPolicy means KFMSilentOptIn is already set, to another tenant, by
// someone other than OneDrive Helper.
var ErrOtherPolicy = errors.New("KFMSilentOptIn is already set by another policy")

// Tenant returns the tenant KFMSilentOptIn is set to ("" = not set).
func Tenant() string {
	k, err := registry.OpenKey(root, policyPath, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, _ := k.GetStringValue(ValueOptIn)
	return strings.TrimSpace(v)
}

// Apply sets KFMSilentOptIn to tenant (and KFMSilentOptInWithNotification,
// so users are told their folders are backed up), unless another policy
// already set it. It reports whether anything changed.
func Apply(tenant string) (bool, error) {
	if !ValidTenant(tenant) {
		return false, errors.New("the tenant ID is not a GUID")
	}
	tenant = norm(tenant)
	cur := Tenant()
	mine := managed(ValueOptIn)
	switch {
	case cur != "" && norm(cur) == tenant:
		return false, nil
	case cur != "" && (mine == "" || norm(mine) != norm(cur)):
		return false, ErrOtherPolicy
	}
	k, _, err := registry.CreateKey(root, policyPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return false, err
	}
	defer k.Close()
	if err := setManaged(ValueOptIn, tenant); err != nil {
		return false, err
	}
	if err := k.SetStringValue(ValueOptIn, tenant); err != nil {
		return false, err
	}
	if _, _, err := k.GetIntegerValue(ValueNotify); errors.Is(err, registry.ErrNotExist) {
		if setManaged(ValueNotify, "1") == nil {
			_ = k.SetDWordValue(ValueNotify, 1)
		}
	}
	return true, nil
}

// Remove deletes the values OneDrive Helper wrote, if they still hold what
// it wrote. It reports whether anything changed.
func Remove() (bool, error) {
	mt, mn := managed(ValueOptIn), managed(ValueNotify)
	if mt == "" && mn == "" {
		return false, nil
	}
	changed := false
	if k, err := registry.OpenKey(root, policyPath, registry.QUERY_VALUE|registry.SET_VALUE); err == nil {
		if cur, _, err := k.GetStringValue(ValueOptIn); err == nil && mt != "" && norm(cur) == norm(mt) {
			changed = k.DeleteValue(ValueOptIn) == nil || changed
		}
		if n, _, err := k.GetIntegerValue(ValueNotify); err == nil && mn != "" && n == 1 {
			changed = k.DeleteValue(ValueNotify) == nil || changed
		}
		k.Close()
	}
	return changed, registry.DeleteKey(root, managedPath)
}

func managed(name string) string {
	k, err := registry.OpenKey(root, managedPath, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, _ := k.GetStringValue(name)
	return v
}

func setManaged(name, v string) error {
	k, _, err := registry.CreateKey(root, managedPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(name, v)
}
