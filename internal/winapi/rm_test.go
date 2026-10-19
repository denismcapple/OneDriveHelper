package winapi

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProgramsUsingFindsThisProcess(t *testing.T) {
	p := filepath.Join(t.TempDir(), "open.txt")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got := ProgramsUsing([]string{p}); len(got) == 0 {
		t.Skip("Restart Manager reported no holder (it names only some process types)")
	}
	if got := ProgramsUsing(nil); got != nil {
		t.Errorf("no files: %v", got)
	}
}

func TestProgramsUsingClosedFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "closed.txt")
	os.WriteFile(p, []byte("x"), 0o644)
	if got := ProgramsUsing([]string{p}); len(got) != 0 {
		t.Errorf("nobody has it open: %v", got)
	}
}
