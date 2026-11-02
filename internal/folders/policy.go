package folders

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// Whether a Folder Redirection policy still applies to a user is read from
// Windows' Resultant Set of Policy, as gpresult reports it. (The Folder
// Redirection extension's own history in the registry, which
// GetAppliedGPOList reads, was found empty while a policy applied, and
// still set after it was removed.)

// rsop is the part of "gpresult /scope user /x" output that matters.
type rsop struct {
	User struct {
		GPOs []rsopGPO `xml:"GPO"`
		Data []struct {
			Extension struct {
				Type string `xml:"http://www.w3.org/2001/XMLSchema-instance type,attr"`
			} `xml:"Extension"`
			Name string `xml:"Name"`
		} `xml:"ExtensionData"`
	} `xml:"UserResults"`
}

type rsopGPO struct {
	Name          string `xml:"Name"`
	Enabled       string `xml:"Enabled"`
	IsValid       string `xml:"IsValid"`
	FilterAllowed string `xml:"FilterAllowed"`
	AccessDenied  string `xml:"AccessDenied"`
	Links         []struct {
		Enabled string `xml:"Enabled"`
	} `xml:"Link"`
	Extensions []string `xml:"ExtensionName"`
}

func yes(s string) bool { return strings.EqualFold(strings.TrimSpace(s), "true") }

// applies reports whether the GPO is applied to the user: valid, enabled,
// not filtered out or denied, and linked with an enabled link.
func (g rsopGPO) applies() bool {
	if !yes(g.IsValid) || !yes(g.Enabled) || !yes(g.FilterAllowed) || yes(g.AccessDenied) {
		return false
	}
	for _, l := range g.Links {
		if yes(l.Enabled) {
			return true
		}
	}
	return false
}

// redirectionGPOs returns the names of the applied GPOs that carry Folder
// Redirection settings, from gpresult's XML report.
func redirectionGPOs(report []byte) ([]string, error) {
	var r rsop
	dec := xml.NewDecoder(bytes.NewReader(utf8Report(report)))
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil } // already UTF-8
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("read the Group Policy report: %w", err)
	}
	// The extension's name is translated on non-English Windows; its data's
	// type is not. Fall back to the English name.
	name := "Folder Redirection"
	for _, d := range r.User.Data {
		if strings.HasSuffix(d.Extension.Type, "FolderRedirectionSettings") && strings.TrimSpace(d.Name) != "" {
			name = strings.TrimSpace(d.Name)
		}
	}
	var out []string
	for _, g := range r.User.GPOs {
		if !g.applies() {
			continue
		}
		for _, e := range g.Extensions {
			if strings.EqualFold(strings.TrimSpace(e), name) {
				out = append(out, g.Name)
				break
			}
		}
	}
	return out, nil
}

// utf8Report converts gpresult's report (UTF-16 or UTF-8, with or without a
// byte order mark) to UTF-8.
func utf8Report(b []byte) []byte {
	switch {
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
		u := make([]uint16, (len(b)-2)/2)
		for i := range u {
			u[i] = uint16(b[2+2*i]) | uint16(b[3+2*i])<<8
		}
		return []byte(string(utf16.Decode(u)))
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		return b[3:]
	}
	return b
}

// gpresult runs "gpresult /scope user /x <file>" (for another user with
// /user, which needs admin) and returns the report. Hidden: no console
// window flashes up.
var gpresult = func(user string) ([]byte, error) {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp("", "OneDriveHelper-rsop-*.xml")
	if err != nil {
		return nil, err
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)
	args := []string{"/scope", "user", "/x", path, "/f"}
	if user != "" {
		args = append([]string{"/user", user}, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(sys, "gpresult.exe"), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("gpresult: %v %s", err, strings.TrimSpace(string(out)))
	}
	return os.ReadFile(path)
}

// policyCacheFor: how long a gpresult answer is reused. Policy changes only
// apply at a policy refresh or sign-in anyway.
const policyCacheFor = 15 * time.Minute

var policyCache struct {
	sync.Mutex
	at    map[string]time.Time
	names map[string][]string
	errs  map[string]error
}

// PolicyGPOs returns the names of the Group Policy objects with Folder
// Redirection settings that currently apply to the user (sid "" = the
// current user), as gpresult reports them. Empty means no redirection
// policy applies any more. Answers are reused for 15 minutes.
func PolicyGPOs(sid string) ([]string, error) {
	policyCache.Lock()
	defer policyCache.Unlock()
	if t, ok := policyCache.at[sid]; ok && time.Since(t) < policyCacheFor {
		return policyCache.names[sid], policyCache.errs[sid]
	}
	names, err := policyGPOs(sid)
	if policyCache.at == nil {
		policyCache.at, policyCache.names, policyCache.errs = map[string]time.Time{}, map[string][]string{}, map[string]error{}
	}
	policyCache.at[sid], policyCache.names[sid], policyCache.errs[sid] = time.Now(), names, err // failures too: gpresult takes seconds
	return names, err
}

func policyGPOs(sid string) ([]string, error) {
	user := ""
	if sid != "" {
		s, err := windows.StringToSid(sid)
		if err != nil {
			return nil, err
		}
		acct, domain, _, err := s.LookupAccount("")
		if err != nil {
			return nil, fmt.Errorf("look up %s: %w", sid, err)
		}
		user = domain + `\` + acct
	}
	report, err := gpresult(user)
	if err != nil {
		return nil, err
	}
	return redirectionGPOs(report)
}
