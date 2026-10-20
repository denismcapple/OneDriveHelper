package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	elogFile = filepath.Join("..", "..", "internal", "elog", "elog.go")
	cmdDir   = filepath.Join("..", "..", "cmd", "OneDriveHelper")
)

func TestSysoUpToDate(t *testing.T) {
	ids, err := EventIDs(elogFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range Targets {
		got, err := os.ReadFile(filepath.Join(cmdDir, FileName(tg.Arch)))
		if err != nil || !bytes.Equal(got, Object(tg.Machine, tg.Reloc, ids)) {
			t.Errorf("%s is missing or out of date (%v): run go generate ./cmd/OneDriveHelper", FileName(tg.Arch), err)
		}
	}
}

func TestEventIDs(t *testing.T) {
	ids, err := EventIDs(elogFile)
	if err != nil {
		t.Fatal(err)
	}
	has := map[uint32]bool{}
	for _, id := range ids {
		has[id] = true
	}
	for _, id := range []uint32{1000, 3001, 3005, 3006, 4001, 5013} {
		if !has[id] {
			t.Errorf("event ID %d not found in %v", id, ids)
		}
	}
}

// TestBuiltExeFormatsEveryEvent builds OneDriveHelper.exe for each
// architecture and has Windows format every event ID from it, as Event
// Viewer does. Loading as a data file works for any architecture.
func TestBuiltExeFormatsEveryEvent(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the service exe")
	}
	ids, err := EventIDs(elogFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range Targets {
		exe := filepath.Join(t.TempDir(), "OneDriveHelper.exe")
		cmd := exec.Command("go", "build", "-trimpath", "-o", exe, ".")
		cmd.Dir = cmdDir
		cmd.Env = append(os.Environ(), "GOOS=windows", "GOARCH="+tg.Arch, "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s build: %v\n%s", tg.Arch, err, out)
		}
		h, err := windows.LoadLibraryEx(exe, 0, windows.LOAD_LIBRARY_AS_DATAFILE|windows.LOAD_LIBRARY_AS_IMAGE_RESOURCE)
		if err != nil {
			t.Fatalf("%s: load: %v", tg.Arch, err)
		}
		for _, id := range ids {
			if got, err := format(h, id, "line one\r\nline two (50%)"); err != nil || got != "line one\r\nline two (50%)\r\n" {
				t.Errorf("%s: event %d formats as %q (%v)", tg.Arch, id, got, err)
			}
		}
		if _, err := format(h, 999, "x"); err == nil {
			t.Errorf("%s: an unknown ID should not format", tg.Arch)
		}
		_ = windows.FreeLibrary(h)
	}
}

func format(h windows.Handle, id uint32, insert string) (string, error) {
	s, _ := windows.UTF16PtrFromString(insert)
	args := []uintptr{uintptr(unsafe.Pointer(s))}
	buf := make([]uint16, 1024)
	n, err := windows.FormatMessage(windows.FORMAT_MESSAGE_FROM_HMODULE|windows.FORMAT_MESSAGE_ARGUMENT_ARRAY,
		uintptr(h), id, 0, buf, (*byte)(unsafe.Pointer(&args[0])))
	if err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:n]), nil
}
