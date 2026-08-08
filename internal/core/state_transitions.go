package core

import (
	"time"

	"github.com/librescoot/librefsm"

	"vehicle-service/internal/fsm"
	"vehicle-service/internal/types"
)

// cancelHandlebarLock cancels any ongoing handlebar locking attempt
func (v *VehicleSystem) cancelHandlebarLock() {
	v.mu.Lock()
	if v.handlebarTimer != nil {
		v.logger.Infof("Cancelling handlebar locking timer")
		v.handlebarTimer.Stop()
		v.handlebarTimer = nil
	}
	if v.handlebarDone != nil {
		close(v.handlebarDone)
		v.handlebarDone = nil
	}
	v.mu.Unlock()
	// Restore original handlebar position callback
	v.io.RegisterInputCallback("handlebar_position", v.handleHandlebarPosition)
}

// cancelHandlebarUnlock cancels any ongoing handlebar unlock retry loop
func (v *VehicleSystem) cancelHandlebarUnlock() {
	v.mu.Lock()
	if v.handlebarUnlockDone != nil {
		close(v.handlebarUnlockDone)
		v.handlebarUnlockDone = nil
	}
	v.mu.Unlock()
}

// unlockHandlebarIfNeeded checks if the handlebar needs unlocking and unlocks it
// Also cancels any ongoing handlebar locking attempt
func (v *VehicleSystem) unlockHandlebarIfNeeded() error {
	v.cancelHandlebarLock()

	// Read the actual lock sensor state from hardware to avoid acting on
	// stale cached values (e.g. transient readings during a lock pulse)
	sensorVal, err := v.io.ReadDigitalInputDirect("handlebar_lock_sensor")
	if err != nil {
		v.logger.Warnf("Failed to read handlebar lock sensor: %v", err)
	} else {
		v.mu.Lock()
		v.handlebarUnlocked = sensorVal // sensor true (pressed) = unlocked
		v.mu.Unlock()
	}

	v.mu.RLock()
	unlocked := v.handlebarUnlocked
	v.mu.RUnlock()

	if !unlocked {
		v.unlockHandlebar()
	}
	return nil
}

// lockHandlebar initiates handlebar locking with a handlebarLockWindow
// positioning window. If the handlebar is already in lock-position it locks
// immediately; otherwise it waits for the user to move it into position within
// the window. The done channel is set up before the goroutine starts so
// cancelHandlebarLock() can cancel both the immediate-lock path and the
// timer-wait path. onLocked, if non-nil, runs once the lock is sensor-confirmed
// engaged (hop-on uses it to record that it owns the lock for release on exit).
func (v *VehicleSystem) lockHandlebar(onLocked func()) {
	done := make(chan struct{})
	v.mu.Lock()
	if v.handlebarDone != nil {
		close(v.handlebarDone)
	}
	v.handlebarDone = done
	v.mu.Unlock()

	go func() {
		// Check for cancellation before doing any work
		select {
		case <-done:
			return
		default:
		}

		handlebarPos, err := v.io.ReadDigitalInput("handlebar_position")
		if err != nil {
			v.logger.Errorf("Failed to read handlebar position: %v", err)
			return
		}

		if handlebarPos {
			// Handlebar is in position, lock it with retry + sensor verification
			v.lockHandlebarWithRetry(done, onLocked)
			// Clear done reference
			v.mu.Lock()
			if v.handlebarDone == done {
				v.handlebarDone = nil
			}
			v.mu.Unlock()
			return
		}

		// Handlebar not in position — set up timer and wait
		v.mu.Lock()
		if v.handlebarTimer != nil {
			v.handlebarTimer.Stop()
		}
		v.handlebarTimer = time.NewTimer(handlebarLockWindow)
		v.mu.Unlock()

		cleanup := func() {
			v.mu.Lock()
			v.handlebarTimer = nil
			if v.handlebarDone == done {
				v.handlebarDone = nil
			}
			v.mu.Unlock()
			v.io.RegisterInputCallback("handlebar_position", v.handleHandlebarPosition)
			v.logger.Debugf("Restored original handlebar position callback")
		}

		// Register temporary callback for handlebar position during lock window
		v.io.RegisterInputCallback("handlebar_position", func(channel string, value bool) error {
			if !value {
				return nil // Only care about activation
			}

			// Check if cancelled
			select {
			case <-done:
				v.logger.Debugf("Lock window callback: already cancelled")
				return nil
			default:
			}

			// Check if we're still in the window
			v.mu.RLock()
			timer := v.handlebarTimer
			v.mu.RUnlock()
			if timer == nil {
				v.logger.Debugf("Lock window has expired")
				return nil
			}

			timer.Stop()

			v.lockHandlebarWithRetry(done, onLocked)
			cleanup()
			return nil
		})

		v.logger.Debugf("Started 60 second window for handlebar lock")

		v.mu.RLock()
		timerC := v.handlebarTimer.C
		v.mu.RUnlock()
		select {
		case <-timerC:
			cleanup()
			v.logger.Debugf("Handlebar lock window expired")
		case <-done:
			v.logger.Debugf("Handlebar lock window cancelled")
		}
	}()
}

