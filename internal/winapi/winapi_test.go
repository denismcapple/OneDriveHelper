package winapi

import (
	"errors"
	"testing"
	"time"
)

func TestLogonIDIsStableWithinASession(t *testing.T) {
	a, err := LogonID()
	if err != nil || a == 0 {
		t.Fatalf("LogonID: %d %v", a, err)
	}
	if b, _ := LogonID(); b != a {
		t.Errorf("the logon ID must not change within a session: %d then %d", a, b)
	}
}

func TestWithDeadlineReturnsResult(t *testing.T) {
	code, err, done := WithDeadline(func() (uint32, error) { return 7, nil }, time.Now().Add(time.Second))
	if code != 7 || err != nil {
		t.Errorf("got %d %v", code, err)
	}
	<-done
}

func TestWithDeadlineTimesOutAndDiscardsLateResult(t *testing.T) {
	release := make(chan struct{})
	code, err, done := WithDeadline(func() (uint32, error) {
		<-release     // the UAC prompt is still up
		return 0, nil // approved, but too late
	}, time.Now().Add(50*time.Millisecond))
	if !errors.Is(err, ErrTimedOut) || code != 0 {
		t.Fatalf("got %d %v", code, err)
	}
	select {
	case <-done:
		t.Fatal("the stale call has not returned yet")
	default:
	}
	close(release)
	<-done // returns; its result changes nothing the caller saw
}
