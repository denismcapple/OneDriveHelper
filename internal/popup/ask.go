package popup

import (
	"fmt"
	"runtime"
	"sync/atomic"
	"time"
)

var askSeq atomic.Int64

// Ask shows s and waits until one of its buttons is clicked. It returns the
// button's index (its Action, if any, still runs), or -1 without a UI.
// Every button closes the card, except Stay buttons, which only run their
// Action.
func (m *Manager) Ask(s Spec) int {
	i, _ := m.AskWithin(s, 0)
	return i
}

// AskWithin is Ask with a time limit (0 = none). After the limit the card
// closes and it returns -1, true.
//
// It may be called from a thread that owns windows (e.g. the settings
// window): their messages keep being handled while it waits. It must not be
// called on the popup UI thread itself (button actions run on their own
// goroutines, so they may call it).
func (m *Manager) AskWithin(s Spec, limit time.Duration) (int, bool) {
	if m == nil || len(s.Buttons) == 0 {
		return -1, false
	}
	if s.ID == "" {
		s.ID = fmt.Sprintf("ask-%d", askSeq.Add(1))
	}
	answer := make(chan int, 1)
	buttons := make([]Button, len(s.Buttons))
	for i, b := range s.Buttons {
		if b.Stay {
			b.Dismiss, b.Menu = false, nil
			buttons[i] = b
			continue
		}
		action := b.Action
		b.Dismiss, b.Menu = true, nil
		b.Action = func() {
			select {
			case answer <- i:
			default:
			}
			if action != nil {
				action()
			}
		}
		buttons[i] = b
	}
	s.Buttons = buttons
	m.Show(s)

	var deadline <-chan time.Time
	if limit > 0 {
		t := time.NewTimer(limit)
		defer t.Stop()
		deadline = t.C
	}
	runtime.LockOSThread() // nests with a caller's own lock
	defer runtime.UnlockOSThread()
	quit := false
	defer func() {
		if quit {
			call(pPostQuitMessage, 0) // hand WM_QUIT back to the caller's loop
		}
	}()
	for {
		select {
		case i := <-answer:
			return i, false
		case <-deadline:
			m.Hide(s.ID)
			return -1, true
		default:
		}
		call(pMsgWaitForMultipleObj, 0, 0, 50, qsAllInput, mwmoInputAvail)
		var q msg
		for call(pPeekMessageW, ptr(&q), 0, 0, 0, pmRemove) != 0 {
			if q.Message == wmQuit {
				quit = true
				break
			}
			call(pTranslateMessage, ptr(&q))
			call(pDispatchMessageW, ptr(&q))
		}
	}
}
