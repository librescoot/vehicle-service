package core

import (
	"fmt"
	"time"

	"github.com/librescoot/librefsm"

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

// unlockHandlebarIfNeeded checks if the handlebar needs unlocking and unlocks it
// Also cancels any ongoing handlebar locking attempt
func (v *VehicleSystem) unlockHandlebarIfNeeded() error {
	v.cancelHandlebarLock()

	// Check if handlebar needs to be unlocked
	handlebarPos, err := v.io.ReadDigitalInput("handlebar_position")
	if err != nil {
		v.logger.Infof("Failed to read handlebar position: %v", err)
		return err
	}
	if handlebarPos && !v.handlebarUnlocked {
		v.unlockHandlebar()
	}
	return nil
}

// lockHandlebar initiates handlebar locking with a 10-second window for positioning
func (v *VehicleSystem) lockHandlebar() {
	// Run the lock operation in a goroutine
	go func() {
		handlebarPos, err := v.io.ReadDigitalInput("handlebar_position")
		if err != nil {
			v.logger.Errorf("Failed to read handlebar position: %v", err)
			return
		}

		if handlebarPos {
			// Handlebar is in position, lock it immediately
			if err := v.pulseOutput("handlebar_lock_close", handlebarLockDuration); err != nil {
				v.logger.Infof("Failed to lock handlebar: %v", err)
				return
			}
			v.logger.Infof("Handlebar locked")

			// Reset unlock state after successful lock
			v.mu.Lock()
			v.handlebarUnlocked = false
			v.mu.Unlock()
			v.logger.Debugf("Reset handlebar unlock state after successful lock")
			return
		}

		// Create done channel and timer under mutex
		done := make(chan struct{})
		v.mu.Lock()
		if v.handlebarTimer != nil {
			v.handlebarTimer.Stop()
		}
		v.handlebarTimer = time.NewTimer(handlebarLockWindow)
		if v.handlebarDone != nil {
			close(v.handlebarDone)
		}
		v.handlebarDone = done
		v.mu.Unlock()

		// Create a cleanup function to restore the original callback
		cleanup := func() {
			v.mu.Lock()
			v.handlebarTimer = nil
			v.handlebarDone = nil
			v.mu.Unlock()
			// Re-register the original handlebar position callback
			v.io.RegisterInputCallback("handlebar_position", v.handleHandlebarPosition)
			v.logger.Debugf("Restored original handlebar position callback")
		}

		// Register temporary callback for handlebar position during lock window
		v.io.RegisterInputCallback("handlebar_position", func(channel string, value bool) error {
			if !value {
				return nil // Only care about activation
			}

			// Check if we're still in the window
			v.mu.RLock()
			timer := v.handlebarTimer
			v.mu.RUnlock()
			if timer == nil {
				v.logger.Debugf("Lock window has expired")
				return nil
			}

			// Stop the timer
			timer.Stop()

			// Lock the handlebar
			if err := v.pulseOutput("handlebar_lock_close", handlebarLockDuration); err != nil {
				cleanup()
				v.logger.Infof("Failed to lock handlebar: %v", err)
				return err
			}
			v.logger.Infof("Handlebar locked")

			// Reset unlock state after successful lock
			v.mu.Lock()
			v.handlebarUnlocked = false
			v.mu.Unlock()
			v.logger.Debugf("Reset handlebar unlock state after successful lock")

			// Restore original callback
			cleanup()
			return nil
		})

		v.logger.Debugf("Started 10 second window for handlebar lock")

		// Wait for timer expiration or cancellation
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

// unlockHandlebar pulses the handlebar lock open output asynchronously.
// The handlebarUnlocked flag is set reactively by the lock sensor callback.
func (v *VehicleSystem) unlockHandlebar() {
	go func() {
		if err := v.pulseOutput("handlebar_lock_open", handlebarLockDuration); err != nil {
			v.logger.Errorf("Failed to unlock handlebar: %v", err)
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

	state := v.getCurrentState()
	v.mu.RLock()
	unlocked := v.handlebarUnlocked
	v.mu.RUnlock()

	// Only unlock if we haven't unlocked yet in this power cycle
	if !unlocked && (state == types.StateParked || state == types.StateReadyToDrive) {
		v.unlockHandlebar()
	}

	return nil
}

// updateEngineBrake updates only the engine brake based on current state
// Used when brake lever state changes but vehicle state hasn't
func (v *VehicleSystem) updateEngineBrake() error {
	v.mu.RLock()
	currentState := v.state
	v.mu.RUnlock()

	// Read current brake states
	brakeLeft, err := v.io.ReadDigitalInput("brake_left")
	if err != nil {
		return fmt.Errorf("failed to read brake_left: %w", err)
	}
	brakeRight, err := v.io.ReadDigitalInput("brake_right")
	if err != nil {
		return fmt.Errorf("failed to read brake_right: %w", err)
	}

	// Engine brake logic:
	// - In READY_TO_DRIVE: follows brake levers (engaged when either pressed)
	// - In all other states: always engaged (motor disabled)
	var engineBrakeEngaged bool
	if currentState == types.StateReadyToDrive {
		engineBrakeEngaged = brakeLeft || brakeRight
	} else {
		engineBrakeEngaged = true
	}

	if err := v.io.WriteDigitalOutput("engine_brake", engineBrakeEngaged); err != nil {
		return fmt.Errorf("failed to set engine brake: %w", err)
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
