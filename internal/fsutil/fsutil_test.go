package fsutil

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestIsLink(t *testing.T) {
	for _, c := range []struct {
		name string
		e    Entry
		want bool
	}{
		{"mount point", Entry{Attr: AttrReparsePoint | AttrDirectory, Tag: TagMountPoint}, true},
		{"symlink", Entry{Attr: AttrReparsePoint, Tag: TagSymlink}, true},
		{"cloud root tag", Entry{Attr: AttrReparsePoint | AttrDirectory, Tag: 0x9000001A}, false},
		{"cloud file tag", Entry{Attr: AttrReparsePoint, Tag: 0x9000701A}, false},
		{"mount tag without reparse attribute", Entry{Attr: AttrDirectory, Tag: TagMountPoint}, false},
		{"symlink tag without reparse attribute", Entry{Tag: TagSymlink}, false},
	} {
		if got := c.e.IsLink(); got != c.want {
			t.Errorf("%s: IsLink = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIsCloud(t *testing.T) {
	for _, c := range []struct {
		name string
		e    Entry
		want bool
	}{
		{"cloud root", Entry{Attr: AttrReparsePoint, Tag: 0x9000001A}, true},
		{"cloud placeholder", Entry{Attr: AttrReparsePoint, Tag: 0x9000701A}, true},
		{"mount point", Entry{Attr: AttrReparsePoint, Tag: TagMountPoint}, false},
		{"symlink", Entry{Attr: AttrReparsePoint, Tag: TagSymlink}, false},
		{"zero tag", Entry{Attr: AttrReparsePoint}, false},
		{"cloud tag without reparse attribute", Entry{Tag: 0x9000701A}, false},
	} {
		if got := c.e.IsCloud(); got != c.want {
			t.Errorf("%s: IsCloud = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestOnlineOnly(t *testing.T) {
	for _, c := range []struct {
		name string
		attr uint32
		want bool
	}{
		{"offline", AttrOffline, true},
		{"recall on open", AttrRecallOnOpen, true},
		{"recall on data access", AttrRecallOnDataAccess, true},
		{"plain file", 0x20, false},
		{"read-only", AttrReadOnly, false},
	} {
		if got := (Entry{Attr: c.attr}).OnlineOnly(); got != c.want {
			t.Errorf("%s: OnlineOnly = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIsDir(t *testing.T) {
	if !(Entry{Attr: AttrDirectory}).IsDir() || (Entry{Attr: 0x20}).IsDir() {
		t.Error("IsDir")
	}
}

func TestReadDirDoesNotFollowJunctions(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "sub")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "inside.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	junction := filepath.Join(root, "junction")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, target).CombinedOutput(); err != nil {
		t.Skipf("cannot create a junction here: %v %s", err, out)
	}
	entries, err := ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Entry{}
	for _, e := range entries {
		got[e.Name] = e
	}
	if len(entries) != 3 {
		t.Fatalf("want 3 entries, got %+v", entries)
	}
	j := got["junction"]
	if !j.IsLink() || j.Tag != TagMountPoint || j.Attr&AttrReparsePoint == 0 {
		t.Errorf("junction entry: %+v", j)
	}
	if got["sub"].IsLink() || !got["sub"].IsDir() || got["file.txt"].Size != 1 {
		t.Errorf("entries: %+v", got)
	}
}

// A missing folder is an error (ERROR_PATH_NOT_FOUND), not an empty listing:
// callers such as the remediation walker record it as a scan error.
func TestReadDirMissingFolder(t *testing.T) {
	entries, err := ReadDir(filepath.Join(t.TempDir(), "nope"))
	if !errors.Is(err, windows.ERROR_PATH_NOT_FOUND) || len(entries) != 0 {
		t.Errorf("missing folder: %v %v", entries, err)
	}
}

func TestReadDirEmptyFolder(t *testing.T) {
	entries, err := ReadDir(t.TempDir())
	if err != nil || len(entries) != 0 {
		t.Errorf("empty folder: %v %v", entries, err)
	}
}
