package odstatus

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func currentSID(t *testing.T) string {
	t.Helper()
	tok := windows.GetCurrentProcessToken()
	u, err := tok.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return u.User.Sid.String()
}

// TestReadLive reads this PC's real OneDrive status (read only). It is
// skipped where Windows or OneDrive offers none.
func TestReadLive(t *testing.T) {
	roots, err := Read(currentSID(t), 15*time.Second)
	if errors.Is(err, ErrUnsupported) || (err == nil && len(roots) == 0) {
		t.Skip("no OneDrive status on this PC")
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range roots {
		t.Logf("%s (%s): state=%s status=%q message=%q action=%q err=%q", r.Name, r.Account(), r.State, r.Status, r.Message, r.Action, r.Err)
		if r.Err == "" && (r.State < InSync || r.State > Offline) {
			t.Errorf("%s: state %d out of range", r.Name, r.State)
		}
	}
}

func TestCallTimeoutDoesNotStackThreads(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	if err := call(50*time.Millisecond, func() { <-release }); !errors.Is(err, ErrTimeout) {
		t.Fatalf("a hung call must time out: %v", err)
	}
	start := time.Now()
	if err := call(time.Second, func() {}); !errors.Is(err, ErrTimeout) || time.Since(start) > 100*time.Millisecond {
		t.Errorf("while a call is stuck, the next one must fail at once: %v", err)
	}
}

func TestStateString(t *testing.T) {
	if Error.String() != "error" || State(-1).String() != "unknown" {
		t.Error(Error.String(), State(-1).String())
	}
}