// lockHandlebarWithRetry pulses the lock closed and verifies via the lock sensor.
// Retries up to handlebarLockRetries times if the sensor doesn't confirm engagement.
// onLocked, if non-nil, runs once the lock is sensor-confirmed engaged.
func (v *VehicleSystem) lockHandlebarWithRetry(done chan struct{}, onLocked func()) {
	for attempt := 1; attempt <= handlebarLockRetries; attempt++ {
		// Check for cancellation
		select {
		case <-done:
			v.logger.Debugf("Handlebar lock cancelled before attempt %d", attempt)
			return
		default:
		}

		if err := v.pulseHandlebarLock(true); err != nil {
			v.logger.Errorf("Failed to lock handlebar (attempt %d/%d): %v", attempt, handlebarLockRetries, err)
			return
		}

		// Check for cancellation after pulse
		select {
		case <-done:
			v.logger.Debugf("Handlebar lock cancelled during pulse")
			return
		default:
		}

		// Wait for mechanism to settle after pulse ends
		time.Sleep(handlebarLockRetryDelay)

		// Read hardware sensor directly — the cached flag may reflect
		// a transient state during the pulse
		sensorVal, err := v.io.ReadDigitalInputDirect("handlebar_lock_sensor")
		if err != nil {
			v.logger.Errorf("Failed to read handlebar lock sensor: %v", err)
			return
		}
		locked := !sensorVal // sensor false (released) = locked

		// Update cached state to match reality
		v.mu.Lock()
		v.handlebarUnlocked = sensorVal
		v.mu.Unlock()

		if locked {
			v.setHandlebarLatch(true)
			v.logger.Infof("Handlebar locked successfully (attempt %d/%d)", attempt, handlebarLockRetries)
			if onLocked != nil {
				onLocked()
			}
			return
		}

		if attempt < handlebarLockRetries {
			v.logger.Warnf("Handlebar not locked after attempt %d/%d, retrying", attempt, handlebarLockRetries)
		}
	}
	v.logger.Errorf("Handlebar lock failed after %d attempts — lock sensor still shows unlocked", handlebarLockRetries)
}

// unlockHandlebar pulses the handlebar lock open output asynchronously.
// Does an initial burst of quick retries, then keeps retrying with backoff
// as long as the vehicle remains in parked or ready-to-drive state.
// The retry loop can be cancelled via cancelHandlebarUnlock().
func (v *VehicleSystem) unlockHandlebar() {
	v.cancelHandlebarUnlock()

	done := make(chan struct{})
	v.mu.Lock()
	v.handlebarUnlockDone = done
	v.mu.Unlock()

	go func() {
		attempt := 0
		for {
			attempt++

			select {
			case <-done:
				v.logger.Infof("Handlebar unlock cancelled (attempt %d)", attempt)
				return
			default:
			}

			stateID := v.getCurrentStateID()
			if stateID != fsm.StateParked && stateID != fsm.StateReadyToDrive {
				v.logger.Infof("Handlebar unlock: state changed to %s, stopping retries", stateID)
				return
			}

			if err := v.pulseHandlebarLock(false); err != nil {
				v.logger.Errorf("Failed to unlock handlebar (attempt %d): %v", attempt, err)
				return
			}

			time.Sleep(handlebarUnlockRetryDelay)

			select {
			case <-done:
				v.logger.Infof("Handlebar unlock cancelled after pulse (attempt %d)", attempt)
				return
			default:
			}

			sensorVal, err := v.io.ReadDigitalInputDirect("handlebar_lock_sensor")
			if err != nil {
				v.logger.Errorf("Failed to read handlebar lock sensor: %v", err)
				return
			}
			unlocked := sensorVal

			v.mu.Lock()
			v.handlebarUnlocked = unlocked
			v.mu.Unlock()

			if unlocked {
				v.setHandlebarLatch(false)
				v.logger.Infof("Handlebar unlocked successfully (attempt %d)", attempt)
				return
			}

			if attempt <= handlebarUnlockRetries {
				v.logger.Warnf("Handlebar still locked after attempt %d, retrying", attempt)
			} else {
				delay := time.Duration(attempt-handlebarUnlockRetries) * time.Second
				if delay > 5*time.Second {
					delay = 5 * time.Second
				}
				v.logger.Warnf("Handlebar still locked after attempt %d, retrying in %v", attempt, delay)

				select {
				case <-done:
					v.logger.Infof("Handlebar unlock cancelled during backoff (attempt %d)", attempt)
					return
				case <-time.After(delay):
				}
			}
		}
	}()
	v.logger.Infof("Handlebar unlock initiated")
}

// handleHandlebarPosition is the callback for handlebar position sensor changes
func (v *VehicleSystem) handleHandlebarPosition(channel string, value bool) error {
	// Always update Redis state first
	if err := v.redis.SetHandlebarPosition(value); err != nil {
		v.logger.Warnf("Failed to update handlebar position in Redis: %v", err)
	}

	if !value {
		return nil // Only care about activation
	}

	stateID := v.getCurrentStateID()
	if stateID != fsm.StateParked && stateID != fsm.StateReadyToDrive {
		return nil
	}

	// Read lock sensor from hardware to avoid stale cached state
	sensorVal, err := v.io.ReadDigitalInputDirect("handlebar_lock_sensor")
	if err != nil {
		v.logger.Warnf("Failed to read handlebar lock sensor: %v", err)
		return nil
	}
	unlocked := sensorVal // sensor true (pressed) = unlocked

	v.mu.Lock()
	v.handlebarUnlocked = unlocked
	v.mu.Unlock()

	if !unlocked {
		v.unlockHandlebar()
	}

	return nil
}

// getCurrentState returns the current state (thread-safe) using FSM
func (v *VehicleSystem) getCurrentState() types.SystemState {
	if v.machine != nil {
		return stateIDToSystemState(v.machine.CurrentState())
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.state
}

// getCurrentStateID returns the current FSM state ID
func (v *VehicleSystem) getCurrentStateID() librefsm.StateID {
	if v.machine != nil {
		return v.machine.CurrentState()
	}
	return systemStateToStateID(v.state)
}
