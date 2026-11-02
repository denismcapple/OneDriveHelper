package folders

import (
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// asUTF16 is how gpresult writes the report.
func asUTF16(s string) []byte {
	out := []byte{0xFF, 0xFE}
	for _, u := range utf16.Encode([]rune(s)) {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

// The report gpresult wrote for a real user while "Staff - Redirected folders"
// applied (the denied and invalid GPOs, and one without Folder Redirection,
// are from the same report).
func TestRedirectionGPOsInScope(t *testing.T) {
	report := readFixture(t, "rsop-in-scope.xml")
	for _, b := range [][]byte{report, asUTF16(string(report))} {
		got, err := redirectionGPOs(b)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(got, ",") != "Staff - Redirected folders" {
			t.Errorf("applied Folder Redirection GPOs: %q", got)
		}
	}
}

func TestRedirectionGPOsOutOfScope(t *testing.T) {
	in := string(readFixture(t, "rsop-in-scope.xml"))
	link := "<Enabled>true</Enabled><NoOverride>false</NoOverride></Link><SecurityFilter>NT AUTHORITY"
	for name, report := range map[string]string{
		"link disabled (as gpresult showed 'Disabled (Link)')": strings.Replace(in, link, strings.Replace(link, "true", "false", 1), 1),
		"filtered out by security":                             strings.Replace(in, "<FilterAllowed>true</FilterAllowed><AccessDenied>false</AccessDenied><Link><SOMPath>contoso.local/Contoso/Staff</SOMPath><SOMOrder>8", "<FilterAllowed>false</FilterAllowed><AccessDenied>true</AccessDenied><Link><SOMPath>contoso.local/Contoso/Staff</SOMPath><SOMOrder>8", 1),
		"GPO no longer in the report":                          strings.Replace(in, "Staff - Redirected folders", "Staff - Redirected folders-gone", 1),
	} {
		if name == "GPO no longer in the report" {
			report = strings.Replace(report, "<ExtensionName>Folder Redirection</ExtensionName>", "", 1)
		}
		got, err := redirectionGPOs([]byte(report))
		if err != nil || len(got) != 0 {
			t.Errorf("%s: %q %v", name, got, err)
		}
	}
}

// On non-English Windows the extension's name is translated: it is matched
// through its data's (untranslated) type.
func TestRedirectionGPOsTranslated(t *testing.T) {
	in := strings.ReplaceAll(string(readFixture(t, "rsop-in-scope.xml")), "Folder Redirection", "Ordnerumleitung")
	if got, _ := redirectionGPOs([]byte(in)); strings.Join(got, ",") != "Staff - Redirected folders" {
		t.Errorf("translated: %q", got)
	}
}

func TestPolicyGPOsCaches(t *testing.T) {
	saved := gpresult
	defer func() { gpresult = saved }()
	calls := 0
	report := readFixture(t, "rsop-in-scope.xml")
	gpresult = func(string) ([]byte, error) { calls++; return report, nil }
	policyCache.Lock()
	policyCache.at = nil
	policyCache.Unlock()
	for i := 0; i < 3; i++ {
		if got, err := PolicyGPOs(""); err != nil || len(got) != 1 {
			t.Fatalf("%q %v", got, err)
		}
	}
	if calls != 1 {
		t.Errorf("gpresult ran %d times; answers are reused for %s", calls, policyCacheFor)
	}
	policyCache.Lock()
	policyCache.at[""] = time.Now().Add(-policyCacheFor)
	policyCache.Unlock()
	PolicyGPOs("")
	if calls != 2 {
		t.Error("an old answer is refreshed")
	}
}
