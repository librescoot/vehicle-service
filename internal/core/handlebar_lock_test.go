package core

import (
	"sync"
	"testing"
	"time"
)

// lockHandlebar's goroutine sets v.handlebarTimer, releases the lock, registers
// a position callback, and only then reads the timer's channel back. Anything
// that clears the timer in between leaves it dereferencing nil:
// cancelHandlebarLock does exactly that, and so does the window callback's own
// cleanup. A keycard tap or a state change landing while the lock window opens
// is enough.
//
// The panic happens on the lockHandlebar goroutine, so it takes the whole test
// binary down rather than failing this test.
func TestLockHandlebarCancelledWhileOpeningWindow(t *testing.T) {
	for i := 0; i < 300; i++ {
		system, mockIO, _ := newTestVehicleSystem()
		// Out of position, so the goroutine takes the timer-wait path.
		mockIO.setDigitalInput("handlebar_position", false)

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			system.cancelHandlebarLock()
		}()
		system.lockHandlebar(nil)
		wg.Wait()

		system.cancelHandlebarLock()
		time.Sleep(time.Millisecond)
	}
}
