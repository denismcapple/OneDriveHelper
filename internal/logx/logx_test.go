package logx

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/windows"
)

func openT(t *testing.T, p string) *log.Logger {
	lg := Open(p, false)
	t.Cleanup(func() {
		if c, ok := lg.Writer().(io.Closer); ok {
			c.Close()
		}
	})
	return lg
}

func shrink(t *testing.T, n int64) {
	saved := maxSize
	maxSize = n
	t.Cleanup(func() { maxSize = saved })
}

func TestRotatesWhileRunning(t *testing.T) {
	shrink(t, 1000)
	p := filepath.Join(t.TempDir(), "agent.log")
	lg := openT(t, p)
	for i := 0; i < 30; i++ {
		lg.Printf("line %02d %s", i, strings.Repeat("x", 40))
	}
	cur, err1 := os.Stat(p)
	old, err2 := os.Stat(p + ".1")
	if err1 != nil || err2 != nil {
		t.Fatalf("both files should exist: %v %v", err1, err2)
	}
	if cur.Size() > maxSize || old.Size() > maxSize {
		t.Errorf("sizes %d / %d exceed %d", cur.Size(), old.Size(), maxSize)
	}
}

func TestSecondRotationReplacesOld(t *testing.T) {
	shrink(t, 200)
	p := filepath.Join(t.TempDir(), "service.log")
	lg := openT(t, p)
	for i := 0; i < 50; i++ {
		lg.Printf("entry %03d %s", i, strings.Repeat("y", 30))
	}
	b, _ := os.ReadFile(p + ".1")
	if strings.Contains(string(b), "entry 000") {
		t.Error("the oldest lines should have been rotated away")
	}
	if c, _ := os.ReadFile(p); !strings.Contains(string(c), "entry 049") {
		t.Error("the newest line should be in the current file")
	}
}

func TestConcurrentWritersNoTornLines(t *testing.T) {
	shrink(t, 4000)
	p := filepath.Join(t.TempDir(), "agent.log")
	lg := openT(t, p)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				lg.Printf("g%d-i%03d-%s", g, i, strings.Repeat("z", 20))
			}
		}(g)
	}
	wg.Wait()
	for _, f := range []string{p, p + ".1"} {
		fh, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasSuffix(line, strings.Repeat("z", 20)) {
				t.Errorf("torn line in %s: %q", f, line)
			}
		}
		fh.Close()
	}
}

func TestCreateProtectedDirHasNoInheritedACEs(t *testing.T) {
	sid, err := currentSID()
	if err != nil {
		t.Skip(err)
	}
	dir := filepath.Join(t.TempDir(), "OneDriveHelper")
	// Same shape as the real descriptor, plus the test user so the temp folder can be cleaned up.
	sddl := "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;BU)(A;OICI;FA;;;" + sid + ")"
	if err := createProtectedDir(dir, sddl); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	ctl, _, err := sd.Control()
	if err != nil || ctl&windows.SE_DACL_PROTECTED == 0 {
		t.Errorf("DACL must be protected from the start (control %#x, %v)", ctl, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
			t.Errorf("ACE %d is inherited", i)
		}
	}
}

func TestPrepareProgramDataDirMovesUntrustedFolder(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("needs elevation")
	}
	pd := t.TempDir()
	t.Setenv("ProgramData", pd)
	planted := filepath.Join(pd, "OneDriveHelper")
	os.MkdirAll(planted, 0o755) // created by "a user" (not owned by Administrators when run as a normal admin)
	os.WriteFile(filepath.Join(planted, "service.log"), []byte("planted"), 0o644)
	if err := PrepareProgramDataDir(); err != nil {
		t.Fatal(err)
	}
	if !ownedByAdmins(planted) {
		t.Error("the folder must end up owned by Administrators")
	}
}

func TestOpenDoesNotCreateProgramDataFolder(t *testing.T) {
	pd := t.TempDir()
	t.Setenv("ProgramData", pd)
	lg := Open(filepath.Join(pd, "OneDriveHelper", "service.log"), false)
	if lg.Writer() != os.Stderr {
		t.Error("must fall back to stderr instead of creating the folder with inherited permissions")
	}
	if _, err := os.Stat(filepath.Join(pd, "OneDriveHelper")); err == nil {
		t.Error("folder must not be created by Open")
	}
}

func currentSID() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
}

func TestFallsBackToStderr(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	os.WriteFile(blocker, []byte("x"), 0o644)
	lg := Open(filepath.Join(blocker, "sub", "agent.log"), false) // parent is a file: can't create
	if lg.Writer() != os.Stderr {
		t.Errorf("expected stderr, got %T", lg.Writer())
	}
	_ = fmt.Sprint()
}
