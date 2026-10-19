// Package fsutil lists directories with raw Win32 attributes so cloud
// placeholders, junctions and symlinks can be told apart reliably.
package fsutil

import (
	"errors"
	"path/filepath"

	"golang.org/x/sys/windows"

	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

const (
	AttrReadOnly           = 0x1
	AttrDirectory          = 0x10
	AttrReparsePoint       = 0x400
	AttrOffline            = 0x1000
	AttrRecallOnOpen       = 0x40000
	AttrRecallOnDataAccess = 0x400000

	TagMountPoint = 0xA0000003
	TagSymlink    = 0xA000000C
	tagCloudMask  = 0xFFFF0FFF
	tagCloud      = 0x9000001A
)

type Entry struct {
	Name       string
	Attr       uint32
	Tag        uint32
	Size       int64
	Creation   windows.Filetime
	LastAccess windows.Filetime
	LastWrite  windows.Filetime
}

func (e Entry) IsDir() bool { return e.Attr&AttrDirectory != 0 }

// IsLink reports a symlink or junction; these must never be followed.
func (e Entry) IsLink() bool {
	return e.Attr&AttrReparsePoint != 0 && (e.Tag == TagSymlink || e.Tag == TagMountPoint)
}

// IsCloud reports a Cloud Files (OneDrive) placeholder / sync root.
func (e Entry) IsCloud() bool {
	return e.Attr&AttrReparsePoint != 0 && e.Tag&tagCloudMask == tagCloud
}

// OnlineOnly reports a file whose content is not stored locally (Files On-Demand).
func (e Entry) OnlineOnly() bool {
	return e.Attr&(AttrOffline|AttrRecallOnOpen|AttrRecallOnDataAccess) != 0
}

// ReadDir lists dir (excluding . and ..) without following reparse points or
// triggering hydration of placeholders.
func ReadDir(dir string) ([]Entry, error) {
	pattern, err := windows.UTF16PtrFromString(winapi.LongPath(filepath.Join(dir, "*")))
	if err != nil {
		return nil, err
	}
	var fd windows.Win32finddata
	h, err := windows.FindFirstFile(pattern, &fd)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return nil, nil
		}
		return nil, err
	}
	defer windows.FindClose(h)
	var out []Entry
	for {
		name := windows.UTF16ToString(fd.FileName[:])
		if name != "." && name != ".." {
			e := Entry{
				Name:       name,
				Attr:       fd.FileAttributes,
				Size:       int64(fd.FileSizeHigh)<<32 | int64(fd.FileSizeLow),
				Creation:   fd.CreationTime,
				LastAccess: fd.LastAccessTime,
				LastWrite:  fd.LastWriteTime,
			}
			if e.Attr&AttrReparsePoint != 0 {
				e.Tag = fd.Reserved0
			}
			out = append(out, e)
		}
		if err := windows.FindNextFile(h, &fd); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				return out, nil
			}
			return out, err
		}
	}
}
