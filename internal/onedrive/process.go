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

// IsRunningInSession reports whether a process named exe runs in the session.
func IsRunningInSession(exe string, session uint32) bool {
	procs, _ := Processes()
	for _, p := range procs {
		if p.Session == session && strings.EqualFold(p.Name, exe) {
			return true
		}
	}
	return false
}

// OneDriveRunning reports whether OneDrive.exe is running in the session.
func OneDriveRunning(session uint32) bool { return IsRunningInSession("OneDrive.exe", session) }

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
