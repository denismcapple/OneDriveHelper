package remediate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
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
// stored files are copied there (the "copy" pass); otherwise it only counts.
type walker struct {
	stats      Stats
	dst        string
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
		return
	}
	w.stats.Dirs++
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
		case e.IsDir():
			w.dir(s, d, r)
		case e.OnlineOnly():
			w.stats.OnlineOnlyFiles++
			w.onlineOnly = append(w.onlineOnly, r)
		default:
			w.stats.LocalFiles++
			w.stats.LocalBytes += e.Size
			if w.dst != "" {
				if err := copyFile(s, d, e); err != nil {
					w.stats.fail("copy %s: %v", s, err)
				}
			}
		}
	}
}

func copyFile(src, dst string, e fsutil.Entry) error {
	h, ti, err := openNoFollow(src, false)
	if err != nil {
		return err
	}
	in := os.NewFile(uintptr(h), src)
	defer in.Close()
	if ti.isLink() {
		return errors.New("is a link; skipped")
	}
	out, err := os.OpenFile(winapi.LongPath(dst), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != e.Size {
		return fmt.Errorf("copied %d of %d bytes", n, e.Size)
	}
	_ = os.Chtimes(winapi.LongPath(dst), time.Unix(0, e.LastAccess.Nanoseconds()), time.Unix(0, e.LastWrite.Nanoseconds()))
	return nil
}

// Scan counts what is stored locally in dir.
func Scan(dir string) (Stats, []string) {
	w := &walker{}
	w.dir(dir, "", "")
	return w.stats, w.onlineOnly
}

// CopyLocal copies every locally stored file from src into dst.
func CopyLocal(src, dst string) (Stats, []string) {
	w := &walker{dst: dst}
	w.dir(src, dst, "")
	return w.stats, w.onlineOnly
}

// DeleteTree removes dir and its contents. Links are removed, never followed.
func DeleteTree(dir string) error {
	h, ti, err := openNoFollow(dir, true)
	if err != nil {
		return err
	}
	var errs []error
	if !ti.isLink() {
		entries, err := fsutil.ReadDir(dir)
		errs = append(errs, err)
		for _, e := range entries {
			p := filepath.Join(dir, e.Name)
			switch {
			case e.IsDir() && !e.IsLink():
				errs = append(errs, DeleteTree(p))
			case e.IsDir():
				errs = append(errs, removeDir(p))
			default:
				errs = append(errs, removeFile(p))
			}
		}
	}
	windows.CloseHandle(h)
	errs = append(errs, removeDir(dir))
	return errors.Join(errs...)
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

// moveDir renames src to dst on the same volume (used for leftovers).
func moveDir(src, dst string) error {
	if err := os.MkdirAll(winapi.LongPath(filepath.Dir(dst)), 0o755); err != nil {
		return err
	}
	s, _ := windows.UTF16PtrFromString(winapi.LongPath(src))
	d, _ := windows.UTF16PtrFromString(winapi.LongPath(dst))
	return windows.MoveFileEx(s, d, 0)
}
