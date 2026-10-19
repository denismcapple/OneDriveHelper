package agent

import (
	"time"

	"golang.org/x/sys/windows/registry"

	"github.com/denismcapple/OneDriveHelper/internal/config"
	"github.com/denismcapple/OneDriveHelper/internal/onedrive"
	"github.com/denismcapple/OneDriveHelper/internal/winapi"
)

// Ignore ("snooze") state is kept per alert under
// HKCU\Software\OneDriveHelper\State\Snooze\<alert id>.
const snoozePath = `Software\OneDriveHelper\State\Snooze\`

// bootTolerance allows for clock drift when comparing boot times.
const bootTolerance = 2 * time.Minute

// SetSnooze records that the user chose to ignore one alert. d is a token
// from config.IgnoreChoices ("5m", "2h", "reboot").
func SetSnooze(h onedrive.Hive, alert, d string, now time.Time) error {
	dur, err := config.ParseIgnore(d)
	if err != nil {
		return err
	}
	k, err := h.Create(snoozePath + alert)
	if err != nil {
		return err
	}
	defer k.Close()
	_ = k.DeleteValue("Until")
	_ = k.DeleteValue("BootTime")
	if dur == 0 {
		return k.SetQWordValue("BootTime", uint64(winapi.BootTime().Unix()))
	}
	return k.SetQWordValue("Until", uint64(now.Add(dur).Unix()))
}

// ClearSnooze removes the ignore state of the given alerts (all if none given).
func ClearSnooze(h onedrive.Hive, alerts ...string) {
	if len(alerts) == 0 {
		alerts = []string{idNotRunning, idOrphans}
	}
	for _, a := range alerts {
		_ = registry.DeleteKey(h.Root, h.Prefix+snoozePath+a)
	}
}

// SnoozeInfo describes an alert's ignore state.
type SnoozeInfo struct {
	Active      bool
	Until       time.Time // zero when ignored until restart
	UntilReboot bool
}

// Snooze returns the ignore state of an alert.
func Snooze(h onedrive.Hive, alert string, now, boot time.Time) SnoozeInfo {
	k, err := h.Open(snoozePath+alert, registry.QUERY_VALUE)
	if err != nil {
		return SnoozeInfo{}
	}
	defer k.Close()
	until, _, _ := k.GetIntegerValue("Until")
	bootAt, _, _ := k.GetIntegerValue("BootTime")
	return snoozeState(until, bootAt, now, boot)
}

// Snoozed reports whether an alert is currently ignored.
func Snoozed(h onedrive.Hive, alert string, now, boot time.Time) bool {
	return Snooze(h, alert, now, boot).Active
}

func snoozeState(until, bootAt uint64, now, boot time.Time) SnoozeInfo {
	if until != 0 && now.Before(time.Unix(int64(until), 0)) {
		return SnoozeInfo{Active: true, Until: time.Unix(int64(until), 0)}
	}
	if bootAt != 0 {
		d := boot.Sub(time.Unix(int64(bootAt), 0))
		if d < bootTolerance && d > -bootTolerance {
			return SnoozeInfo{Active: true, UntilReboot: true}
		}
	}
	return SnoozeInfo{}
}

// Describe renders "until 15:30" / "until the next restart".
func (s SnoozeInfo) Describe() string {
	if s.UntilReboot {
		return "until the next restart"
	}
	return "until " + s.Until.Format("15:04")
}
