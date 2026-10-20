package remediate

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

// ReportName is the recovery report saved for a technician when a fix
// leaves a folder out of place: in the backup run folder (it stays after
// the staging folder is cleaned up) and in the staging run folder.
const ReportName = "RECOVERY-REPORT.txt"

// writeReport writes the report for j into each of dirs (skipping empty or
// missing ones) and returns the first path written ("" if none could be).
func writeReport(j *Journal, lg *log.Logger, dirs ...string) string {
	text := strings.ReplaceAll(reportText(j, time.Now()), "\n", "\r\n")
	first := ""
	for _, d := range dirs {
		if d == "" || !exists(d) {
			continue
		}
		p := filepath.Join(d, ReportName)
		if err := os.WriteFile(winapi.LongPath(p), []byte(text), 0o644); err != nil {
			if lg != nil {
				lg.Printf("write %s: %v", p, err)
			}
			continue
		}
		if first == "" {
			first = p
		}
	}
	return first
}

// reportText explains, per folder, where it is now, where its backup is and
// which copy holds the user's latest local edits.
func reportText(j *Journal, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "OneDrive Helper - recovery report\n=================================\n\n")
	fmt.Fprintf(&b, "User:       %s\nFix run:    %s (started %s)\nBackups:    %s\nUpdated:    %s\n\n",
		j.User, j.RunID, j.Started.Local().Format("2006-01-02 15:04:05"), j.RunDir, now.Local().Format("2006-01-02 15:04:05"))
	b.WriteString("A fix did not finish cleanly. Nothing was deleted that is not also in the backup.\n" +
		"For each folder below: where it is now, where its backup is, and which copy to use.\n")
	for i, e := range j.Entries {
		fmt.Fprintf(&b, "\n%d. %s\n   State:            %s\n", i+1, e.Source, e.State)
		where, use := "", ""
		switch e.State {
		case StateDone:
			where = "removed after every file was saved"
			use = "The backup (" + e.Backup + ") holds everything that was stored on this PC."
		case StateRestored:
			where = "put back in its original place (" + e.Source + ")"
			use = "Use the folder in its original place: it is the user's own copy, including any edits. The backup may be incomplete."
		case StateSkipped:
			where = "untouched, in its original place"
			use = "Nothing was changed."
		case StateMoving, StateStaged:
			where = "in the staging folder (" + e.Staged + "), unless it never moved"
			use = "The folder in staging is the user's own copy, including any edits. The backup was not finished, so do not rely on it."
		case StateCopied, StateKept:
			where = "kept in the staging folder (" + e.Staged + ")"
			use = "The folder in staging is the user's own copy, including any edits. The backup is a copy taken during the fix; the staging copy is never older."
		case StateDeleting, StatePartial:
			where = "partly removed; what is left is in " + e.Staged
			use = "The backup (" + e.Backup + ") is complete: every file was verified there before anything was deleted. What is left in staging is identical to the backup."
		}
		fmt.Fprintf(&b, "   Folder is now:    %s\n   Backup copy:      %s\n   Use:              %s\n", where, e.Backup, use)
		if e.Note != "" {
			fmt.Fprintf(&b, "   Note:             %s\n", e.Note)
		}
	}
	b.WriteString("\nTo put a folder back by hand: make sure OneDrive is not syncing that location, then move the\n" +
		"folder from staging to its original place (or copy the backup there). Keep this report with the ticket.\n")
	return b.String()
}
