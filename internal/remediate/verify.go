package remediate

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/denismcapple/OneDriveHelper/internal/fsutil"
	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

// entryKind is what a recorded entry was when it was backed up.
type entryKind int

const (
	kindFile entryKind = iota
	kindOnline
	kindDir
	kindLink
)

// fileMeta is what identifies one version of a file, read through a handle
// (directory listings can lag for open files, so they are never trusted).
type fileMeta struct {
	ID        [3]uint32 // volume serial, index high, index low
	Size      int64
	LastWrite int64
	Attr      uint32
}

func metaOf(h windows.Handle) (fileMeta, error) {
	var i windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &i); err != nil {
		return fileMeta{}, err
	}
	return fileMeta{
		ID:        [3]uint32{i.VolumeSerialNumber, i.FileIndexHigh, i.FileIndexLow},
		Size:      int64(i.FileSizeHigh)<<32 | int64(i.FileSizeLow),
		LastWrite: i.LastWriteTime.Nanoseconds(),
		Attr:      i.FileAttributes,
	}, nil
}

// recEntry is one item of the verified record for a folder.
type recEntry struct {
	Kind     entryKind
	Meta     fileMeta
	Verified bool // file: copied, flushed and size-checked, unchanged while read
}

// record maps paths relative to the staged folder ("" is the folder itself).
type record map[string]*recEntry

// copyRecorded backs up every locally stored file of src into dst, recording
// each entry from its open handle (see KTD2): a file is verified when its copy
// wrote exactly its size, was flushed, and the source did not change while it
// was being read. Online-only files are recorded but not copied.
func copyRecorded(src, dst string) (record, Stats, []string) {
	w := &walker{dst: dst, rec: record{}}
	w.dir(src, dst, "")
	return w.rec, w.stats, w.onlineOnly
}

// copyFileRecorded copies src to dst through an open source handle.
func copyFileRecorded(src, dst string) (fileMeta, error) {
	h, ti, err := openNoFollow(src, false)
	if err != nil {
		return fileMeta{}, err
	}
	in := os.NewFile(uintptr(h), src)
	defer in.Close()
	if ti.isLink() {
		return fileMeta{}, errors.New("is a link; skipped")
	}
	before, err := metaOf(h)
	if err != nil {
		return fileMeta{}, err
	}
	out, err := os.OpenFile(winapi.LongPath(dst), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return before, err
	}
	n, err := io.Copy(env.backupWriter(out), in)
	if err == nil {
		if ferr := env.flush(out); ferr != nil {
			err = fmt.Errorf("flush backup to disk: %w", ferr)
		}
	}
	if cerr := out.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		return before, err
	}
	if n != before.Size {
		return before, fmt.Errorf("copied %d of %d bytes", n, before.Size)
	}
	env.afterRead(src)
	after, err := metaOf(h)
	if err != nil {
		return before, err
	}
	if after != before {
		return before, errors.New("the file changed while it was being saved")
	}
	// Backup timestamps are cosmetic: verification never reads them.
	_ = os.Chtimes(winapi.LongPath(dst), time.Now(), time.Unix(0, before.LastWrite))
	return before, nil
}

// metaNoFollow reads an entry's identity without reading its content (and
// without triggering hydration of online-only placeholders).
func metaNoFollow(path string, dir bool) (fileMeta, tagInfo, error) {
	h, ti, err := openNoFollow(path, dir)
	if err != nil {
		return fileMeta{}, ti, err
	}
	defer windows.CloseHandle(h)
	m, err := metaOf(h)
	return m, ti, err
}

// verified holds every entry of a folder open for deletion after it passed
// the pre-delete check.
type verified struct {
	files []openEntry // files and online-only entries, open with DELETE
	dirs  []string    // relative directory paths, deepest first, "" last
}

type openEntry struct {
	rel string
	h   windows.Handle
}

func (v *verified) close() {
	for _, f := range v.files {
		if f.h != 0 {
			windows.CloseHandle(f.h)
		}
	}
	v.files = nil
}

