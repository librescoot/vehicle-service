package core

import (
	"fmt"
	"strings"
	"time"

	"vehicle-service/internal/types"
)

// handleSeatboxRequest handles seatbox lock requests from Redis
func (v *VehicleSystem) handleSeatboxRequest(on bool) error {
	v.logger.Debugf("Handling seatbox request: %v", on)
	if on {
		if err := v.redis.PublishSeatboxOpened(); err != nil {
			v.logger.Warnf("Failed to publish seatbox opened event: %v", err)
		}
		return v.openSeatboxLock()
	}
	return nil
}

// handleHornRequest handles horn activation requests from Redis
func (v *VehicleSystem) handleHornRequest(on bool) error {
	v.logger.Debugf("Handling horn request: %v", on)
	return v.io.WriteDigitalOutput("horn", on)
}

// handleBlinkerRequest handles blinker state requests from Redis
func (v *VehicleSystem) handleBlinkerRequest(state string) error {
	v.logger.Debugf("Handling blinker request: %s", state)

	// Stop any existing blinker routine
	if v.blinkerStopChan != nil {
		close(v.blinkerStopChan)
		v.blinkerStopChan = nil
	}

	var cue int
	switch state {
	case "off":
		cue = 9 // LED_BLINK_NONE
		v.blinkerState = BlinkerOff
	case "left":
		cue = 10 // LED_BLINK_LEFT
		v.blinkerState = BlinkerLeft
	case "right":
		cue = 11 // LED_BLINK_RIGHT
		v.blinkerState = BlinkerRight
	case "both":
		cue = 12 // LED_BLINK_BOTH
		v.blinkerState = BlinkerBoth
	default:
		return fmt.Errorf("invalid blinker state: %s", state)
	}

	if state != "off" {
		// Start blinker routine for non-off states
		v.blinkerStopChan = make(chan struct{})
		go v.runBlinker(cue, state, v.blinkerStopChan)
	} else {
		// For off state, play the cue immediately since no goroutine will handle it
		if err := v.io.PlayPwmCue(cue); err != nil {
			return err
		}
	}

	return v.redis.SetBlinkerState(state)
}

// handleStateRequest handles vehicle state change requests from Redis
func (v *VehicleSystem) handleStateRequest(state string) error {
	v.logger.Debugf("Handling state request: %s", state)
	currentState := v.getCurrentState()

	switch state {
	case "unlock":
		kickstandValue, err := v.io.ReadDigitalInput("kickstand")
		if err != nil {
			v.logger.Errorf("Failed to read kickstand: %v", err)
			if currentState == types.StateStandby {
				return v.transitionTo(types.StateParked)
			} else {
				return nil
			}

		}
		if v.isReadyToDrive() && !kickstandValue {
			return v.transitionTo(types.StateReadyToDrive)
		} else {
			if currentState == types.StateStandby {
				return v.transitionTo(types.StateParked)
			} else {
				return nil
			}
		}
	case "lock":
		if currentState == types.StateParked {
			v.logger.Infof("Transitioning to SHUTTING_DOWN")
			return v.transitionTo(types.StateShuttingDown)
		} else {
			return fmt.Errorf("vehicle must be parked to lock")
		}
	case "lock-hibernate":
		if currentState == types.StateParked {
			v.logger.Infof("Transitioning to SHUTTING_DOWN for lock-hibernate")
			v.mu.Lock()
			v.hibernationRequest = true // Set hibernation request for lock-hibernate
			v.mu.Unlock()
			if err := v.transitionTo(types.StateShuttingDown); err != nil {
				return err
			}

			if err := v.redis.SendCommand("scooter:power", "hibernate-manual"); err != nil {
				v.logger.Errorf("Failed to send hibernate command: %v", err)
			}
			return nil
		} else {
			return fmt.Errorf("vehicle must be parked to lock")
		}
	default:
		return fmt.Errorf("invalid state request: %s", state)
	}
}

// handleLedCueRequest handles LED cue playback requests from Redis
func (v *VehicleSystem) handleLedCueRequest(cueIndex int) error {
	v.logger.Debugf("Handling LED cue request: %d", cueIndex)
	return v.io.PlayPwmCue(cueIndex)
}

// handleLedFadeRequest handles LED fade playback requests from Redis
func (v *VehicleSystem) handleLedFadeRequest(ledChannel int, fadeIndex int) error {
	v.logger.Debugf("Handling LED fade request: channel=%d, index=%d", ledChannel, fadeIndex)
	return v.io.PlayPwmFade(ledChannel, fadeIndex)
}

// handleForceLockRequest handles force-lock requests from Redis
// It initiates a forced transition to standby state, skipping the handlebar lock
func (v *VehicleSystem) handleForceLockRequest() error {
	v.mu.Lock()
	v.forceStandbyNoLock = true
	v.mu.Unlock()

	// Always transition to STANDBY, the dbcUpdating flag is checked in the transition function
	v.logger.Infof("Handling force-lock request: transitioning to STANDBY (no lock).")
	return v.transitionTo(types.StateStandby)
}

