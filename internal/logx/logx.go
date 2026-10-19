// Package logx provides a small size-rotated file logger.
package logx

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
)

const maxSize = 5 << 20

// Open returns a logger writing to path (rotated to path.1 when larger than 5 MB).
// If the file cannot be opened the logger falls back to stderr.
func Open(path string, alsoStderr bool) *log.Logger {
	var w io.Writer = os.Stderr
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		if fi, err := os.Stat(path); err == nil && fi.Size() > maxSize {
			_ = os.Rename(path, path+".1")
		}
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			w = f
			if alsoStderr {
				w = io.MultiWriter(f, os.Stderr)
			}
		}
	}
	return log.New(w, "", log.LstdFlags)
}

// PrepareProgramDataDir makes sure %ProgramData%\OneDriveHelper is owned by
// Administrators and writable only by SYSTEM/Administrators, so a standard
// user cannot plant links that redirect the service's log writes. A folder a
// user created beforehand is moved aside rather than trusted. Requires admin.
func PrepareProgramDataDir() error {
	dir := ProgramDataDir()
	if fi, err := os.Lstat(dir); err == nil {
		if !fi.IsDir() || fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 || !ownedByAdmins(dir) {
			if err := os.Rename(dir, dir+".untrusted-"+time.Now().Format("20060102-150405")); err != nil {
				return err
			}
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString("O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;BU)")
	if err != nil {
		return err
	}
	owner, _, _ := sd.Owner()
	dacl, _, _ := sd.DACL()
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		owner, nil, dacl, nil)
}

func ownedByAdmins(path string) bool {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false
	}
	o, _, err := sd.Owner()
	return err == nil && o != nil && (o.IsWellKnown(windows.WinLocalSystemSid) || o.IsWellKnown(windows.WinBuiltinAdministratorsSid))
}

// ProgramDataDir returns %ProgramData%\OneDriveHelper.
func ProgramDataDir() string {
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = `C:\ProgramData`
	}
	return filepath.Join(pd, "OneDriveHelper")
}
