package agent

import (
	"encoding/base64"
	"fmt"
	"html"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/denismcapple/OneDriveHelper/internal/config"
)

// supportEML builds an unsent draft e-mail (.eml with "X-Unsent: 1", which
// Outlook and most desktop mail programs open as a new, editable message).
// The body has a short intro and the full diagnostic report in a small
// monospace font; a plain-text version is included for text-only clients.
func supportEML(c config.Config, addr string, s Status, report string) []byte {
	host, _ := os.Hostname()
	subject := "OneDrive problem on " + host + ": " + s.summary()

	var problems []string
	if s.NotRunning() {
		problems = append(problems, "OneDrive is not running")
	}
	for _, o := range s.Orphans {
		problems = append(problems, "Not syncing: "+o.Path)
	}
	if len(problems) == 0 {
		problems = append(problems, "No problems detected right now")
	}

	var plain strings.Builder
	plain.WriteString("Hello,\r\n\r\nOneDrive Helper on my computer reports a problem:\r\n\r\n")
	for _, p := range problems {
		plain.WriteString("- " + p + "\r\n")
	}
	plain.WriteString("\r\nDiagnostic details:\r\n\r\n" + report)

	var h strings.Builder
	h.WriteString(`<html><head><meta charset="utf-8"></head><body style="font-family:'Segoe UI',Calibri,Arial,sans-serif;font-size:11pt;color:#1f1f1f">`)
	h.WriteString("<p>Hello,</p><p>OneDrive Helper on my computer reports a problem:</p><ul>")
	for _, p := range problems {
		h.WriteString("<li>" + html.EscapeString(p) + "</li>")
	}
	h.WriteString("</ul><p>Diagnostic details:</p>")
	h.WriteString(`<pre style="font-family:Consolas,'Courier New',monospace;font-size:8pt;line-height:1.35;color:#444444;` +
		`background:#f6f6f6;border:1px solid #dddddd;padding:8px;white-space:pre-wrap">`)
	h.WriteString(html.EscapeString(strings.ReplaceAll(report, "\r\n", "\n")))
	h.WriteString("</pre></body></html>")

	const boundary = "OneDriveHelper-boundary-7f3a9c"
	var b strings.Builder
	fmt.Fprintf(&b, "To: %s\r\n", addr)
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("X-Unsent: 1\r\nMIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=\"%s\"\r\n\r\n", boundary)
	for _, part := range []struct{ typ, body string }{{"text/plain", plain.String()}, {"text/html", h.String()}} {
		fmt.Fprintf(&b, "--%s\r\nContent-Type: %s; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n", boundary, part.typ)
		enc := base64.StdEncoding.EncodeToString([]byte(part.body))
		for len(enc) > 76 {
			b.WriteString(enc[:76] + "\r\n")
			enc = enc[76:]
		}
		b.WriteString(enc + "\r\n")
	}
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return []byte(b.String())
}

// writeSupportEML saves the draft under %TEMP%\OneDriveHelper and removes
// drafts older than a day.
func writeSupportEML(data []byte) (string, error) {
	dir := filepath.Join(os.TempDir(), "OneDriveHelper")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if old, _ := filepath.Glob(filepath.Join(dir, "Support request *.eml")); len(old) > 0 {
		for _, f := range old {
			if fi, err := os.Stat(f); err == nil && time.Since(fi.ModTime()) > 24*time.Hour {
				_ = os.Remove(f)
			}
		}
	}
	path := filepath.Join(dir, "Support request "+time.Now().Format("2006-01-02 150405")+".eml")
	return path, os.WriteFile(path, data, 0o600)
}
