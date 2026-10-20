package config

import (
	"errors"
	"fmt"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/denismcapple/OneDriveHelper/internal/logx"
)

// LogoName is the logo's file name next to a settings file and in
// %ProgramData%\OneDriveHelper.
const LogoName = "logo.png"

// Logo limits (internal/popup refuses to draw anything bigger, too).
const (
	maxLogoBytes  = 2 << 20
	maxLogoWidth  = 2000
	maxLogoHeight = 1000
)

// InstalledLogo is where a PC keeps its logo.
func InstalledLogo() string { return filepath.Join(logx.ProgramDataDir(), LogoName) }

// LogoPath is the logo file to show: LogoFile if it is an absolute path,
// otherwise that name in %ProgramData%\OneDriveHelper. "" = no logo.
func (c Config) LogoPath() string {
	switch {
	case c.LogoFile == "":
		return ""
	case filepath.IsAbs(c.LogoFile):
		return c.LogoFile
	}
	return filepath.Join(logx.ProgramDataDir(), c.LogoFile)
}

// CheckLogo checks that path is a PNG within the limits and returns its size.
func CheckLogo(path string) (int, int, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, errors.New("file not found")
		}
		return 0, 0, err
	}
	if fi.IsDir() {
		return 0, 0, errors.New("is a folder, not a PNG file")
	}
	if fi.Size() > maxLogoBytes {
		return 0, 0, errors.New("must be 2 MB or smaller")
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	cfg, err := png.DecodeConfig(f)
	if err != nil {
		return 0, 0, errors.New("is not a PNG image")
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > maxLogoWidth || cfg.Height > maxLogoHeight {
		return cfg.Width, cfg.Height, fmt.Errorf("is %d x %d pixels; it must be at most %d x %d", cfg.Width, cfg.Height, maxLogoWidth, maxLogoHeight)
	}
	return cfg.Width, cfg.Height, nil
}

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// copyLogo checks src and copies it to dst through a temporary file, so dst
// is never half-written. Copying a file onto itself does nothing.
func copyLogo(src, dst string) error {
	if samePath(src, dst) {
		return nil
	}
	if _, _, err := CheckLogo(src); err != nil {
		return fmt.Errorf("%s %w", src, err)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, io.LimitReader(in, maxLogoBytes+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// installLogo copies c's logo to InstalledLogo and points c at it (or removes
// the installed logo when c has none). Requires admin.
func installLogo(c *Config) error {
	if c.LogoFile == "" {
		if err := os.Remove(InstalledLogo()); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove the old logo: %w", err)
		}
		return nil
	}
	if !samePath(c.LogoPath(), InstalledLogo()) {
		if err := logx.PrepareProgramDataDir(); err != nil {
			return fmt.Errorf("prepare %s: %w", logx.ProgramDataDir(), err)
		}
		if err := copyLogo(c.LogoPath(), InstalledLogo()); err != nil {
			return fmt.Errorf("logo: %w", err)
		}
	}
	c.LogoFile = LogoName
	return nil
}
