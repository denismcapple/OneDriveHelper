package remediate

import (
	"fmt"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

var procDeleteAce = windows.NewLazySystemDLL("advapi32.dll").NewProc("DeleteAce")

// readOnlyMask is what OneDrive denies Everyone on local copies of files
// from a library the user can only read: delete, write, append and delete
// child (icacls shows "Everyone:(DENY)(D,WD,AD,DC)"; SYNCHRONIZE is allowed
// too, as icacls adds it to such entries).
const readOnlyMask = windows.DELETE | windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | fileDeleteChild | windows.SYNCHRONIZE

const fileDeleteChild = 0x40 // FILE_DELETE_CHILD

// unlockReadOnly removes OneDrive's read-only lock from every file and folder
// in the record, so the verified copies can be deleted. Only explicit deny
// entries for Everyone that deny nothing beyond readOnlyMask are removed; any
// other permission is left alone. The user owns the files in their profile,
// and an owner may always change a file's permissions, so this needs no
// elevation. It returns how many entries were unlocked.
func unlockReadOnly(staged string, rec record) (int, []error) {
	n := 0
	var errs []error
	for rel, re := range rec {
		if re.Kind == kindLink {
			continue
		}
		changed, err := unlockEntry(filepath.Join(staged, rel), re.Kind == kindDir)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", rel, err))
		} else if changed {
			n++
		}
	}
	return n, errs
}

func unlockEntry(path string, dir bool) (bool, error) {
	p, err := windows.UTF16PtrFromString(winapi.LongPath(path))
	if err != nil {
		return false, err
	}
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if dir {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	h, err := windows.CreateFile(p, windows.READ_CONTROL|windows.WRITE_DAC|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(h)
	if ti, err := tagOf(h); err != nil || ti.isLink() {
		return false, err // never change a link's (or its target's) permissions
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return false, nil // no DACL: nothing is denied
	}
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		return false, err
	}
	removed := false
	for i := int(dacl.AceCount) - 1; i >= 0; i-- {
		var ace *windows.ACCESS_ALLOWED_ACE // same layout as ACCESS_DENIED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			return false, err
		}
		if ace.Header.AceType != windows.ACCESS_DENIED_ACE_TYPE || ace.Header.AceFlags&windows.INHERITED_ACE != 0 ||
			ace.Mask&^readOnlyMask != 0 || !(*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(everyone) {
			continue
		}
		if r, _, e := procDeleteAce.Call(uintptr(unsafe.Pointer(dacl)), uintptr(i)); r == 0 {
			return false, e
		}
		removed = true
	}
	if !removed {
		return false, nil
	}
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION)
	if ctl, _, err := sd.Control(); err == nil && ctl&windows.SE_DACL_PROTECTED != 0 {
		info |= windows.PROTECTED_DACL_SECURITY_INFORMATION // keep inheritance as it was
	}
	if err := windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, info, nil, nil, dacl, nil); err != nil {
		return false, err
	}
	return true, nil
}
