package service

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/elog"
	"github.com/denismcapple/OneDriveHelper/internal/logx"
	"github.com/denismcapple/OneDriveHelper/internal/version"
)

// InstallDir is %ProgramFiles%\OneDriveHelper.
func InstallDir() string {
	pf := os.Getenv("ProgramFiles")
	if pf == "" {
		pf = `C:\Program Files`
	}
	return filepath.Join(pf, "OneDriveHelper")
}

// Install copies the binaries to Program Files, registers the service and
// event source, applies the optional JSON settings file, fills in default
// settings and starts the service.
func Install(configFile string, logf func(string, ...any)) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	srcDir, dir := filepath.Dir(self), InstallDir()
	if _, err := os.Stat(filepath.Join(srcDir, version.AgentExe)); err != nil {
		return fmt.Errorf("%s must be next to %s", version.AgentExe, filepath.Base(self))
	}
	if configFile != "" { // validate before changing anything
		if _, _, err := config.ReadFile(configFile); err != nil {
			return fmt.Errorf("settings file %s: %w", configFile, err)
		}
	}

	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	// Never interrupt a fix that is moving or deleting a user's files: wait
	// for it first (up to fixWaitCap), before stopping anything.
	w := realWaitEnv(logf)
	deadline := w.now().Add(fixWaitCap)
	legacy := installedPredatesFixLock()
	w.waitForFix(deadline)
	existing, err := m.OpenService(version.ServiceName)
	if err == nil {
		logf("Stopping existing service for upgrade...")
		_ = stopAndWait(existing)
		existing.Close()
	}
	w.stopAgents(deadline, legacy)

	if !strings.EqualFold(filepath.Clean(srcDir), filepath.Clean(dir)) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		for _, f := range []string{version.ServiceExe, version.AgentExe, version.ConfigExe} {
			if _, err := os.Stat(filepath.Join(srcDir, f)); err != nil && f == version.ConfigExe {
				continue // the config tool is optional
			}
			if err := copyWithRetry(filepath.Join(srcDir, f), filepath.Join(dir, f)); err != nil {
				return fmt.Errorf("copy %s: %w", f, err)
			}
			_ = os.Remove(filepath.Join(dir, f+".old")) // left by an earlier upgrade
		}
		logf("Copied binaries to %s", dir)
	}
	svcExe := filepath.Join(dir, version.ServiceExe)
	// Always quote the image path (mgr.CreateService only quotes paths that
	// contain spaces) so it can never be parsed as an "unquoted service path".
	binPath := QuotePath(svcExe)

	s, err := m.OpenService(version.ServiceName)
	if err != nil {
		s, err = m.CreateService(version.ServiceName, svcExe, mgr.Config{StartType: mgr.StartAutomatic})
		if err != nil {
			return fmt.Errorf("create service: %w", err)
		}
		logf("Created service %s", version.ServiceName)
	}
	defer s.Close()
	cur, err := s.Config()
	if err != nil {
		return fmt.Errorf("read service config: %w", err)
	}
	cur.BinaryPathName = binPath
	cur.DisplayName = version.ProductName
	cur.Description = "Monitors OneDrive health and alerts the signed-in user when OneDrive is not running or a library has stopped syncing."
	cur.StartType, cur.DelayedAutoStart = mgr.StartAutomatic, true
	cur.ServiceStartName = "LocalSystem"
	if err := s.UpdateConfig(cur); err != nil {
		return fmt.Errorf("configure service: %w", err)
	}
	if got, err := s.Config(); err != nil || got.BinaryPathName != binPath {
		return fmt.Errorf("service image path is %q, expected %q", got.BinaryPathName, binPath)
	}
	logf("Service image path: %s", binPath)
	_ = s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}, 86400)

	if err := elog.Install(); err != nil {
		logf("Warning: event log source: %v", err)
	}
	if err := logx.PrepareProgramDataDir(); err != nil {
		logf("Warning: securing %s: %v", logx.ProgramDataDir(), err)
	}
	if configFile != "" {
		if err := config.Import(configFile); err != nil {
			return fmt.Errorf("apply settings file: %w", err)
		}
		logf("Applied settings from %s", configFile)
	}
	if old := config.RemoveObsolete(); len(old) > 0 {
		logf("Removed obsolete settings: %s", strings.Join(old, ", "))
	}
	if err := config.WriteDefaults(); err != nil {
		return fmt.Errorf("write default config: %w", err)
	}
	logf("Configuration: HKLM\\%s", config.KeyPath)
	if err := s.Start(); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	logf("Service started")
	return nil
}

// QuotePath wraps an executable path in double quotes.
func QuotePath(p string) string { return `"` + strings.Trim(p, `"`) + `"` }

// Uninstall stops and removes the service and event source.
// Configuration is kept unless purge is set.
func Uninstall(purge bool, logf func(string, ...any)) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	if s, err := m.OpenService(version.ServiceName); err == nil {
		w := realWaitEnv(logf)
		deadline := w.now().Add(fixWaitCap)
		legacy := installedPredatesFixLock()
		w.waitForFix(deadline)
		_ = stopAndWait(s)
		w.stopAgents(deadline, legacy)
		err = s.Delete()
		s.Close()
		if err != nil {
			return fmt.Errorf("delete service: %w", err)
		}
		logf("Service removed")
	} else {
		logf("Service not installed")
	}
	// No agent is left to recover unfinished fixes after an uninstall: warn.
	if left := unfinishedFixes(profileDirs()); len(left) > 0 {
		msg := "These OneDrive Helper fixes did not finish. The folders are not where they were and must be restored by hand " +
			"(each run folder holds the folder and a manifest.json; the backups are in the user's ODHelper\\Saved folder):\r\n" + strings.Join(left, "\r\n")
		logf("WARNING: %s", strings.ReplaceAll(msg, "\r\n", "\n  "))
		ev := elog.Open()
		ev.Warning(elog.FixInterrupted, msg)
		ev.Close()
	}
	_ = elog.Remove()
	if purge {
		_ = deleteTree(registry.LOCAL_MACHINE, config.KeyPath)
		logf("Configuration removed")
	}
	logf("Program files left in %s (delete once agents have exited)", InstallDir())
	return nil
}

// Start / Stop the installed service.
func Start() error {
	return withService(func(s *mgr.Service) error { return s.Start() })
}

func Stop() error { return withService(stopAndWait) }

func withService(fn func(*mgr.Service) error) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(version.ServiceName)
	if err != nil {
		return err
	}
	defer s.Close()
	return fn(s)
}

func stopAndWait(s *mgr.Service) error {
	st, err := s.Control(svc.Stop)
	if err != nil {
		return err
	}
	for deadline := time.Now().Add(30 * time.Second); st.State != svc.Stopped; {
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for the service to stop")
		}
		time.Sleep(300 * time.Millisecond)
		if st, err = s.Query(); err != nil {
			return err
		}
	}
	return nil
}

func deleteTree(root registry.Key, path string) error {
	k, err := registry.OpenKey(root, path, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return err
	}
	subs, _ := k.ReadSubKeyNames(-1)
	k.Close()
	for _, s := range subs {
		_ = deleteTree(root, path+`\`+s)
	}
	return registry.DeleteKey(root, path)
}

func copyWithRetry(src, dst string) error {
	var err error
	for i := 0; i < 20; i++ { // agents may hold the old exe open for a few seconds
		if err = copyFile(src, dst); err == nil {
			return nil
		}
		// A running image can be renamed even though it cannot be overwritten.
		_ = os.Rename(dst, dst+".old")
		time.Sleep(time.Second)
	}
	return err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
