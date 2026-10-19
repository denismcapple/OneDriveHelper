package remediate

import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/denismcapple/OneDriveHelper/internal/fsutil"
	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

const fileAttributeTagInfo = 9 // FILE_INFO_BY_HANDLE_CLASS.FileAttributeTagInfo

type tagInfo struct {
	Attr uint32
	Tag  uint32
}

func (t tagInfo) isLink() bool {
	return t.Attr&fsutil.AttrReparsePoint != 0 && (t.Tag == fsutil.TagSymlink || t.Tag == fsutil.TagMountPoint)
}

// openNoFollow opens path without following reparse points. Directory handles
// are opened without FILE_SHARE_DELETE so the directory cannot be renamed or
// replaced (e.g. by a junction) while we work inside it.
func openNoFollow(path string, dir bool) (windows.Handle, tagInfo, error) {
	p, err := windows.UTF16PtrFromString(winapi.LongPath(path))
	if err != nil {
		return 0, tagInfo{}, err
	}
	access := uint32(windows.GENERIC_READ)
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT | windows.FILE_FLAG_SEQUENTIAL_SCAN)
	if dir {
		access = windows.FILE_LIST_DIRECTORY | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE
		flags = windows.FILE_FLAG_OPEN_REPARSE_POINT | windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	h, err := windows.CreateFile(p, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return 0, tagInfo{}, err
	}
	var ti tagInfo
	if err := windows.GetFileInformationByHandleEx(h, fileAttributeTagInfo, (*byte)(unsafe.Pointer(&ti)), uint32(unsafe.Sizeof(ti))); err != nil {
		windows.CloseHandle(h)
		return 0, tagInfo{}, err
	}
	return h, ti, nil
}

// Stats summarises an orphaned folder.
type Stats struct {
	LocalFiles      int      `json:"localFiles"`
	LocalBytes      int64    `json:"localBytes"`
	OnlineOnlyFiles int      `json:"onlineOnlyFiles"`
	Dirs            int      `json:"dirs"`
	LinksSkipped    int      `json:"linksSkipped"`
	Errors          []string `json:"errors,omitempty"`
}

func (s *Stats) fail(format string, a ...any) {
	if len(s.Errors) < 200 {
		s.Errors = append(s.Errors, fmt.Sprintf(format, a...))
	}
}

// walker visits a tree without following links. If dst is non-empty, locally
// stored files are copied there and every entry is recorded in rec (the
// "copy" pass); otherwise it only counts.
type walker struct {
	stats      Stats
	dst        string
	rec        record
	onlineOnly []string // relative paths of files that only exist in the cloud
}

func (w *walker) dir(src, dst, rel string) {
	h, ti, err := openNoFollow(src, true)
	if err != nil {
		w.stats.fail("open folder %s: %v", src, err)
		return
	}
	defer windows.CloseHandle(h)
	if ti.isLink() {
		w.stats.LinksSkipped++
		if w.rec != nil {
			w.rec[rel] = &recEntry{Kind: kindLink}
		}
		return
	}
	w.stats.Dirs++
	if w.rec != nil {
		m, err := metaOf(h)
		if err != nil {
			w.stats.fail("read %s: %v", src, err)
		}
		w.rec[rel] = &recEntry{Kind: kindDir, Meta: m}
	}
	if w.dst != "" {
		if err := os.MkdirAll(winapi.LongPath(dst), 0o755); err != nil {
			w.stats.fail("create %s: %v", dst, err)
			return
		}
	}
	entries, err := fsutil.ReadDir(src)
	if err != nil {
		w.stats.fail("list %s: %v", src, err)
	}
	for _, e := range entries {
		s, d, r := filepath.Join(src, e.Name), filepath.Join(dst, e.Name), filepath.Join(rel, e.Name)
		switch {
		case e.IsLink():
			w.stats.LinksSkipped++
			if w.rec != nil {
				w.rec[r] = &recEntry{Kind: kindLink}
			}
		case e.IsDir():
			w.dir(s, d, r)
		case e.OnlineOnly():
			w.stats.OnlineOnlyFiles++
			w.onlineOnly = append(w.onlineOnly, r)
			if w.rec != nil {
				m, ti, err := metaNoFollow(s, false)
				if err != nil || ti.isLink() {
					w.stats.fail("read %s: %v", s, err)
				}
				w.rec[r] = &recEntry{Kind: kindOnline, Meta: m}
			}
		default:
			w.stats.LocalFiles++
			w.stats.LocalBytes += e.Size
			if w.dst != "" {
				m, err := copyFileRecorded(s, d)
				if err != nil {
					w.stats.fail("copy %s: %v", s, err)
				}
				if w.rec != nil {
					w.rec[r] = &recEntry{Kind: kindFile, Meta: m, Verified: err == nil}
				}
			}
		}
	}
}

// Scan counts what is stored locally in dir.
func Scan(dir string) (Stats, []string) {
	w := &walker{}
	w.dir(dir, "", "")
	return w.stats, w.onlineOnly
}

func removeFile(p string) error {
	u, _ := windows.UTF16PtrFromString(winapi.LongPath(p))
	_ = windows.SetFileAttributes(u, windows.FILE_ATTRIBUTE_NORMAL)
	if err := windows.DeleteFile(u); err != nil {
		return fmt.Errorf("delete %s: %w", p, err)
	}
	return nil
}

func removeDir(p string) error {
	u, _ := windows.UTF16PtrFromString(winapi.LongPath(p))
	_ = windows.SetFileAttributes(u, windows.FILE_ATTRIBUTE_NORMAL)
	if err := windows.RemoveDirectory(u); err != nil {
		return fmt.Errorf("remove folder %s: %w", p, err)
	}
	return nil
}