// handleUpdateRequest handles update requests from the update-service
func (v *VehicleSystem) handleUpdateRequest(action string) error {
	v.logger.Debugf("Handling update request: %s", action)

	switch action {
	case "start":
		v.logger.Infof("Starting update process")
		return nil

	case "start-dbc":
		v.logger.Infof("Starting DBC update process")
		v.mu.Lock()
		v.dbcUpdating = true
		v.mu.Unlock()
		// Ensure dashboard is powered on
		if err := v.setPower("dashboard_power", true); err != nil {
			v.logger.Errorf("%v for DBC update", err)
			return err
		}
		return nil

	case "complete-dbc":
		v.logger.Infof("DBC update process complete")
		v.mu.Lock()
		v.dbcUpdating = false
		deferredPower := v.deferredDashboardPower
		v.deferredDashboardPower = nil
		currentState := v.state
		v.mu.Unlock()

		// Apply any deferred dashboard power state
		if deferredPower != nil {
			if *deferredPower {
				v.logger.Debugf("Applying deferred dashboard power: ON")
				if err := v.setPower("dashboard_power", true); err != nil {
					v.logger.Errorf("%v (deferred)", err)
					return err
				}
			} else {
				v.logger.Debugf("Applying deferred dashboard power: OFF")
				if err := v.setPower("dashboard_power", false); err != nil {
					v.logger.Errorf("%v (deferred)", err)
					return err
				}
			}
		} else if currentState == types.StateStandby {
			// No deferred power state, but we're in standby - turn off dashboard power
			v.logger.Debugf("DBC update complete and in standby state - turning off dashboard power")
			if err := v.setPower("dashboard_power", false); err != nil {
				v.logger.Errorf("%v", err)
				return err
			}
		}
		return nil

	case "complete":
		v.logger.Infof("Update process complete")
		return nil

	case "cycle-dashboard-power":
		// Cycle dashboard power to reboot the DBC
		v.logger.Infof("Cycling dashboard power to reboot DBC")

		// Turn off dashboard power
		if err := v.setPower("dashboard_power", false); err != nil {
			v.logger.Errorf("%v", err)
			return err
		}
		time.Sleep(1 * time.Second)
		// Turn dashboard power back on
		if err := v.setPower("dashboard_power", true); err != nil {
			v.logger.Errorf("%v", err)
			return err
		}
		v.logger.Infof("Dashboard power cycled successfully")
		return nil

	default:
		return fmt.Errorf("invalid update action: %s", action)
	}
}

// EnableDashboardForUpdate turns on the dashboard without entering ready-to-drive state
func (v *VehicleSystem) EnableDashboardForUpdate() error {
	v.logger.Debugf("Enabling dashboard for update")

	// Turn on dashboard power
	if err := v.setPower("dashboard_power", true); err != nil {
		v.logger.Errorf("%v for update", err)
		return err
	}

	v.logger.Infof("Dashboard power enabled for update")
	return nil
}

// handleHardwareRequest processes hardware power control commands from Redis
func (v *VehicleSystem) handleHardwareRequest(command string) error {
	v.logger.Debugf("Handling hardware request: %s", command)

	parts := strings.Split(command, ":")
	if len(parts) != 2 {
		return fmt.Errorf("invalid hardware command format: %s", command)
	}

	component := parts[0]
	action := parts[1]

	switch component {
	case "dashboard":
		switch action {
		case "on":
			if err := v.setPower("dashboard_power", true); err != nil {
				v.logger.Errorf("%v", err)
				return err
			}
			v.logger.Infof("Dashboard power enabled")
		case "off":
			if err := v.setPower("dashboard_power", false); err != nil {
				v.logger.Errorf("%v", err)
				return err
			}
			v.logger.Infof("Dashboard power disabled")
		default:
			return fmt.Errorf("invalid dashboard action: %s", action)
		}
	case "engine":
		switch action {
		case "on":
			if err := v.setPower("engine_power", true); err != nil {
				v.logger.Errorf("%v", err)
				return err
			}
			v.logger.Infof("Engine power enabled")
		case "off":
			if err := v.setPower("engine_power", false); err != nil {
				v.logger.Errorf("%v", err)
				return err
			}
			v.logger.Infof("Engine power disabled")
		default:
			return fmt.Errorf("invalid engine action: %s", action)
		}
	case "handlebar":
		switch action {
		case "lock":
			if err := v.pulseOutput("handlebar_lock_close", handlebarLockDuration); err != nil {
				v.logger.Infof("Failed to lock handlebar: %v", err)
				return err
			}
			v.logger.Infof("Handlebar lock activated")
		case "unlock":
			if err := v.pulseOutput("handlebar_lock_open", handlebarLockDuration); err != nil {
				v.logger.Infof("Failed to unlock handlebar: %v", err)
				return err
			}
			v.handlebarUnlocked = true
			v.logger.Infof("Handlebar unlock activated")
		default:
			return fmt.Errorf("invalid handlebar action: %s", action)
		}
	default:
		return fmt.Errorf("invalid hardware component: %s", component)
	}

	return nil
}
