package elog

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// A source registered by an older version (EventCreate.exe, CustomSource=1)
// is re-pointed at our own exe.
func TestInstallRepointsOldSource(t *testing.T) {
	const path = `Software\OneDriveHelperTest\EventSource`
	t.Cleanup(func() {
		_ = registry.DeleteKey(registry.CURRENT_USER, path)
		_ = registry.DeleteKey(registry.CURRENT_USER, `Software\OneDriveHelperTest`)
	})
	k, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	_ = k.SetExpandStringValue("EventMessageFile", `%SystemRoot%\System32\EventCreate.exe`)
	_ = k.SetDWordValue("CustomSource", 1)
	k.Close()

	exe := `C:\Program Files\OneDriveHelper\OneDriveHelper.exe`
	for i := 0; i < 2; i++ { // and again, as on every upgrade
		if err := install(registry.CURRENT_USER, path, exe); err != nil {
			t.Fatal(err)
		}
	}
	k, err = registry.OpenKey(registry.CURRENT_USER, path, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	if v, typ, err := k.GetStringValue("EventMessageFile"); err != nil || v != exe || typ != registry.EXPAND_SZ {
		t.Errorf("EventMessageFile = %q (type %d, %v)", v, typ, err)
	}
	if v, _, err := k.GetIntegerValue("TypesSupported"); err != nil || v != 7 {
		t.Errorf("TypesSupported = %d (%v)", v, err)
	}
	if _, _, err := k.GetIntegerValue("CustomSource"); !errors.Is(err, registry.ErrNotExist) {
		t.Errorf("CustomSource should be removed: %v", err)
	}
}
