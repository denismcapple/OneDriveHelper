package winapi

import (
	"os"
	"path/filepath"
	"testing"
)

// Only an existing folder is ever opened: never a file (which would start
// its program) or a missing path. Opening a real folder isn't tested here
// because it would open an Explorer window.
func TestOpenFolderRefusesFilesAndMissingPaths(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "evil.exe")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{file, filepath.Join(dir, "gone")} {
		if err := OpenFolder(p); err == nil {
			t.Errorf("%s must be refused", p)
		}
	}
}
