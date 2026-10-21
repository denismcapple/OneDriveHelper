package remediate

import (
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/elog"
	"github.com/denismcapple/OneDriveHelper/internal/ipc"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
	"github.com/denismcapple/OneDriveHelper/internal/popup"
	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

func TestApprovalDeadlineAlreadyPassed(t *testing.T) {
	now := time.Now()
	shown := false
	code, ev := decideApproval(now.Add(-time.Second), func() time.Time { return now }, func(time.Duration) (bool, bool) {
		shown = true
		return true, false
	})
	if code != ExitCancelled || ev != elog.ApprovalTimedOut || shown {
		t.Errorf("code %d event %d shown %v", code, ev, shown)
	}
}

func TestApprovalDialogLimitBeforeDeadline(t *testing.T) {
	saved := approvalDialogLimit
	approvalDialogLimit = 50 * time.Millisecond
	defer func() { approvalDialogLimit = saved }()
	var gotLimit time.Duration
	code, ev := decideApproval(time.Now().Add(time.Hour), time.Now, func(limit time.Duration) (bool, bool) {
		gotLimit = limit
		return false, true // the dialog closed itself
	})
	if code != ExitCancelled || ev != elog.ApprovalTimedOut || gotLimit != 50*time.Millisecond {
		t.Errorf("code %d event %d limit %s", code, ev, gotLimit)
	}
}

func TestApprovalLimitIsCappedByDeadline(t *testing.T) {
	var gotLimit time.Duration
	decideApproval(time.Now().Add(time.Minute), time.Now, func(limit time.Duration) (bool, bool) {
		gotLimit = limit
		return true, false
	})
	if gotLimit > time.Minute || gotLimit < 55*time.Second {
		t.Errorf("limit %s should be the time left to the deadline", gotLimit)
	}
}

func TestApprovalOutcomes(t *testing.T) {
	if code, ev := decideApproval(time.Now().Add(time.Hour), time.Now, func(time.Duration) (bool, bool) { return true, false }); code != ExitApproved || ev != elog.RemediationStarted {
		t.Errorf("approved: %d %d", code, ev)
	}
	if code, ev := decideApproval(time.Now().Add(time.Hour), time.Now, func(time.Duration) (bool, bool) { return false, false }); code != ExitCancelled || ev != elog.RemediationCancelled {
		t.Errorf("declined: %d %d", code, ev)
	}
	// OK pressed after the deadline: never 5001.
	start := time.Now()
	late := func() time.Time { return start.Add(2 * time.Hour) }
	calls := 0
	clock := func() time.Time {
		calls++
		if calls > 1 {
			return late()
		}
		return start
	}
	if code, ev := decideApproval(start.Add(time.Hour), clock, func(time.Duration) (bool, bool) { return true, false }); code != ExitCancelled || ev != elog.ApprovalTimedOut {
		t.Errorf("late approval: %d %d", code, ev)
	}
}

func TestParseDeadline(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	if got := ParseDeadline("1800000600", start); !got.Equal(time.Unix(1_800_000_600, 0)) {
		t.Errorf("valid: %v", got)
	}
	for _, bad := range []string{"", "soon", "-5"} {
		if got := ParseDeadline(bad, start); !got.Equal(start.Add(approvalDeadline)) {
			t.Errorf("%q: %v", bad, got)
		}
	}
}

// A panic inside Execute (after the folder was staged) unwinds UserFlow:
// the fix lock is released, nothing more runs, and recovery puts the folder back.
func TestPanicDuringExecuteReleasesLockAndIsRecoverable(t *testing.T) {
	e, _ := testEnv(t)
	f := newFixture(t, three)
	e.beforeCheck = func(string) { panic("crash after copying") }
	saved := flow
	defer func() { flow = saved }()
	flow = defaultFlowEnv()
	flow.target = func() (Target, error) { return f.plan.Target, nil }
	flow.recover = func(Target, *log.Logger) {}
	flow.plan = func(Target, config.Config) (*Plan, error) { return f.plan, nil }
	flow.elevate = func(string, []string, time.Time) (uint32, error, <-chan struct{}) {
		done := make(chan struct{})
		close(done)
		return ExitApproved, nil, done
	}
	resynced := false
	flow.resync = func([]onedrive.Orphan, *log.Logger) string { resynced = true; return "" }
	flow.info, flow.warn, flow.fail = func(string, string) {}, func(string, string) {}, func(string, string) {}
	flow.confirm = func(string, []popup.Item, string) bool { return true }
	func() {
		defer func() { recover() }()
		UserFlow("agent.exe", discard, func(bool) {}, func() bool { return false })
	}()
	if ipc.FixInProgress() {
		t.Error("the fix lock must be released by the panic unwinding")
	}
	if resynced {
		t.Error("nothing may run after the panic")
	}
	*e = defaultEnvForRecovery(e)
	if left := Recover(f.plan.Target, discard, nil); len(left) != 0 || len(readAll(t, f.lib())) != 3 {
		t.Errorf("recovery should put the folder back: left %+v, files %v", left, readAll(t, f.lib()))
	}
}

// Old copies and folders from a disconnected account are saved and removed
// but never synced again; the confirmation must not promise a re-sync.
func TestConfirmationForFoldersThatAreNotResynced(t *testing.T) {
	lib := &onedrive.Library{UserEmail: "d@contoso.com"}
	moved := onedrive.Orphan{Name: "Internal - Documents", Path: `C:\Users\d\OLD\Internal - Documents`, Kind: onedrive.KindMoved,
		MovedTo: `C:\Users\d\New\Internal - Documents`, Library: lib, WebURL: "https://contoso.sharepoint.com/sites/internal"}
	gone := onedrive.Orphan{Name: "Shared - Documents", Path: `C:\Users\d\Northwind\Shared - Documents`, Kind: onedrive.KindDisconnected, Library: lib}
	stopped := onedrive.Orphan{Name: "Bids - Documents", Path: `C:\Users\d\New\Bids - Documents`, Kind: onedrive.KindStopped, Library: lib}
	for _, o := range []onedrive.Orphan{moved, gone} {
		if o.NeedsResync() || strings.Contains(ResyncNote(o), "synced again automatically") {
			t.Errorf("%s must not be re-synced: %q", o.Name, ResyncNote(o))
		}
	}
	if !strings.Contains(ResyncNote(moved), moved.MovedTo) || !strings.Contains(ResyncNote(gone), "no longer connected") {
		t.Errorf("notes: %q / %q", ResyncNote(moved), ResyncNote(gone))
	}
	plan := &Plan{Target: Target{UserName: "d"}, BackupDir: `C:\Users\d\ODHelper\Saved`,
		Items: []Item{{Orphan: moved, Stats: Stats{LocalFiles: 9, LocalBytes: 4 << 20}}, {Orphan: stopped}}}
	p := plan.Summary()
	for _, want := range []string{"Old copies: these libraries now sync to \"New\" (1)", ActionBackup, moved.Path, "9 file(s)",
		"Stopped syncing (1)", ActionResync} {
		if !strings.Contains(p, want) {
			t.Errorf("summary missing %q:\n%s", want, p)
		}
	}
	// The approval card: section headers, then each folder's name with its
	// path and size in small print. "Now syncs to" isn't repeated per folder.
	a := plan.ApprovalMessage()
	var lines []string
	for _, it := range a.Items {
		lines = append(lines, it.Title+"|"+it.Path+"|"+it.Note)
	}
	got := strings.Join(lines, "\n")
	for _, want := range []string{
		"Old copies: these libraries now sync to \"New\" (1)||" + ActionBackup,
		"•  Internal - Documents|" + moved.Path + "|9 files on this PC (4.0 MB)",
		"Files will be saved to|" + plan.BackupDir + "|",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("approval items missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(a.PlainText(), "now syncs to") || a.Text != "User: d" || a.Buttons[1] != "Approve" {
		t.Errorf("approval card:\n%s", a.PlainText())
	}

	confirmation := func(orphans ...onedrive.Orphan) ([]popup.Item, string) {
		saved := flow
		defer func() { flow = saved }()
		var items []Item
		for _, o := range orphans {
			items = append(items, Item{Orphan: o})
		}
		var shown []popup.Item
		footer := ""
		flow = flowEnv{
			target:  func() (Target, error) { return Target{UserName: "d", SID: "S-1-5-21-1"}, nil },
			recover: func(Target, *log.Logger) {},
			plan: func(Target, config.Config) (*Plan, error) {
				return &Plan{Items: items, FreeBytes: 10, Required: 1, BackupDir: `C:\Users\d\ODHelper\Saved`}, nil
			},
			confirm: func(_ string, it []popup.Item, f string) bool { shown, footer = it, f; return false },
			warn:    func(string, string) {}, fail: func(string, string) {}, info: func(string, string) {},
		}
		UserFlow("agent.exe", discard, func(bool) {}, func() bool { return false })
		return shown, footer
	}
	items, footer := confirmation(moved, gone)
	var notes []string
	for _, it := range items {
		if it.Heading {
			notes = append(notes, it.Note)
		}
	}
	last := items[len(items)-1]
	if strings.Join(notes, "|") != ActionBackup+"|"+ActionBackup || last.Title != "Your files are saved to" || !last.Plain ||
		last.Path != `C:\Users\d\ODHelper\Saved` || footer != "Nothing is deleted until a copy is saved.\nAn administrator needs to approve this." {
		t.Errorf("confirmation: %v / %+v / %q", notes, last, footer)
	}
	if items, _ := confirmation(moved, stopped); items[0].Note != ActionResync {
		t.Errorf("re-sync expected for the stopped library: %+v", items)
	}
}

// Each folder is listed once: its name, and its path as a link that opens
// it, as long as the folder exists.
func TestItemsListEachFolderOnceWithAnOpenLink(t *testing.T) {
	savedOpen, savedExists := openFolder, folderExists
	defer func() { openFolder, folderExists = savedOpen, savedExists }()
	var opened []string
	openFolder = func(p string) { opened = append(opened, p) }
	folderExists = func(p string) bool { return !strings.HasSuffix(p, `\A - Documents`) }
	items, more := Items([]onedrive.Orphan{
		{Name: "A - Documents", Path: `C:\Users\d\Org\A - Documents`, Kind: onedrive.KindStopped, WebURL: "https://org.sharepoint.com/sites/a"},
		{Name: "B - Documents", Path: `C:\Users\d\Org\B - Documents`, Kind: onedrive.KindStopped},
		{Name: "Docs", Path: `C:\Users\d\Org\Docs`, KnownFolders: []string{"Documents"}},
	}, 2)
	if len(items) != 3 || !items[0].Heading || items[0].Note != ActionResync || more != "…and 1 more." {
		t.Fatalf("items: %+v %q", items, more)
	}
	if items[1].Title != "•  A - Documents (click 'Sync' on its SharePoint page afterwards)" || items[1].Note != "" ||
		items[2].Title != "•  B - Documents (your support team will add it back)" {
		t.Errorf("exceptions: %q / %q", items[1].Title, items[2].Title)
	}
	if items[1].Open != nil {
		t.Error("a folder that doesn't exist is not a link")
	}
	items[2].Open()
	if len(opened) != 1 || opened[0] != `C:\Users\d\Org\B - Documents` {
		t.Errorf("opened %v", opened)
	}
}

func TestResultMessage(t *testing.T) {
	cfg := config.Defaults()
	cfg.SupportName = "the Service Desk"
	ok := &Result{RunDir: `C:\Users\d\ODHelper\Saved\2026-10-21_1401`, Outcomes: []Outcome{
		{Path: `C:\Users\d\Org\A`, State: StateDone, Removed: true}, {Path: `C:\Users\d\Org\B`, State: StateDone, Removed: true}}}
	m := resultMessage(ok, cfg, resyncText([]string{"A"}, nil, nil))
	if m.Heading != "Folders fixed" || m.Text != "2 folders were moved to your backup folder." || m.Footer != "" || len(m.Items) != 2 ||
		m.Items[0].Title != "OneDrive has been asked to sync A again (confirm if it asks)." ||
		m.Items[1].Title != "Your saved files are in" || m.Items[1].Path != ok.RunDir {
		t.Errorf("success card: %+v", m)
	}
	part := &Result{RunDir: ok.RunDir, Outcomes: []Outcome{
		{Path: `C:\Users\d\Org\A`, State: StateDone, Removed: true},
		{Path: `C:\Users\d\Org\B`, State: StateSkipped, Error: "Microsoft Excel has a file in it open"}}}
	m = resultMessage(part, cfg, "")
	b := m.Items[0]
	if m.Text != "1 folder was moved to your backup folder." || b.Title != "•  B - not changed" || b.Path != `C:\Users\d\Org\B` ||
		b.Note != "Microsoft Excel has a file in it open" || !b.StrongNote || !strings.Contains(m.Footer, "the Service Desk") {
		t.Errorf("partial card: %+v", m)
	}
	if strings.Contains(m.PlainText(), `C:\Users\d\Org\A`) {
		t.Errorf("fixed folders are counted, not listed:\n%s", m.PlainText())
	}
	if got := joinNames([]string{"A", "B", "C"}); got != "A, B and C" {
		t.Errorf("joinNames: %q", got)
	}
}

// UserFlow on an approval timeout: the user is told nothing changed, Execute
// is not called, and UserFlow (so the agent's "fixing" state) lasts until the
// stale UAC call returns.
func TestUserFlowApprovalTimeout(t *testing.T) {
	saved := flow
	defer func() { flow = saved }()
	var mu sync.Mutex
	var infos []string
	executed := false
	stale := make(chan struct{})
	flow = flowEnv{
		target:  func() (Target, error) { return Target{UserName: "jsmith", SID: "S-1-5-21-1"}, nil },
		recover: func(Target, *log.Logger) {},
		plan: func(Target, config.Config) (*Plan, error) {
			return &Plan{Items: []Item{{Orphan: onedrive.Orphan{Path: `C:\Users\jsmith\Contoso\Lib`, Name: "Lib"}}},
				FreeBytes: 10, Required: 1}, nil
		},
		elevate: func(string, []string, time.Time) (uint32, error, <-chan struct{}) {
			return 0, winapi.ErrTimedOut, stale
		},
		execute: func(*Plan, *log.Logger, *elog.Log, func() bool) (*Result, error) {
			executed = true
			return &Result{}, nil
		},
		resync:  func([]onedrive.Orphan, *log.Logger) string { return "" },
		info:    func(_, m string) { mu.Lock(); infos = append(infos, m); mu.Unlock() },
		warn:    func(_, m string) {},
		fail:    func(_, m string) {},
		confirm: func(string, []popup.Item, string) bool { return true },
	}
	done := make(chan struct{})
	go func() {
		UserFlow("agent.exe", discard, func(bool) {}, func() bool { return false })
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("UserFlow must wait for the stale elevation to return")
	case <-time.After(200 * time.Millisecond):
	}
	close(stale)
	<-done
	mu.Lock()
	defer mu.Unlock()
	if executed || len(infos) != 1 || !strings.Contains(infos[0], "nothing was changed") {
		t.Errorf("executed=%v infos=%v", executed, infos)
	}
}
