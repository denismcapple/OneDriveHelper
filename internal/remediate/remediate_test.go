package remediate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
)

func TestCopyAndDeleteTree(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	must(t, os.MkdirAll(filepath.Join(src, "a", "b"), 0o755))
	must(t, os.WriteFile(filepath.Join(src, "top.txt"), []byte("hello"), 0o644))
	must(t, os.WriteFile(filepath.Join(src, "a", "b", "deep.txt"), []byte("world!"), 0o444)) // read-only

	st, online := Scan(src)
	if st.LocalFiles != 2 || st.LocalBytes != 11 || st.Dirs != 3 || len(online) != 0 || len(st.Errors) != 0 {
		t.Fatalf("scan: %+v", st)
	}
	dst := filepath.Join(root, "dst")
	if st, _ := CopyLocal(src, dst); len(st.Errors) != 0 {
		t.Fatalf("copy errors: %v", st.Errors)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "a", "b", "deep.txt")); err != nil || string(b) != "world!" {
		t.Fatalf("copied content: %q %v", b, err)
	}
	if err := DeleteTree(src); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if exists(src) {
		t.Fatal("source still exists")
	}
}

func TestInsideProfile(t *testing.T) {
	for p, want := range map[string]bool{
		`C:\Users\jsmith\Org\Lib`: true,
		`c:\users\JSMITH\org`:     true,
		`C:\Users\jsmith`:         false,
		`C:\Users\jsmith2\Org`:    false,
		`D:\Org\Lib`:              false,
	} {
		if got := insideProfile(p, `C:\Users\jsmith`); got != want {
			t.Errorf("insideProfile(%s) = %v", p, got)
		}
	}
}

func TestSaveName(t *testing.T) {
	used := map[string]bool{}
	a := saveName(onedrive.Orphan{Name: "Docs", TenantRoot: `C:\Users\d\Contoso`}, used)
	b := saveName(onedrive.Orphan{Name: "docs", TenantRoot: `C:\Users\d\Fabrikam`}, used)
	c := saveName(onedrive.Orphan{Name: "Docs", TenantRoot: `C:\Users\d\Fabrikam`}, used)
	if a != "Docs" || b != "docs (Fabrikam)" || c != "Docs (Fabrikam 2)" {
		t.Errorf("got %q %q %q", a, b, c)
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KB", 133706396: "127.5 MB"} {
		if got := HumanBytes(n); got != want {
			t.Errorf("HumanBytes(%d) = %s want %s", n, got, want)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
