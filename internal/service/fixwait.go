package service

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/denismcapple/OneDriveHelper/internal/elog"
	"github.com/denismcapple/OneDriveHelper/internal/ipc"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
	"github.com/denismcapple/OneDriveHelper/internal/version"
)

// fixWaitCap is how long install/upgrade/uninstall waits for a running fix
// before going ahead anyway (an RMM job must still finish).
var fixWaitCap = 30 * time.Minute

// agentGrace is how long agents of a version with the fix lock get to exit
// after the service has signalled them, when no fix is running.
var agentGrace = 20 * time.Second

// waitEnv is the outside world of the wait/stop logic (replaced in tests).
type waitEnv struct {
	fixInProgress func() bool
	agents        func() ([]uint32, error) // pids of running agents
	terminate     func(pid uint32) error
	now           func() time.Time
	sleep         func(time.Duration)
	logf          func(string, ...any)
	event         func(id uint32, warning bool, msg string)
}

func realWaitEnv(logf func(string, ...any)) waitEnv {
	ev := elog.Open()
	return waitEnv{
		fixInProgress: ipc.FixInProgress,
		agents:        runningAgents,
		terminate:     terminateProcess,
		now:           time.Now,
		sleep:         time.Sleep,
		logf:          logf,
		event: func(id uint32, warning bool, msg string) {
			if warning {
				ev.Warning(id, msg)
			} else {
				ev.Error(id, msg)
			}
		},
	}
}

// waitForFix waits, before anything is stopped, while a fix is in progress,
// up to deadline. It logs every minute and logs 1002 once.
func (w waitEnv) waitForFix(deadline time.Time) {
	if !w.fixInProgress() {
		return
	}
	w.logf("A OneDrive Helper fix is in progress; waiting for it to finish (up to %s)...", deadline.Sub(w.now()).Round(time.Minute))
	w.event(elog.InstallerWaitingForFix, true, "The installer is waiting for a OneDrive Helper fix in progress to finish before upgrading or removing the service.")
	lastLog := w.now()
	for w.fixInProgress() && w.now().Before(deadline) {
		w.sleep(2 * time.Second)
		if w.now().Sub(lastLog) >= time.Minute {
			lastLog = w.now()
			w.logf("Still waiting for the fix to finish...")
		}
	}
	if w.fixInProgress() {
		w.logf("The fix is still running after the wait limit; continuing anyway.")
	} else {
		w.logf("The fix has finished.")
	}
}

// stopAgents waits for user agents to exit after the service has stopped,
// then terminates any that are left. It keeps waiting while a fix is in
// progress (or, for versions that predate the fix lock, for the agents to exit
// on their own, which they do after finishing a fix), until deadline. A failed
// process listing is "unknown", never "no agents".
func (w waitEnv) stopAgents(deadline time.Time, legacy bool) {
	grace := w.now().Add(agentGrace)
	logged := false
	var pids []uint32
	for {
		var err error
		pids, err = w.agents()
		if err == nil && len(pids) == 0 {
			return
		}
		if !logged {
			logged = true
			switch {
			case err != nil:
				w.logf("Could not list running agents (%v); waiting...", err)
			case legacy:
				w.logf("Waiting for %d user agent(s) of the previous version to exit (they finish any fix in progress first)...", len(pids))
			default:
				w.logf("Waiting for %d user agent(s) to exit...", len(pids))
			}
		}
		limit := grace
		if w.fixInProgress() {
			// Give the agent its exit grace once the fix ends.
			grace = w.now().Add(agentGrace)
			limit = deadline
		} else if legacy || err != nil {
			limit = deadline
		}
		if limit.After(deadline) {
			limit = deadline
		}
		if !w.now().Before(limit) {
			break
		}
		w.sleep(500 * time.Millisecond)
	}
	interrupting := w.fixInProgress()
	for _, pid := range pids {
		if err := w.terminate(pid); err != nil {
			w.logf("Warning: could not stop agent process %d: %v", pid, err)
		} else {
			w.logf("Stopped agent process %d (it did not exit by itself)", pid)
		}
	}
	if interrupting {
		w.logf("A fix was still in progress; it was interrupted. The user's agent will restore or report it when it next starts.")
		w.event(elog.InstallerInterruptedFix, false, "The installer stopped a OneDrive Helper agent while a fix was in progress. The fix will be restored or reported when the agent next starts.")
	}
}

func runningAgents() ([]uint32, error) {
	procs, err := onedrive.Processes()
	if err != nil {
		return nil, err
	}
	var pids []uint32
	for _, p := range procs {
		if strings.EqualFold(p.Name, version.AgentExe) {
			pids = append(pids, p.PID)
		}
	}
	return pids, nil
}

func terminateProcess(pid uint32) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 1); err != nil {
		return err
	}
	_, _ = windows.WaitForSingleObject(h, 5000)
	return nil
}

// installedPredatesFixLock reports whether the installed version's agents
// don't create the fix lock (so the installer can't see a fix in progress and
// must wait for them to exit on their own). A missing install is "no".
func installedPredatesFixLock() bool {
	exe := filepath.Join(InstallDir(), version.ServiceExe)
	if _, err := os.Stat(exe); err != nil {
		return false
	}
	out, err := exec.Command(exe, "version").Output()
	if err != nil {
		return true // can't tell: be patient
	}
	return versionLess(strings.TrimSpace(string(out)), ipc.FixLockVersion)
}

// versionLess compares x.y.z versions; anything unparsable counts as older.
func versionLess(a, b string) bool {
	pa, okA := parseVersion(a)
	pb, _ := parseVersion(b)
	if !okA {
		return true
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func parseVersion(s string) ([3]int, bool) {
	var v [3]int
	parts := strings.Split(strings.TrimPrefix(s, "v"), ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// unfinishedFixes lists, read-only, fix runs in the given profiles that did
// not finish (folders not done, restored or skipped), and unreadable records.
// After an uninstall no agent is left to recover them, so the installer warns.
func unfinishedFixes(profiles []string) []string {
	var out []string
	for _, p := range profiles {
		runs, _ := filepath.Glob(filepath.Join(p, "ODHelper", "Fixing", "*", "manifest.json"))
		for _, m := range runs {
			b, err := os.ReadFile(m)
			var j struct {
				Folders []struct {
					Source, Staged, State string
				} `json:"folders"`
			}
			if err == nil {
				err = json.Unmarshal(b, &j)
			}
			if err != nil {
				out = append(out, fmt.Sprintf("%s (record unreadable: %v)", filepath.Dir(m), err))
				continue
			}
			for _, f := range j.Folders {
				switch f.State {
				case "done", "restored", "skipped":
				default:
					out = append(out, fmt.Sprintf("%s: %s is in %s (%s)", filepath.Dir(m), f.Source, f.Staged, f.State))
				}
			}
		}
	}
	return out
}

// profileDirs lists every local user profile folder.
func profileDirs() []string {
	const list = `SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList`
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, list, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil
	}
	sids, _ := k.ReadSubKeyNames(-1)
	k.Close()
	var out []string
	for _, sid := range sids {
		if d := onedrive.ProfileDir(sid); d != "" {
			out = append(out, d)
		}
	}
	return out
}