// checkForDelete compares the staged folder with its record. Every entry on
// disk must be in the record and verified; every file is opened with DELETE
// access and sharing that excludes writers and deleters (so a file still open
// for writing fails here), then compared through that handle. Any problem is
// returned as a reason and nothing stays open.
func checkForDelete(staged string, rec record) (*verified, string) {
	found := map[string]bool{}
	var walk func(rel string) string
	walk = func(rel string) string {
		entries, err := fsutil.ReadDir(filepath.Join(staged, rel))
		if err != nil {
			return fmt.Sprintf("could not list %s: %v", rel, err)
		}
		for _, e := range entries {
			r := filepath.Join(rel, e.Name)
			found[r] = true
			re, ok := rec[r]
			switch {
			case !ok:
				return fmt.Sprintf("%s was added after the backup", r)
			case re.Kind == kindLink || e.IsLink():
				return fmt.Sprintf("%s is a shortcut link (junction or symbolic link)", r)
			case e.IsDir() != (re.Kind == kindDir):
				return fmt.Sprintf("%s changed type after the backup", r)
			case e.IsDir():
				if why := walk(r); why != "" {
					return why
				}
			}
		}
		return ""
	}
	if why := walk(""); why != "" {
		return nil, why
	}
	v := &verified{}
	for r, re := range rec {
		if r != "" && !found[r] {
			return nil, fmt.Sprintf("%s disappeared after the backup", r)
		}
		if re.Kind == kindDir {
			v.dirs = append(v.dirs, r)
			continue
		}
		if re.Kind == kindFile && !re.Verified {
			v.close()
			return nil, fmt.Sprintf("%s was not backed up", r)
		}
		h, err := openForDelete(filepath.Join(staged, r), false)
		if err != nil {
			v.close()
			return nil, fmt.Sprintf("%s is open in another program: %v", r, err)
		}
		m, err := metaOf(h)
		if err != nil || m != re.Meta {
			windows.CloseHandle(h)
			v.close()
			return nil, fmt.Sprintf("%s changed after the backup", r)
		}
		v.files = append(v.files, openEntry{rel: r, h: h})
	}
	depth := func(r string) int {
		if r == "" {
			return -1 // the folder itself goes last
		}
		return strings.Count(r, `\`)
	}
	sort.SliceStable(v.dirs, func(i, j int) bool { return depth(v.dirs[i]) > depth(v.dirs[j]) })
	return v, ""
}

// openForDelete opens an entry without following reparse points, with DELETE
// access and sharing that excludes other writers and deleters.
func openForDelete(path string, dir bool) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(winapi.LongPath(path))
	if err != nil {
		return 0, err
	}
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if dir {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	return windows.CreateFile(p, windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.FILE_WRITE_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, flags, 0)
}

// deleteByHandle marks an open entry for deletion (clearing read-only first).
func deleteByHandle(h windows.Handle) error {
	if m, err := metaOf(h); err == nil && m.Attr&windows.FILE_ATTRIBUTE_READONLY != 0 {
		attr := m.Attr &^ windows.FILE_ATTRIBUTE_READONLY
		if attr == 0 {
			attr = windows.FILE_ATTRIBUTE_NORMAL
		}
		var bi [40]byte // FILE_BASIC_INFO: zero times = unchanged
		binary.LittleEndian.PutUint32(bi[32:], attr)
		if err := windows.SetFileInformationByHandle(h, windows.FileBasicInfo, &bi[0], uint32(len(bi))); err != nil {
			return err
		}
	}
	del := byte(1) // FILE_DISPOSITION_INFO{DeleteFile: TRUE}
	return windows.SetFileInformationByHandle(h, windows.FileDispositionInfo, &del, 1)
}

// deleteVerified removes every checked entry through its handle, files first,
// then directories (deepest first). It stops at the first failure and returns
// the relative paths that were not removed.
func deleteVerified(staged string, v *verified, rec record) (remaining []string, err error) {
	for i, f := range v.files {
		derr := env.deleteEntry(f.h)
		windows.CloseHandle(f.h)
		v.files[i].h = 0
		if derr != nil {
			for _, rest := range v.files[i:] {
				remaining = append(remaining, rest.rel)
			}
			v.close()
			return append(remaining, v.dirs...), fmt.Errorf("delete %s: %w", f.rel, derr)
		}
	}
	v.files = nil
	for i, r := range v.dirs {
		h, oerr := openForDelete(filepath.Join(staged, r), true)
		if oerr == nil {
			if m, merr := metaOf(h); merr != nil || m.ID != rec[r].Meta.ID {
				oerr = errors.New("folder changed")
			} else if ti, terr := tagOf(h); terr != nil || ti.isLink() {
				oerr = errors.New("is a link")
			} else {
				oerr = env.deleteEntry(h)
			}
			windows.CloseHandle(h)
		}
		if oerr != nil {
			return v.dirs[i:], fmt.Errorf("remove folder %s: %w", filepath.Join(staged, r), oerr)
		}
	}
	return nil, nil
}

func tagOf(h windows.Handle) (tagInfo, error) {
	var ti tagInfo
	err := windows.GetFileInformationByHandleEx(h, fileAttributeTagInfo, (*byte)(unsafe.Pointer(&ti)), uint32(unsafe.Sizeof(ti)))
	return ti, err
}

// --- moving a folder by handle ---

var errIsLink = errors.New("is a junction or symbolic link")

// errBusy marks a move that failed because something has the folder (or a
// file in it) open.
type errBusy struct{ err error }

func (e errBusy) Error() string { return e.err.Error() }
func (e errBusy) Unwrap() error { return e.err }

// A busy folder is tried moveTries times over about 5 seconds (vars for tests).
var (
	moveTries    = 3
	moveInterval = 2500 * time.Millisecond
)

// moveByHandle renames the folder src to dst (same volume) through a handle
// opened without following reparse points, so nothing is re-resolved between
// the link check and the rename. Busy folders are retried a few times.
func moveByHandle(src, dst string) error {
	var last error
	for try := 1; try <= moveTries; try++ {
		last = renameOnce(src, dst)
		if last == nil {
			return nil
		}
		var busy errBusy
		if !errors.As(last, &busy) {
			return last
		}
		if try < moveTries {
			time.Sleep(moveInterval)
		}
	}
	return last
}

func renameOnce(src, dst string) error {
	p, err := windows.UTF16PtrFromString(winapi.LongPath(src))
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return classifyMoveErr(err)
	}
	defer windows.CloseHandle(h)
	if ti, err := tagOf(h); err != nil {
		return err
	} else if ti.isLink() {
		return errIsLink
	}
	name := windows.StringToUTF16(winapi.LongPath(dst))
	name = name[:len(name)-1]
	// FILE_RENAME_INFO: ReplaceIfExists(1, padded to 8) RootDirectory(8) FileNameLength(4) FileName
	buf := make([]byte, 20+len(name)*2+2)
	binary.LittleEndian.PutUint32(buf[16:], uint32(len(name)*2))
	for i, c := range name {
		binary.LittleEndian.PutUint16(buf[20+i*2:], c)
	}
	if err := windows.SetFileInformationByHandle(h, windows.FileRenameInfo, &buf[0], uint32(len(buf))); err != nil {
		return classifyMoveErr(err)
	}
	return nil
}

func classifyMoveErr(err error) error {
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return errBusy{err}
	}
	return err
}

// noLinkBelow checks every path component below profile, down to and
// including path, without following reparse points. It returns the first
// junction or symbolic link found. The profile folder itself is not checked
// (profiles moved with a junction are common and not this tool's concern).
func noLinkBelow(profile, path string) error {
	if !insideProfile(path, profile) {
		return fmt.Errorf("%s is not inside the profile %s", path, profile)
	}
	rel, err := filepath.Rel(profile, path)
	if err != nil {
		return err
	}
	cur := profile
	for _, part := range strings.Split(rel, `\`) {
		cur = filepath.Join(cur, part)
		h, ti, err := openNoFollow(cur, true)
		if err != nil {
			return fmt.Errorf("check %s: %w", cur, err)
		}
		windows.CloseHandle(h)
		if ti.isLink() {
			return fmt.Errorf("%s is a junction or symbolic link", cur)
		}
	}
	return nil
}

// parentSafe reports whether path's parent exists as a real folder (not a
// link), so moving a staged folder back there cannot be redirected.
func parentSafe(path string) bool {
	h, ti, err := openNoFollow(filepath.Dir(path), true)
	if err != nil {
		return false
	}
	windows.CloseHandle(h)
	return !ti.isLink() && ti.Attr&windows.FILE_ATTRIBUTE_DIRECTORY != 0
}
