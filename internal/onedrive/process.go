package onedrive

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Proc is a running process.
type Proc struct {
	PID, ParentPID, Session uint32
	Name                    string
}

// Processes returns all running processes.
func Processes() ([]Proc, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	var out []Proc
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		p := Proc{PID: pe.ProcessID, ParentPID: pe.ParentProcessID, Name: windows.UTF16ToString(pe.ExeFile[:])}
		_ = windows.ProcessIdToSessionId(p.PID, &p.Session)
		out = append(out, p)
	}
	if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		err = nil
	}
	return out, err
}

// ProcState is whether a process is running. A failed process listing is
// Unknown, never "not running".
type ProcState int

const (
	StateUnknown ProcState = iota
	StateRunning
	StateNotRunning
)

func (s ProcState) String() string {
	switch s {
	case StateRunning:
		return "running"
	case StateNotRunning:
		return "not running"
	}
	return "unknown"
}

// RunningInSession reports whether a process named exe runs in the session.
func RunningInSession(exe string, session uint32) (ProcState, error) {
	procs, err := Processes()
	if err != nil {
		return StateUnknown, err
	}
	for _, p := range procs {
		if p.Session == session && strings.EqualFold(p.Name, exe) {
			return StateRunning, nil
		}
	}
	return StateNotRunning, nil
}

// OneDriveState reports whether OneDrive.exe is running in the session.
func OneDriveState(session uint32) ProcState {
	s, _ := RunningInSession("OneDrive.exe", session)
	return s
}

// OneDriveStateForUser reports whether OneDrive.exe runs for the user with
// the given SID, in any session (so a dry run from session 0, or the elevated
// approval step, sees the right user). A failed listing is Unknown. Processes
// whose owner cannot be read are another user's (a standard user cannot open
// other users' processes; their own always can be).
func OneDriveStateForUser(sid string) ProcState {
	procs, err := Processes()
	if err != nil {
		return StateUnknown
	}
	for _, p := range procs {
		if !strings.EqualFold(p.Name, "OneDrive.exe") {
			continue
		}
		if owner, err := processOwner(p.PID); err == nil && strings.EqualFold(owner, sid) {
			return StateRunning
		}
	}
	return StateNotRunning
}

func processOwner(pid uint32) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	var tok windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok); err != nil {
		return "", err
	}
	defer tok.Close()
	u, err := tok.GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String(), nil
}

// UpdateInProgress reports whether a OneDrive installer/updater is running.
func UpdateInProgress() bool {
	procs, _ := Processes()
	for _, p := range procs {
		switch strings.ToLower(p.Name) {
		case "onedrivesetup.exe", "onedrivestandaloneupdater.exe":
			return true
		}
	}
	return false
}

// OneDriveExe locates OneDrive.exe for the user.
func OneDriveExe(h Hive) string {
	candidates := []string{h.String(`Software\Microsoft\OneDrive`, "OneDriveTrigger")}
	for _, env := range []string{"LOCALAPPDATA", "ProgramFiles", "ProgramFiles(x86)"} {
		if base := os.Getenv(env); base != "" {
			sub := `Microsoft OneDrive\OneDrive.exe`
			if env == "LOCALAPPDATA" {
				sub = `Microsoft\OneDrive\OneDrive.exe`
			}
			candidates = append(candidates, filepath.Join(base, sub))
		}
	}
	for _, c := range candidates {
		// The registry value is user-writable; only ever start a file called OneDrive.exe.
		if c == "" || !strings.EqualFold(filepath.Base(c), "OneDrive.exe") {
			continue
		}
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return ""
}

// LaunchOneDrive starts OneDrive for the current user in the background.
func LaunchOneDrive(h Hive) error {
	exe := OneDriveExe(h)
	if exe == "" {
		return errors.New("OneDrive.exe could not be found")
	}
	cmd := exec.Command(exe, "/background")
	cmd.Dir = filepath.Dir(exe)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
