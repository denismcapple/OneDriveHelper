package popup

import (
	"runtime/debug"
	"sync/atomic"
)

// panicHandler receives panics recovered in goroutines the popup package
// starts for button, menu and tray actions.
var panicHandler atomic.Value // func(r any, stack []byte)

// SetPanicHandler sets the function told about a panic in an action. Without
// one, panics are still recovered (an action must never take the agent down).
func SetPanicHandler(h func(r any, stack []byte)) { panicHandler.Store(h) }

// safeRun runs f, recovering any panic.
func safeRun(f func()) {
	defer func() {
		if r := recover(); r != nil {
			if h, ok := panicHandler.Load().(func(any, []byte)); ok && h != nil {
				h(r, debug.Stack())
			}
		}
	}()
	f()
}
