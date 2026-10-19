package service

import "testing"

func TestQuotePath(t *testing.T) {
	for in, want := range map[string]string{
		`C:\Program Files\OneDriveHelper\OneDriveHelper.exe`:   `"C:\Program Files\OneDriveHelper\OneDriveHelper.exe"`,
		`C:\Tools\OneDriveHelper.exe`:                          `"C:\Tools\OneDriveHelper.exe"`, // quoted even without spaces
		`"C:\Program Files\OneDriveHelper\OneDriveHelper.exe"`: `"C:\Program Files\OneDriveHelper\OneDriveHelper.exe"`,
	} {
		if got := QuotePath(in); got != want {
			t.Errorf("QuotePath(%s) = %s", in, got)
		}
	}
}
