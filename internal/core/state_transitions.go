package core

import (
	"fmt"
	"time"

	"vehicle-service/internal/types"
)

// transitionTo handles state transitions and their side effects
func (v *VehicleSystem) transitionTo(newState types.SystemState) error {
	v.mu.Lock()
	if newState == v.state {
		v.logger.Debugf("State transition skipped - already in state %s", newState)
		v.mu.Unlock()
		return nil
	}

	oldState := v.state
	v.state = newState

	v.mu.Unlock()

	// Request CPU governor change when leaving standby
	if oldState == types.StateStandby && newState != types.StateStandby {
		v.logger.Debugf("Leaving Standby: Requesting CPU governor change to ondemand")
		if err := v.redis.SendCommand("scooter:governor", "ondemand"); err != nil {
			v.logger.Warnf("Warning: Failed to request CPU governor change to ondemand: %v", err)
		}
	}

	v.logger.Infof("State transition: %s -> %s", oldState, newState)

	if err := v.publishState(); err != nil {
		v.logger.Errorf("Failed to publish state: %v", err)
		return fmt.Errorf("failed to publish state: %w", err)
	}

	// Cancel auto-standby timer when leaving parked state
	if oldState == types.StateParked && newState != types.StateParked {
		v.cancelAutoStandbyTimer()
	}

	v.logger.Debugf("Applying state transition effects for %s", newState)
	switch newState {

	case types.StateReadyToDrive:
		// Record entry time for park debounce protection
		v.mu.Lock()
		v.readyToDriveEntryTime = time.Now()
		v.mu.Unlock()

		if err := v.unlockHandlebarIfNeeded(); err != nil {
			return err
		}

		if err := v.setPower("engine_power", true); err != nil {
			v.logger.Errorf("%v", err)
			return err
		}

		if err := v.setPower("dashboard_power", true); err != nil {
			v.logger.Errorf("%v", err)
			return err
		}

		// Check current brake state and set engine brake pin accordingly
		brakeLeft, brakeRight, err := v.readBrakeStates()
		if err != nil {
			v.logger.Errorf("%v during transition", err)
			return err
		}
		if err := v.io.WriteDigitalOutput("engine_brake", brakeLeft || brakeRight); err != nil {
			v.logger.Errorf("Failed to set engine brake during transition: %v", err)
			return err
		}
		v.logger.Debugf("Engine brake set to %v during transition (left: %v, right: %v)", brakeLeft || brakeRight, brakeLeft, brakeRight)

		// Always play parked-to-drive cue when entering ready-to-drive
		v.playLedCue(3, "parked to drive")

		// When coming from standby, synchronize brake states since brake inputs were ignored
		if oldState == types.StateStandby {
			brakeLeft, brakeRight, err := v.readBrakeStates()
			if err != nil {
				v.logger.Errorf("%v after Standby->Ready transition", err)
			}

			if err := v.redis.SetBrakeState("left", brakeLeft); err != nil {
				v.logger.Warnf("Warning: failed to publish brake_left state after Standby->Ready transition: %v", err)
			}
			if err := v.redis.SetBrakeState("right", brakeRight); err != nil {
				v.logger.Warnf("Warning: failed to publish brake_right state after Standby->Ready transition: %v", err)
			}

			// Play brake cue if either brake is pressed
			if brakeLeft || brakeRight {
				v.playLedCue(4, "brake off to on")
			}
		}

	case types.StateParked:
		if err := v.unlockHandlebarIfNeeded(); err != nil {
			return err
		}

		// Always turn on dashboard power when entering parked state
		if err := v.setPower("dashboard_power", true); err != nil {
			v.logger.Errorf("%v", err)
			return err
		}

		// Engage engine brake BEFORE powering ECU to prevent movement
		if err := v.io.WriteDigitalOutput("engine_brake", true); err != nil {
			v.logger.Errorf("Failed to engage engine brake: %v", err)
			return err
		}

		// Keep ECU powered in parked state
		if err := v.setPower("engine_power", true); err != nil {
			v.logger.Errorf("%v", err)
			return err
		}

		if oldState == types.StateReadyToDrive {
			v.playLedCue(6, "drive to parked")
		}

		if oldState == types.StateStandby {
			brakeLeft, brakeRight, err := v.readBrakeStates()
			if err != nil {
				v.logger.Errorf("%v", err)
				return err
			}
			brakesPressed := brakeLeft || brakeRight

			if brakesPressed {
				v.playLedCue(2, "standby to parked brake on")
			} else {
				v.playLedCue(1, "standby to parked brake off")
			}
		}

	// Start auto-standby timer when entering parked state
	v.startAutoStandbyTimer()

	case types.StateStandby:
		v.mu.Lock()
		forcedStandby := v.forceStandbyNoLock
		shutdownFromParked := v.shutdownFromParked
		if forcedStandby {
			v.forceStandbyNoLock = false // Reset the flag
		}
		if shutdownFromParked {
			v.shutdownFromParked = false // Reset the flag
		}
		v.mu.Unlock()

		// Track standby entry time for MDB reboot timer (3-minute requirement)
		v.logger.Debugf("Setting standby timer start for MDB reboot coordination")
		if err := v.redis.PublishStandbyTimerStart(); err != nil {
			v.logger.Warnf("Warning: Failed to set standby timer start: %v", err)
			// Not critical for state transition
		}

		isFromParked := (oldState == types.StateParked)

		if forcedStandby {
			v.logger.Debugf("Forced standby: skipping handlebar lock.")
		} else if isFromParked {
			v.logger.Infof("Locking handlebar (direct transition from parked)")
			v.lockHandlebar()

			// Play shutdown LED cue (only for direct parked→standby without shutting-down)
			brakeLeft, brakeRight, err := v.readBrakeStates()
			if err != nil {
				v.logger.Infof("%v for standby cue", err)
			}
			brakesPressed := brakeLeft || brakeRight

			if brakesPressed {
				v.playLedCue(8, "parked brake on to standby")
			} else {
				v.playLedCue(7, "parked brake off to standby")
			}
		}

		// Turn off dashboard power when entering standby, unless DBC update is in progress
		v.mu.Lock()
		if v.dbcUpdating {
			v.logger.Debugf("DBC update in progress, deferring dashboard power OFF until update completes")
			powerOff := false
			v.deferredDashboardPower = &powerOff
			v.mu.Unlock()
		} else {
			v.mu.Unlock()
			if err := v.setPower("dashboard_power", false); err != nil {
				v.logger.Errorf("%v", err)
			}
		}

		// Final "all off" cue for standby.
		v.playLedCue(0, "all off")

	case types.StateShuttingDown:
		v.logger.Infof("Entering shutting down state from %s", oldState)

		// Track if we're coming from parked state
		if oldState == types.StateParked {
			v.mu.Lock()
			v.shutdownFromParked = true
			v.mu.Unlock()
			v.logger.Debugf("Shutdown initiated from parked state")
		}

		// Stop any existing shutdown timer
		if v.shutdownTimer != nil {
			v.shutdownTimer.Stop()
			v.shutdownTimer = nil
		}

		// Turn off all outputs
		if err := v.setPower("engine_power", false); err != nil {
			v.logger.Errorf("%v during shutdown", err)
		}

		// Play shutdown LED cue based on brake state
		brakeLeft, brakeRight, err := v.readBrakeStates()
		if err != nil {
			v.logger.Infof("%v for shutdown cue", err)
		}
		brakesPressed := brakeLeft || brakeRight

		if brakesPressed {
			v.playLedCue(8, "parked brake on to standby")
		} else {
			v.playLedCue(7, "parked brake off to standby")
		}

		// Start handlebar locking immediately to give user more time to position handlebar
		if oldState == types.StateParked {
			v.logger.Debugf("Starting handlebar locking during shutdown (from parked state)")
			v.lockHandlebar()
		}

		// Keep dashboard power on briefly to allow for proper shutdown messaging
		// The timer will handle transitioning to standby and turning off dashboard power

		v.shutdownTimer = time.AfterFunc(shutdownTimerDuration, v.triggerShutdownTimeout)
		v.logger.Infof("Started shutdown timer (4.0s)")

	}

	// Update engine brake based on new state
	if err := v.updateEngineBrake(); err != nil {
		v.logger.Errorf("Failed to update engine brake after state transition: %v", err)
		return err
	}

	v.logger.Debugf("State transition completed successfully")
	return nil
}

