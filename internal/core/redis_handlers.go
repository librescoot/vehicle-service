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
		v.openSeatboxLock()
	}
	return nil
}

// handleHornRequest handles horn activation requests from Redis
func (v *VehicleSystem) handleHornRequest(on bool) error {
	v.logger.Debugf("Handling horn request: %v", on)

	// Check if horn is allowed before activating
	if on && !v.isHornAllowed() {
		v.mu.RLock()
		mode := v.hornEnableMode
		v.mu.RUnlock()
		v.logger.Debugf("Horn request denied (mode: %s, state: %s)", mode, v.getCurrentState())
		return nil // Silent failure - don't activate horn but don't error
	}

	return v.io.WriteDigitalOutput("horn", on)
}

// handleBlinkerRequest handles blinker state requests from Redis
func (v *VehicleSystem) handleBlinkerRequest(state string) error {
	v.logger.Debugf("Handling blinker request: %s", state)

	// 1. FIRST stop the goroutine (prevents new cues from being played)
	v.stopBlinker()

	// 2. For "off", wait for in-progress fade to reach zero point
	if state == "off" {
		startNanos := v.blinkerStartNanos.Load()
		cueIdx := v.blinkerCueIndex.Load()

		if startNanos > 0 && cueIdx >= 0 && v.ledCurves != nil {
			totalElapsed := time.Duration(time.Now().UnixNano() - startNanos)
			cyclePos := totalElapsed % blinkerInterval
			waitTime := v.ledCurves.WaitForCueZeroOrEnd(int(cueIdx), cyclePos)
			if waitTime > 0 {
				v.logger.Debugf("Waiting %v for blinker fade zero point", waitTime)
				time.Sleep(waitTime)
			}
		}

		v.blinkerStartNanos.Store(0)
		v.blinkerCueIndex.Store(-1)
	}

	// 3. Determine and play the target cue
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
		v.blinkerCueIndex.Store(int32(cue))
		v.blinkerStartNanos.Store(time.Now().UnixNano())
		stopChan := make(chan struct{})
		v.mu.Lock()
		v.blinkerStopChan = stopChan
		v.mu.Unlock()
		go v.runBlinker(cue, state, stopChan)
	} else {
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

		// Keep MDB awake during standby while DBC is updating
		if err := v.redis.SetInhibitor("dbc-update", "suspend-only", "DBC update in progress"); err != nil {
			v.logger.Warnf("Failed to set DBC update inhibitor: %v", err)
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
		currentState := v.state
		v.mu.Unlock()

		// Persist to Redis
		if err := v.redis.SetDbcUpdating(false); err != nil {
			v.logger.Warnf("Failed to persist DBC updating state to Redis: %v", err)
		}

		// Remove suspend-only inhibitor now that DBC update is done
		if err := v.redis.RemoveInhibitor("dbc-update"); err != nil {
			v.logger.Warnf("Failed to remove DBC update inhibitor: %v", err)
		}

		// Handle deferred power-on request
		if deferredPower != nil && *deferredPower {
			v.logger.Debugf("Applying deferred dashboard power: ON")
			if err := v.setPower("dashboard_power", true); err != nil {
				v.logger.Errorf("%v (deferred)", err)
				return err
			}
			return nil
		}

		// If in standby, transition to shutting-down to give DBC time to poweroff cleanly
		// The shutdown timeout will transition back to standby, which turns off dashboard power
		if currentState == types.StateStandby {
			v.logger.Infof("DBC update complete in standby - transitioning to shutting-down for clean DBC shutdown")
			if err := v.machine.SendSync(librefsm.Event{ID: fsm.EvDbcUpdateComplete}); err != nil {
				v.logger.Warnf("Failed to send DBC update complete event: %v", err)
			}
		} else {
			v.logger.Debugf("DBC update complete but not in standby (state=%s) - leaving dashboard power on", currentState)
		}
		return nil

	case "complete":
		v.logger.Infof("Update process complete")
		return nil

	case "cycle-dashboard-power":
		// Cycle dashboard power to reboot the DBC
		v.mu.RLock()
		dbcUpdating := v.dbcUpdating
		v.mu.RUnlock()

		if dbcUpdating {
			v.logger.Warnf("Rejecting cycle-dashboard-power - DBC update in progress")
			return fmt.Errorf("DBC update in progress, cannot cycle dashboard power")
		}

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

			// Ask DBC to shut down cleanly before cutting power.
			// dbc-dispatcher listens on dbc:command and runs poweroff.
			if !force {
				if err := v.redis.PublishMessage("dbc:command", "poweroff"); err != nil {
					v.logger.Warnf("Failed to send DBC poweroff command: %v", err)
				} else {
					v.logger.Infof("Sent DBC poweroff, waiting 4s for clean shutdown")
					time.Sleep(4 * time.Second)
				}
			}

			if err := v.setPower("dashboard_power", false); err != nil {
				v.logger.Errorf("%v", err)
				return err
			}
			if force {
				v.logger.Infof("Dashboard power disabled (forced, no graceful shutdown)")
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
			if err := v.pulseHandlebarLock(true); err != nil {
				v.logger.Infof("Failed to lock handlebar: %v", err)
				return err
			}
			// Wait for mechanism to settle, then verify via sensor
			time.Sleep(handlebarLockRetryDelay)
			sensorVal, err := v.io.ReadDigitalInputDirect("handlebar_lock_sensor")
			if err != nil {
				v.logger.Warnf("Failed to read handlebar lock sensor after lock command: %v", err)
			} else {
				v.mu.Lock()
				v.handlebarUnlocked = sensorVal
				v.mu.Unlock()
				if sensorVal {
					v.logger.Warnf("Handlebar lock command sent but sensor still shows unlocked")
				} else {
					v.logger.Infof("Handlebar locked via hardware command")
				}
			}
		case "unlock":
			if err := v.pulseHandlebarLock(false); err != nil {
				v.logger.Infof("Failed to unlock handlebar: %v", err)
				return err
			}
			// Wait for mechanism to settle, then verify via sensor
			time.Sleep(handlebarUnlockRetryDelay)
			sensorVal, err := v.io.ReadDigitalInputDirect("handlebar_lock_sensor")
			if err != nil {
				v.logger.Warnf("Failed to read handlebar lock sensor after unlock command: %v", err)
			} else {
				v.mu.Lock()
				v.handlebarUnlocked = sensorVal
				v.mu.Unlock()
				if !sensorVal {
					v.logger.Warnf("Handlebar unlock command sent but sensor still shows locked")
				} else {
					v.logger.Infof("Handlebar unlocked via hardware command")
				}
			}
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

	case "scooter.enable-horn":
		// Read the new value from Redis
		value, err := v.redis.GetHashField("settings", settingKey)
		if err != nil {
			v.logger.Infof("Failed to read setting %s: %v", settingKey, err)
			return err
		}

		// Validate value
		if value != "true" && value != "false" && value != "in-drive" {
			v.logger.Warnf("Invalid horn enable mode: %s (must be 'true', 'false', or 'in-drive')", value)
			return fmt.Errorf("invalid horn enable mode: %s (must be 'true', 'false', or 'in-drive')", value)
		}

		v.mu.Lock()
		v.hornEnableMode = value
		v.mu.Unlock()
		v.logger.Infof("Horn enable mode updated to: %s", value)

	default:
		// Only log unknown settings if they're in the scooter namespace
		// Silently ignore settings for other services (e.g., updates.*, battery.*, etc.)
		if strings.HasPrefix(settingKey, "scooter.") {
			v.logger.Infof("Unknown scooter setting key: %s", settingKey)
		}
	}

	return nil
}

