package service

import (
	"errors"
	"fmt"
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
	if err := preflight(srcDir, filepath.Base(self), configFile); err != nil {
		return err
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

	// From here on, a failure puts the previous programs back and restarts the
	// service, so an upgrade never leaves a PC without a working version.
	// (Settings are validated above but not rolled back.)
	rollback := func() error { return nil }
	if !strings.EqualFold(filepath.Clean(srcDir), filepath.Clean(dir)) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		rb, err := upgrade(srcDir, dir, []string{version.ServiceExe, version.AgentExe, version.ConfigExe}, realFileOps())
		if err != nil {
			restartOld(m, logf)
			return fmt.Errorf("upgrade failed, previous version kept: %w", err)
		}
		rollback = rb
		logf("Copied binaries to %s", dir)
	}
	fail := func(err error) error {
		if rbErr := rollback(); rbErr != nil {
			logf("Warning: rolling back the programs failed: %v", rbErr)
		} else {
			logf("Rolled back to the previous programs")
		}
		restartOld(m, logf)
		return err
	}
	svcExe := filepath.Join(dir, version.ServiceExe)
	// Always quote the image path (mgr.CreateService only quotes paths that
	// contain spaces) so it can never be parsed as an "unquoted service path".
	binPath := QuotePath(svcExe)

	s, err := m.OpenService(version.ServiceName)
	if err != nil {
		s, err = m.CreateService(version.ServiceName, svcExe, mgr.Config{StartType: mgr.StartAutomatic})
		if err != nil {
			return fail(fmt.Errorf("create service: %w", err))
		}
		logf("Created service %s", version.ServiceName)
	}
	defer s.Close()
	cur, err := s.Config()
	if err != nil {
		return fail(fmt.Errorf("read service config: %w", err))
	}
	cur.BinaryPathName = binPath
	cur.DisplayName = version.ProductName
	cur.Description = "Monitors OneDrive health and alerts the signed-in user when OneDrive is not running or a library has stopped syncing."
	cur.StartType, cur.DelayedAutoStart = mgr.StartAutomatic, true
	cur.ServiceStartName = "LocalSystem"
	if err := s.UpdateConfig(cur); err != nil {
		return fail(fmt.Errorf("configure service: %w", err))
	}
	if got, err := s.Config(); err != nil || got.BinaryPathName != binPath {
		return fail(fmt.Errorf("service image path is %q, expected %q", got.BinaryPathName, binPath))
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
			return fail(fmt.Errorf("apply settings file: %w", err))
		}
		logf("Applied settings from %s", configFile)
	}
	if old := config.RemoveObsolete(); len(old) > 0 {
		logf("Removed obsolete settings: %s", strings.Join(old, ", "))
	}
	if err := config.WriteDefaults(); err != nil {
		return fail(fmt.Errorf("write default config: %w", err))
	}
	logf("Configuration: HKLM\\%s", config.KeyPath)
	if err := startAndWait(s); err != nil {
		return fail(fmt.Errorf("start service: %w", err))
	}
	logf("Service started")
	return nil
}

// preflight checks everything that can be checked before anything is stopped.
func preflight(srcDir, self, configFile string) error {
	if _, err := os.Stat(filepath.Join(srcDir, version.AgentExe)); err != nil {
		return fmt.Errorf("%s must be next to %s", version.AgentExe, self)
	}
	if configFile != "" { // validate before changing anything
		if _, _, err := config.ReadFile(configFile); err != nil {
			return fmt.Errorf("settings file %s: %w", configFile, err)
		}
	}
	return nil
}

// startAndWait starts the service and waits until it is running (an image
// that crashes at start-up counts as a failure, so the upgrade rolls back).
func startAndWait(s *mgr.Service) error {
	if err := s.Start(); err != nil {
		return err
	}
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		st, err := s.Query()
		if err != nil {
			return err
		}
		switch st.State {
		case svc.Running:
			return nil
		case svc.Stopped:
			return errors.New("the service stopped right after starting")
		}
	}
	return errors.New("timed out waiting for the service to start")
}

// restartOld starts the (previous) service again after a failed upgrade.
func restartOld(m *mgr.Mgr, logf func(string, ...any)) {
	s, err := m.OpenService(version.ServiceName)
	if err != nil {
		return // fresh install: nothing to restart
	}
	defer s.Close()
	if err := s.Start(); err != nil {
		logf("Warning: could not restart the previous service: %v", err)
	} else {
		logf("Previous service restarted")
	}
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
