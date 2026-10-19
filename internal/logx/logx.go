// Package logx provides a small size-rotated file logger.
package logx

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

// maxSize is the size at which the log rotates to <file>.1 (so a log never
// takes more than about twice this). A var so tests can shrink it.
var maxSize int64 = 5 << 20

// Open returns a logger writing to path, rotated to path.1 whenever it grows
// past 5 MB, also while the process keeps running for weeks. If the file
// cannot be opened the logger falls back to stderr.
func Open(path string, alsoStderr bool) *log.Logger {
	var w io.Writer = os.Stderr
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		if r, err := openRotating(path); err == nil {
			w = r
			if alsoStderr {
				w = io.MultiWriter(r, os.Stderr)
			}
		}
	}
	return log.New(w, "", log.LstdFlags)
}

// rotating is an io.Writer that counts bytes and rotates the file to .1 when
// it passes maxSize. log.Logger serialises writes, but several loggers may
// share one rotating writer, so it has its own lock.
type rotating struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

func openRotating(path string) (*rotating, error) {
	r := &rotating{path: path}
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxSize {
		_ = os.Rename(path, path+".1")
	}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotating) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, fi.Size()
	return nil
}

func (r *rotating) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size+int64(len(p)) > maxSize && r.size > 0 {
		r.rotate()
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// Close closes the current file.
func (r *rotating) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}

// rotate moves the current file to .1 and starts a new one. If another
// process has the file open (e.g. a second session of the same user) the
// rename fails; then it keeps writing to the current file and tries again
// at the next write past the limit.
func (r *rotating) rotate() {
	r.f.Close()
	if err := os.Rename(r.path, r.path+".1"); err != nil {
		if err := r.open(); err != nil {
			r.f, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		}
		return
	}
	if err := r.open(); err != nil {
		r.f, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		r.size = 0
	}
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
