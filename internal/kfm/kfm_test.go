package kfm

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

// useTestKeys points the package at throwaway HKCU keys.
func useTestKeys(t *testing.T) {
	t.Helper()
	base := fmt.Sprintf(`Software\OneDriveHelperTest-%d`, time.Now().UnixNano())
	savedRoot, savedPolicy, savedManaged := root, policyPath, managedPath
	root, policyPath, managedPath = registry.CURRENT_USER, base+`\Policy`, base+`\Managed`
	t.Cleanup(func() {
		for _, p := range []string{policyPath, managedPath, base} {
			_ = registry.DeleteKey(registry.CURRENT_USER, p)
		}
		root, policyPath, managedPath = savedRoot, savedPolicy, savedManaged
	})
}

const tenant = "11111111-2222-3333-4444-555555555555"

func notify(t *testing.T) (uint64, error) {
	k, err := registry.OpenKey(root, policyPath, registry.QUERY_VALUE)
	if err != nil {
		return 0, err
	}
	defer k.Close()
	n, _, err := k.GetIntegerValue(ValueNotify)
	return n, err
}

func TestApplyAndRemove(t *testing.T) {
	useTestKeys(t)
	if _, err := Apply("not a guid"); err == nil {
		t.Error("a bad tenant ID is refused")
	}
	if changed, err := Apply("{" + tenant + "}"); err != nil || !changed || Tenant() != tenant {
		t.Fatalf("apply: changed %v err %v tenant %q", changed, err, Tenant())
	}
	if n, err := notify(t); err != nil || n != 1 {
		t.Errorf("users are told their folders are backed up: %d %v", n, err)
	}
	if changed, err := Apply(tenant); err != nil || changed {
		t.Errorf("applying again changes nothing: %v %v", changed, err)
	}
	if changed, err := Remove(); err != nil || !changed || Tenant() != "" {
		t.Fatalf("remove: %v %v %q", changed, err, Tenant())
	}
	if _, err := notify(t); !errors.Is(err, registry.ErrNotExist) {
		t.Errorf("the notification value it wrote is removed too: %v", err)
	}
	if changed, _ := Remove(); changed {
		t.Error("nothing left to remove")
	}
}

func TestNeverOverridesAnotherPolicy(t *testing.T) {
	useTestKeys(t)
	k, _, _ := registry.CreateKey(root, policyPath, registry.ALL_ACCESS)
	_ = k.SetStringValue(ValueOptIn, "99999999-2222-3333-4444-555555555555") // set by the customer's GPO
	_ = k.SetDWordValue(ValueNotify, 0)
	k.Close()
	if _, err := Apply(tenant); !errors.Is(err, ErrOtherPolicy) {
		t.Fatalf("another policy's value must be left alone: %v", err)
	}
	if changed, _ := Remove(); changed || Tenant() != "99999999-2222-3333-4444-555555555555" {
		t.Error("and never removed")
	}
	if n, _ := notify(t); n != 0 {
		t.Error("the customer's notification setting stays")
	}
}

func TestRemoveLeavesAChangedValue(t *testing.T) {
	useTestKeys(t)
	if _, err := Apply(tenant); err != nil {
		t.Fatal(err)
	}
	k, _ := registry.OpenKey(root, policyPath, registry.SET_VALUE)
	_ = k.SetStringValue(ValueOptIn, "99999999-2222-3333-4444-555555555555") // someone changed it since
	k.Close()
	if _, err := Remove(); err != nil {
		t.Fatal(err)
	}
	if Tenant() != "99999999-2222-3333-4444-555555555555" {
		t.Error("a value changed by someone else is not removed")
	}
}
