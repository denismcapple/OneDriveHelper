package remediate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/denismcapple/OneDriveHelper/internal/dialog"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
	"github.com/denismcapple/OneDriveHelper/internal/popup"
	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

// What the fix does with each group, said once under the group's heading.
const (
	ActionResync = "We will back up these files & folders, then try to sync them again."
	ActionBackup = "We will move these files & folders to a backup folder."
	ActionEmpty  = "We will remove these empty folders."
)

// Group is one group of folders on the cards, e.g. "Old copies".
type Group struct {
	Heading string // with the count, e.g. "Stopped syncing (2)"
	Action  string // what the fix does with them
	Orphans []onedrive.Orphan
}

// Groups sorts folders by what became of their library. Folders whose
// library can't be identified are shown as stopped.
func Groups(orphans []onedrive.Orphan) []Group {
	var stopped, moved, gone, empty []onedrive.Orphan
	for _, o := range orphans {
		switch {
		case o.EmptyOrg:
			empty = append(empty, o)
		case o.Kind == onedrive.KindMoved:
			moved = append(moved, o)
		case o.Kind == onedrive.KindDisconnected:
			gone = append(gone, o)
		default:
			stopped = append(stopped, o)
		}
	}
	var out []Group
	for _, g := range []Group{
		{"Stopped syncing", ActionResync, stopped},
		{movedHeading(moved), ActionBackup, moved},
		{"From an account that is no longer connected", ActionBackup, gone},
		{"Empty leftover folders", ActionEmpty, empty},
	} {
		if len(g.Orphans) > 0 {
			g.Heading = fmt.Sprintf("%s (%d)", g.Heading, len(g.Orphans))
			out = append(out, g)
		}
	}
	return out
}

// movedHeading names the folder old copies now sync to, if they all agree.
func movedHeading(moved []onedrive.Orphan) string {
	org := ""
	for i, o := range moved {
		d := filepath.Base(filepath.Dir(o.MovedTo))
		if i > 0 && !strings.EqualFold(d, org) {
			org = ""
			break
		}
		org = d
	}
	if org == "" || org == "." || org == `\` {
		return "Old copies: these libraries now sync to a new folder"
	}
	return fmt.Sprintf("Old copies: these libraries now sync to %q", org)
}

// exception is a short note after a folder's name when the group's action
// line doesn't fully apply to it ("" otherwise).
func exception(o onedrive.Orphan) string {
	switch {
	case len(o.KnownFolders) > 0:
		return "your support team will fix this one"
	case !o.NeedsResync() || o.CanResync():
		return ""
	case o.WebURL != "":
		return "click 'Sync' on its SharePoint page afterwards"
	}
	return "your support team will add it back"
}

// openFolder opens a listed folder in Explorer; folderExists decides whether
// a path is shown as a link (both replaced in tests).
var (
	openFolder   = func(path string) { _ = winapi.OpenFolder(path) }
	folderExists = func(path string) bool { fi, err := os.Stat(path); return err == nil && fi.IsDir() }
)

// PreviewLinks shows every path as a link, for screenshots made with
// made-up folders. Never set in the real programs.
var PreviewLinks bool

// link returns the action that opens path in Explorer, or nil (no link) if
// it isn't an existing folder.
func link(path string) func() {
	if path == "" || !(PreviewLinks || folderExists(path)) {
		return nil
	}
	return func() { openFolder(path) }
}

// savedTo is the "Your files are saved to <folder>" line, with the folder as
// a link once it exists.
func savedTo(label, dir string) popup.Item {
	return popup.Item{Title: label, Path: dir, Plain: true, Open: link(dir)}
}

// Items lists folders by group: a heading with the action line below it,
// then each folder's name with its path once, as a link that opens it in
// Explorer. At most max folders are listed (0 = all); more says how many
// were left out.
func Items(orphans []onedrive.Orphan, max int) (items []popup.Item, more string) {
	listed := 0
	for _, g := range Groups(orphans) {
		if max > 0 && listed == max {
			break
		}
		items = append(items, popup.Item{Title: g.Heading, Note: g.Action, Heading: true})
		for _, o := range g.Orphans {
			if max > 0 && listed == max {
				break
			}
			title := "•  " + o.Name
			if e := exception(o); e != "" {
				title += " (" + e + ")"
			}
			items = append(items, popup.Item{Title: title, Path: o.Path, Open: link(o.Path)})
			listed++
		}
	}
	if listed < len(orphans) {
		more = fmt.Sprintf("…and %d more.", len(orphans)-listed)
	}
	return items, more
}

// ApprovalMessage is the administrator's approval card: the same groups the
// user saw, each folder with its path and, in small print, what is stored on
// this PC; then where the files go and the space check.
func (p *Plan) ApprovalMessage() dialog.Message {
	stats := map[string]Item{}
	for _, it := range p.Items {
		stats[onedrive.Clean(it.Orphan.Path)] = it
	}
	var items []popup.Item
	for _, g := range Groups(p.Orphans()) {
		items = append(items, popup.Item{Title: g.Heading, Note: g.Action, Heading: true})
		for _, o := range g.Orphans {
			it := stats[onedrive.Clean(o.Path)]
			note := plural(it.Stats.LocalFiles, "file", "files") + " on this PC (" + HumanBytes(it.Stats.LocalBytes) + ")"
			if e := exception(o); e != "" {
				note += " · " + e
			}
			if n := len(it.Stats.Errors); n > 0 {
				note += " · " + plural(n, "item", "items") + " could not be read, so this folder will NOT be removed"
			}
			items = append(items, popup.Item{Title: "•  " + o.Name, Path: o.Path, Note: note, Open: link(o.Path)})
		}
	}
	if len(p.Excluded) > 0 {
		items = append(items, popup.Item{Title: "Skipped (fix these by hand)", Heading: true})
		for _, e := range p.Excluded {
			items = append(items, popup.Item{Title: "•  " + e})
		}
	}
	for _, i := range p.Interrupted {
		items = append(items, popup.Item{Title: "An earlier fix did not finish", Heading: true}, popup.Item{Title: strings.TrimSpace(i.Text())})
	}
	items = append(items, savedTo("Files will be saved to", p.BackupDir))
	return dialog.Message{
		Heading: "Administrator approval: fix these folders?",
		Text:    "User: " + p.Target.UserName,
		Items:   items,
		Footer:  fmt.Sprintf("Space needed: %s  ·  Available: %s", HumanBytes(int64(p.Required)), HumanBytes(int64(p.FreeBytes))),
		Kind:    dialog.KindWarning, Buttons: []string{"Cancel", "Approve"}, Primary: 1, Wide: true,
	}
}