// unlockHandlebarIfNeeded checks if the handlebar needs unlocking and unlocks it
// Also cancels any ongoing handlebar locking attempt
func (v *VehicleSystem) unlockHandlebarIfNeeded() error {
	// Cancel any ongoing handlebar locking attempt when returning to active state
	if v.handlebarTimer != nil {
		v.logger.Infof("Cancelling handlebar locking timer")
		v.handlebarTimer.Stop()
		v.handlebarTimer = nil
		// Restore original handlebar position callback
		v.io.RegisterInputCallback("handlebar_position", v.handleHandlebarPosition)
	}

	// Check if handlebar needs to be unlocked
	handlebarPos, err := v.io.ReadDigitalInput("handlebar_position")
	if err != nil {
		v.logger.Infof("Failed to read handlebar position: %v", err)
		return err
	}
	if handlebarPos && !v.handlebarUnlocked {
		if err := v.unlockHandlebar(); err != nil {
			v.logger.Infof("Failed to unlock handlebar: %v", err)
			return err
		}
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

		// Start 10 second timer for handlebar position
		if v.handlebarTimer != nil {
			v.handlebarTimer.Stop()
		}
		v.handlebarTimer = time.NewTimer(handlebarLockWindow)

		// Create a cleanup function to restore the original callback
		cleanup := func() {
			v.handlebarTimer = nil
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
			if v.handlebarTimer == nil {
				v.logger.Debugf("Lock window has expired")
				return nil
			}

			// Stop the timer
			v.handlebarTimer.Stop()

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

		// Wait for timer expiration
		<-v.handlebarTimer.C
		cleanup()
		v.logger.Debugf("Handlebar lock window expired")
	}()
}

// unlockHandlebar pulses the handlebar lock open output
func (v *VehicleSystem) unlockHandlebar() error {
	if err := v.pulseOutput("handlebar_lock_open", handlebarLockDuration); err != nil {
		return fmt.Errorf("failed to unlock handlebar: %w", err)
	}
	v.handlebarUnlocked = true
	v.logger.Infof("Handlebar unlocked")
	return nil
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
		return v.unlockHandlebar()
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
