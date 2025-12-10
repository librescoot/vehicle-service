package core

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/librescoot/librefsm"

	"vehicle-service/internal/fsm"
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
		// Send EvUnlock - FSM handles the logic:
		// - From Standby: goes to Parked (dashboard booting)
		// - From Parked: goes to ReadyToDrive if kickstand is up (via guard)
		// - From ShuttingDown: goes to Parked (cancel shutdown)
		return v.machine.SendSync(librefsm.Event{ID: fsm.EvUnlock})

	case "lock":
		if currentState != types.StateParked && currentState != types.StateReadyToDrive {
			return fmt.Errorf("vehicle must be parked or ready-to-drive to lock")
		}
		v.logger.Infof("Sending EvLock")
		return v.machine.SendSync(librefsm.Event{ID: fsm.EvLock})

	case "lock-hibernate":
		if currentState != types.StateParked {
			return fmt.Errorf("vehicle must be parked to lock-hibernate")
		}
		v.logger.Infof("Sending EvLockHibernate")
		// The OnLockHibernate action handles setting the hibernation flag and sending the command
		return v.machine.SendSync(librefsm.Event{ID: fsm.EvLockHibernate})

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
	// Send EvForceLock - the OnForceLock action sets the forceStandbyNoLock flag
	v.logger.Infof("Sending EvForceLock: transitioning to STANDBY (no lock)")
	return v.machine.SendSync(librefsm.Event{ID: fsm.EvForceLock})
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

		// Persist to Redis
		if err := v.redis.SetDbcUpdating(true); err != nil {
			v.logger.Warnf("Failed to persist DBC updating state to Redis: %v", err)
		}

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
		v.mu.Unlock()

		// Persist to Redis
		if err := v.redis.SetDbcUpdating(false); err != nil {
			v.logger.Warnf("Failed to persist DBC updating state to Redis: %v", err)
		}

		// Determine what power action to take (if any)
		var powerOff bool
		if deferredPower != nil {
			if *deferredPower {
				v.logger.Debugf("Applying deferred dashboard power: ON")
				if err := v.setPower("dashboard_power", true); err != nil {
					v.logger.Errorf("%v (deferred)", err)
					return err
				}
			} else {
				v.logger.Debugf("Scheduling deferred dashboard power OFF (5s delay)")
				powerOff = true
			}
		} else {
			v.mu.Lock()
			currentState := v.state
			v.mu.Unlock()

			if currentState == types.StateStandby {
				v.logger.Debugf("DBC update complete in standby - scheduling dashboard power OFF (5s delay)")
				powerOff = true
			} else {
				v.logger.Debugf("DBC update complete but not in standby (state=%s) - leaving dashboard power on", currentState)
			}
		}

		// Delay power-off to allow DBC to poweroff gracefully
		if powerOff {
			time.AfterFunc(5*time.Second, func() {
				if err := v.setPower("dashboard_power", false); err != nil {
					v.logger.Errorf("Failed to turn off dashboard power: %v", err)
				}
			})
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
	if len(parts) < 2 || len(parts) > 3 {
		return fmt.Errorf("invalid hardware command format: %s", command)
	}

	component := parts[0]
	action := parts[1]
	force := len(parts) == 3 && parts[2] == "force"

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
			// Check if DBC update is in progress (unless force is specified)
			if !force {
				v.mu.RLock()
				dbcUpdating := v.dbcUpdating
				v.mu.RUnlock()

				if dbcUpdating {
					v.logger.Warnf("Rejecting dashboard:off command - DBC update in progress (use :force to override)")
					return fmt.Errorf("DBC update in progress, cannot turn off dashboard (use dashboard:off:force to override)")
				}
			}

			if err := v.setPower("dashboard_power", false); err != nil {
				v.logger.Errorf("%v", err)
				return err
			}
			if force {
				v.logger.Infof("Dashboard power disabled (forced)")
			} else {
				v.logger.Infof("Dashboard power disabled")
			}
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

// handleSettingsUpdate processes settings changes from Redis
func (v *VehicleSystem) handleSettingsUpdate(settingKey string) error {
	v.logger.Infof("Handling settings update: %s", settingKey)

	switch settingKey {
	case "scooter.brake-hibernation":
		// Read the new value from Redis
		value, err := v.redis.GetHashField("settings", settingKey)
		if err != nil {
			v.logger.Infof("Failed to read setting %s: %v", settingKey, err)
			return err
		}

		v.mu.Lock()
		switch value {
		case "enabled":
			v.brakeHibernationEnabled = true
			v.logger.Infof("Brake hibernation enabled via settings update")
		case "disabled":
			v.brakeHibernationEnabled = false
			v.logger.Infof("Brake hibernation disabled via settings update")
		default:
			v.logger.Warnf("Unknown brake hibernation setting value: '%s'", value)
		}
		v.mu.Unlock()

	case "scooter.auto-standby-seconds":
		// Read the new value from Redis
		value, err := v.redis.GetHashField("settings", settingKey)
		if err != nil {
			v.logger.Infof("Failed to read setting %s: %v", settingKey, err)
			return err
		}

		seconds, parseErr := strconv.Atoi(value)
		if parseErr != nil {
			v.logger.Warnf("Invalid auto-standby setting value: '%s'", value)
			return fmt.Errorf("invalid auto-standby value: %s", value)
		}

		v.mu.Lock()
		oldSeconds := v.autoStandbySeconds
		v.autoStandbySeconds = seconds
		currentState := v.state
		v.mu.Unlock()

		if seconds > 0 {
			v.logger.Infof("Auto-standby enabled via settings update: %d seconds", seconds)
		} else {
			v.logger.Infof("Auto-standby disabled via settings update")
		}

		// If currently in parked state and setting changed, restart or cancel timer
		if currentState == types.StateParked {
			if seconds > 0 && oldSeconds != seconds {
				v.logger.Infof("Auto-standby setting changed while parked, restarting timer")
				v.cancelAutoStandbyTimer()
				v.startAutoStandbyTimer()
			} else if seconds == 0 {
				v.logger.Infof("Auto-standby disabled while parked, canceling timer")
				v.cancelAutoStandbyTimer()
			}
		}

	default:
		// Only log unknown settings if they're in the scooter namespace
		// Silently ignore settings for other services (e.g., updates.*, battery.*, etc.)
		if strings.HasPrefix(settingKey, "scooter.") {
			v.logger.Infof("Unknown scooter setting key: %s", settingKey)
		}
	}

	return nil
}
