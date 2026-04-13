package core

import (
	"context"
	"time"

	"github.com/librescoot/librefsm"
	"vehicle-service/internal/fsm"
	"vehicle-service/internal/types"
)

// Ensure VehicleSystem implements fsm.Actions
var _ fsm.Actions = (*VehicleSystem)(nil)

// stateIDToSystemState converts librefsm StateID to types.SystemState
func stateIDToSystemState(id librefsm.StateID) types.SystemState {
	switch id {
	case fsm.StateInit:
		return types.StateInit
	case fsm.StateStandby:
		return types.StateStandby
	case fsm.StateParked:
		return types.StateParked
	case fsm.StateReadyToDrive:
		return types.StateReadyToDrive
	case fsm.StateWaitingSeatbox:
		return types.StateWaitingSeatbox
	case fsm.StateShuttingDown:
		return types.StateShuttingDown
	case fsm.StateUpdating:
		return types.StateUpdating
	case fsm.StateHibernationInitialHold:
		return types.StateParked // Keep parked state during silent 15s wait
	case fsm.StateHibernation:
		return types.StateWaitingHibernation
	case fsm.StateHibernationAwaitingConfirm:
		return types.StateWaitingHibernation
	case fsm.StateHibernationSeatbox:
		return types.StateWaitingHibernationSeatbox
	case fsm.StateHibernationConfirm:
		return types.StateWaitingHibernationConfirm
	case fsm.StateHopOn:
		// Externally hop-on looks like parked: the dashboard's `isParked()`
		// check (used to allow opening the menu, learning, etc.) keeps
		// working. Hop-on engagement is signalled separately via the
		// `vehicle:hop-on-active` field.
		return types.StateParked
	default:
		return types.SystemState(string(id))
	}
}

// systemStateToStateID converts types.SystemState to librefsm StateID
func systemStateToStateID(s types.SystemState) librefsm.StateID {
	switch s {
	case types.StateInit:
		return fsm.StateInit
	case types.StateStandby:
		return fsm.StateStandby
	case types.StateParked:
		return fsm.StateParked
	case types.StateReadyToDrive:
		return fsm.StateReadyToDrive
	case types.StateWaitingSeatbox:
		return fsm.StateWaitingSeatbox
	case types.StateShuttingDown:
		return fsm.StateShuttingDown
	case types.StateUpdating:
		return fsm.StateUpdating
	case types.StateWaitingHibernation:
		return fsm.StateHibernationInitialHold
	case types.StateWaitingHibernationAdvanced:
		return fsm.StateHibernationAwaitingConfirm
	case types.StateWaitingHibernationSeatbox:
		return fsm.StateHibernationSeatbox
	case types.StateWaitingHibernationConfirm:
		return fsm.StateHibernationConfirm
	default:
		return librefsm.StateID(string(s))
	}
}

// initFSM initializes and starts the librefsm machine
func (v *VehicleSystem) initFSM(ctx context.Context) error {
	def := fsm.NewDefinition(v)
	machine, err := def.Build()
	if err != nil {
		return err
	}
	v.machine = machine

	// Set up state change callback to sync legacy state field and publish
	v.machine.OnStateChange(func(from, to librefsm.StateID) {
		newState := stateIDToSystemState(to)
		oldState := stateIDToSystemState(from)

		v.mu.Lock()
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

		// Publish state directly using the known new state (avoid calling getCurrentState()
		// which would cause a deadlock with the FSM mutex)
		if err := v.redis.PublishVehicleState(newState); err != nil {
			v.logger.Errorf("Failed to publish state: %v", err)
		}
	})

	// Start the FSM
	if err := v.machine.Start(ctx); err != nil {
		return err
	}

	v.logger.Infof("librefsm state machine started")
	return nil
}

// restoreFSMState restores the FSM to a saved state (must be called after hardware init)
func (v *VehicleSystem) restoreFSMState(savedState types.SystemState) error {
	if savedState != types.StateInit && savedState != types.StateShuttingDown {
		v.logger.Infof("Restoring FSM to saved state: %s", savedState)
		if err := v.machine.SetState(systemStateToStateID(savedState)); err != nil {
			v.logger.Errorf("Failed to restore FSM state: %v", err)
			return err
		}
	}
	return nil
}

// sendEvent sends an event to the FSM
func (v *VehicleSystem) sendEvent(event librefsm.EventID) error {
	return v.machine.SendSync(librefsm.Event{ID: event})
}

// === State Entry Actions ===

func (v *VehicleSystem) EnterReadyToDrive(c *librefsm.Context) error {
	v.logger.Debugf("FSM: EnterReadyToDrive")

	// Record entry time for park debounce protection
	v.mu.Lock()
	v.readyToDriveEntryTime = time.Now()
	v.mu.Unlock()

	if err := v.unlockHandlebarIfNeeded(); err != nil {
		return err
	}

	// If handlebar is still locked after unlock attempt, the user forced RTD
	// via the three-button override — cancel the unlock loop.
	v.mu.RLock()
	stillLocked := !v.handlebarUnlocked
	v.mu.RUnlock()
	if stillLocked {
		v.logger.Infof("Handlebar still locked in RTD — forced entry, cancelling unlock retries")
		v.cancelHandlebarUnlock()
	}

	// Ensure backlight is enabled for user interaction
	if err := v.redis.SetBacklightEnabled(true); err != nil {
		v.logger.Warnf("Failed to enable backlight: %v", err)
	}

	if err := v.setPower("engine_power", true); err != nil {
		v.logger.Errorf("%v", err)
		return err
	}

	if err := v.setPower("dashboard_power", true); err != nil {
		v.logger.Errorf("%v", err)
		// Roll back engine power to avoid inconsistent hardware state
		if rbErr := v.setPower("engine_power", false); rbErr != nil {
			v.logger.Errorf("Rollback failed: %v", rbErr)
		}
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

	// When coming from standby, synchronize brake states
	prevState := stateIDToSystemState(c.FromState)
	if prevState == types.StateStandby {
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

		if brakeLeft || brakeRight {
			v.playLedCue(4, "brake off to on")
		}
	}

	return nil
}

func (v *VehicleSystem) EnterParked(c *librefsm.Context) error {
	v.logger.Debugf("FSM: EnterParked")

	if err := v.unlockHandlebarIfNeeded(); err != nil {
		return err
	}

	// Ensure backlight is enabled for user interaction
	if err := v.redis.SetBacklightEnabled(true); err != nil {
		v.logger.Warnf("Failed to enable backlight: %v", err)
	}

	// Always turn on dashboard power when entering parked state
	if err := v.setPower("dashboard_power", true); err != nil {
		v.logger.Errorf("%v", err)
		return err
	}

	// Engage engine brake BEFORE powering ECU to prevent movement
	if err := v.io.WriteDigitalOutput("engine_brake", true); err != nil {
		v.logger.Errorf("Failed to engage engine brake: %v", err)
		if rbErr := v.setPower("dashboard_power", false); rbErr != nil {
			v.logger.Errorf("Rollback failed: %v", rbErr)
		}
		return err
	}

	// Keep ECU powered in parked state
	if err := v.setPower("engine_power", true); err != nil {
		v.logger.Errorf("%v", err)
		if rbErr := v.setPower("dashboard_power", false); rbErr != nil {
			v.logger.Errorf("Rollback failed: %v", rbErr)
		}
		return err
	}

	prevState := stateIDToSystemState(c.FromState)
	if prevState == types.StateReadyToDrive {
		v.playLedCue(6, "drive to parked")
	}

	if prevState == types.StateStandby {
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

		// Restore blinker if the physical switch is still held
		if left, err := v.io.ReadDigitalInput("blinker_left"); err == nil && left {
			v.handleBlinkerChange("blinker_left", true)
		} else if right, err := v.io.ReadDigitalInput("blinker_right"); err == nil && right {
			v.handleBlinkerChange("blinker_right", true)
		}
	}

	// Start (or resume) the auto-standby timer.
	//
	// When transitioning back from hop-on we resume from the saved
	// deadline so a forgotten hop-on still drops to standby on the
	// original schedule. handleHopOnRequest captures the live deadline
	// just before sending EvHopOnEngage; ExitParked then clears the
	// live one but the saved copy persists across the StateHopOn detour.
	v.mu.RLock()
	seconds := v.autoStandbySeconds
	saved := v.hopOnSavedAutoStandbyDl
	v.mu.RUnlock()

	cameFromHopOn := c.FromState == fsm.StateHopOn

	if seconds > 0 && v.machine != nil {
		var duration time.Duration
		var deadline time.Time

		if cameFromHopOn && !saved.IsZero() {
			deadline = saved
			duration = time.Until(deadline)
			if duration < 1*time.Millisecond {
				duration = 1 * time.Millisecond
			}
			v.logger.Infof("Resumed auto-standby timer with %v remaining (came from hop-on)", duration)
		} else {
			duration = time.Duration(seconds) * time.Second
			deadline = time.Now().Add(duration)
			v.logger.Infof("Started auto-standby timer: %d seconds", seconds)
		}

		v.mu.Lock()
		v.autoStandbyDeadline = deadline
		v.hopOnSavedAutoStandbyDl = time.Time{} // consumed (or stale — clear either way)
		v.mu.Unlock()

		v.machine.StartTimer(fsm.TimerAutoStandby, duration, librefsm.Event{ID: fsm.EvAutoStandbyTimeout})
		if err := v.redis.PublishAutoStandbyDeadline(deadline); err != nil {
			v.logger.Warnf("Failed to publish auto-standby deadline: %v", err)
		}
	} else {
		// Even if no timer is armed, clear any stale saved deadline.
		v.mu.Lock()
		v.hopOnSavedAutoStandbyDl = time.Time{}
		v.mu.Unlock()
	}

	return nil
}

func (v *VehicleSystem) EnterStandby(c *librefsm.Context) error {
	v.logger.Debugf("FSM: EnterStandby")

	v.cancelHandlebarUnlock()

	// Turn off any active blinkers
	if err := v.handleBlinkerRequest("off"); err != nil {
		v.logger.Warnf("Failed to turn off blinkers on standby: %v", err)
	}

	v.mu.Lock()
	forcedStandby := v.forceStandbyNoLock
	if forcedStandby {
		v.forceStandbyNoLock = false
	}
	v.mu.Unlock()

	prevState := stateIDToSystemState(c.FromState)
	isFromParked := (prevState == types.StateParked)

	if forcedStandby {
		v.logger.Debugf("Forced standby: skipping handlebar lock.")
	} else if isFromParked {
		v.logger.Infof("Locking handlebar (direct transition from parked)")
		v.lockHandlebar()

		// Play shutdown LED cue
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

	// Final "all off" cue for standby
	v.playLedCue(0, "all off")

	return nil
}

func (v *VehicleSystem) EnterShuttingDown(c *librefsm.Context) error {
	v.logger.Debugf("FSM: EnterShuttingDown")

	v.cancelHandlebarUnlock()

	prevState := stateIDToSystemState(c.FromState)
	v.logger.Infof("Entering shutting down state from %s", prevState)

	// Track if we're coming from parked state
	if prevState == types.StateParked {
		v.mu.Lock()
		v.shutdownFromParked = true
		v.mu.Unlock()
		v.logger.Debugf("Shutdown initiated from parked state")
	}

	// Turn off engine power
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

	// Start handlebar locking immediately
	v.logger.Debugf("Starting handlebar locking during shutdown")
	v.lockHandlebar()

	// Note: The shutdown timer is handled by librefsm WithTimeout
	v.logger.Infof("Shutdown timer started via librefsm (4.0s)")

	// Ask DBC to shut down cleanly via Redis PUBSUB.
	// dbc-dispatcher on the DBC executes the poweroff.
	// GPIO cut in EnterStandby (10s later) is the hard backstop.
	//
	// If a DBC update is in progress, skip the poweroff so the DBC can keep
	// updating during standby. But if hibernation was requested, the MDB is
	// about to power off and the DBC will lose power anyway, so shut it down
	// cleanly instead of deferring.
	v.mu.RLock()
	updating := v.dbcUpdating
	hibernating := v.hibernationRequest
	v.mu.RUnlock()
	if !updating || hibernating {
		if updating && hibernating {
			v.logger.Infof("Hibernate requested, forcing DBC shutdown despite active update")
			v.mu.Lock()
			v.dbcUpdating = false
			v.deferredDashboardPower = nil
			v.mu.Unlock()
			if err := v.redis.RemoveInhibitor("dbc-update"); err != nil {
				v.logger.Warnf("Failed to remove DBC update inhibitor: %v", err)
			}
		}
		if err := v.redis.PublishMessage("dbc:command", "poweroff"); err != nil {
			v.logger.Warnf("Failed to send DBC poweroff: %v", err)
		}
	} else {
		v.logger.Infof("Skipping DBC poweroff, update in progress")
	}

	return nil
}

func (v *VehicleSystem) EnterWaitingSeatbox(c *librefsm.Context) error {
	v.logger.Infof("FSM: EnterWaitingSeatbox - please close seatbox to lock (30s timeout)")
	return nil
}

// === State Exit Actions ===

func (v *VehicleSystem) ExitParked(c *librefsm.Context) error {
	v.logger.Debugf("FSM: ExitParked")
	// Cancel auto-standby timer when leaving parked
	if v.machine != nil {
		v.machine.StopTimer(fsm.TimerAutoStandby)
		v.logger.Debugf("Stopped auto-standby timer")
	}
	// Clear the deadline from Redis
	if err := v.redis.ClearAutoStandbyDeadline(); err != nil {
		v.logger.Warnf("Failed to clear auto-standby deadline: %v", err)
	}
	// Clear the live deadline. The saved-for-hop-on copy is intentionally
	// NOT touched here — handleHopOnRequest captured it before triggering
	// EvHopOnEngage so EnterHopOn / EnterParked-from-HopOn can resume.
	v.mu.Lock()
	v.autoStandbyDeadline = time.Time{}
	v.mu.Unlock()
	return nil
}

// === Hibernation State Actions ===

func (v *VehicleSystem) EnterHibernation(c *librefsm.Context) error {
	v.logger.Debugf("FSM: EnterHibernation (parent state)")
	return nil
}

func (v *VehicleSystem) ExitHibernation(c *librefsm.Context) error {
	v.logger.Debugf("FSM: ExitHibernation (parent state)")
	// Cancel any hibernation-related timers
	v.mu.Lock()
	if v.hibernationForceTimer != nil {
		v.hibernationForceTimer.Stop()
		v.hibernationForceTimer = nil
	}
	v.mu.Unlock()
	return nil
}

func (v *VehicleSystem) EnterHibernationInitialHold(c *librefsm.Context) error {
	v.logger.Infof("FSM: EnterHibernationInitialHold - both brakes held, starting 15s timer")
	return nil
}

func (v *VehicleSystem) EnterHibernationAwaitingConfirm(c *librefsm.Context) error {
	v.logger.Infof("FSM: EnterHibernationAwaitingConfirm - initial hold complete, awaiting confirmation")

	// Start 15s timer to force hibernation if brakes still held
	v.mu.Lock()
	if v.hibernationForceTimer != nil {
		v.hibernationForceTimer.Stop()
	}
	v.hibernationForceTimer = time.AfterFunc(15*time.Second, func() {
		// Verify we're still in the awaiting-confirm state before sending event
		if v.machine.CurrentState() != fsm.StateHibernationAwaitingConfirm {
			v.logger.Debugf("FSM: Force hibernation timer fired but no longer in awaiting-confirm state")
			return
		}

		// Check if brakes are still pressed
		brakeLeft, err := v.io.ReadDigitalInput("brake_left")
		if err != nil {
			v.logger.Warnf("Failed to read brake_left for force hibernation: %v", err)
			return
		}
		brakeRight, err := v.io.ReadDigitalInput("brake_right")
		if err != nil {
			v.logger.Warnf("Failed to read brake_right for force hibernation: %v", err)
			return
		}

		if brakeLeft && brakeRight {
			v.logger.Infof("FSM: Force hibernation - brakes held for 15s")
			v.machine.Send(librefsm.Event{ID: fsm.EvHibernationForceTimeout})
		} else {
			v.logger.Debugf("FSM: Force hibernation cancelled - brakes released")
		}
	})
	v.mu.Unlock()

	return nil
}

func (v *VehicleSystem) EnterHibernationSeatbox(c *librefsm.Context) error {
	v.logger.Infof("FSM: EnterHibernationSeatbox - please close seatbox")
	return nil
}

func (v *VehicleSystem) EnterHibernationConfirm(c *librefsm.Context) error {
	v.logger.Infof("FSM: EnterHibernationConfirm - final 3s confirmation")
	return nil
}

// EnterHopOn enters hop-on / hop-off mode. The scooter stays powered up
// but handleInputChange will publish-only every input. The auto-standby
// timer is resumed from the deadline that handleHopOnRequest captured
// just before sending EvHopOnEngage (since ExitParked already cleared
// the live deadline by the time this runs).
//
// If the EvHopOnEngage payload carries silent=true (combo learning
// mode), the user-facing side-effects (LED cue, opportunistic steering
// lock, hop-on-active flag publish) are skipped — only the input
// suppression behaviour of StateHopOn is borrowed. The auto-standby
// resume still happens regardless.
func (v *VehicleSystem) EnterHopOn(c *librefsm.Context) error {
	silent := false
	if c != nil && c.Event != nil {
		if s, ok := c.Event.Payload.(bool); ok {
			silent = s
		}
	}
	if silent {
		v.logger.Infof("FSM: EnterHopOn (silent)")
	} else {
		v.logger.Infof("FSM: EnterHopOn")
	}

	v.mu.Lock()
	v.hopOnSilent = silent
	v.mu.Unlock()

	// Resume the auto-standby timer using the saved deadline. If it has
	// already passed, fire the timeout immediately so the FSM moves to
	// shutdown without delay.
	v.mu.RLock()
	saved := v.hopOnSavedAutoStandbyDl
	v.mu.RUnlock()

	if !saved.IsZero() && v.machine != nil {
		remaining := time.Until(saved)
		if remaining < 1*time.Millisecond {
			remaining = 1 * time.Millisecond
		}
		v.machine.StartTimer(fsm.TimerAutoStandby, remaining, librefsm.Event{ID: fsm.EvAutoStandbyTimeout})
		v.mu.Lock()
		v.autoStandbyDeadline = saved
		v.mu.Unlock()
		if err := v.redis.PublishAutoStandbyDeadline(saved); err != nil {
			v.logger.Warnf("Failed to publish auto-standby deadline in hop-on: %v", err)
		}
		v.logger.Infof("hop-on: resumed auto-standby timer with %v remaining", remaining)
	}

	if silent {
		// Learning mode: suppress every user-facing signal. The dashboard
		// shows its own learn overlay; vehicle-service stays "invisible"
		// while the user records their combo.
		return nil
	}

	// Publish the hop-on flag so the dashboard renders the lock screen.
	if err := v.redis.SetHopOnActive(true); err != nil {
		v.logger.Warnf("Failed to publish hop-on-active=true: %v", err)
	}

	// Play the same LED cue we use for parked->standby (cue 7/8 picked
	// by current brake state). Visually the scooter "powers down".
	brakeLeft, brakeRight, _ := v.readBrakeStates()
	if brakeLeft || brakeRight {
		v.playLedCue(8, "hop-on engage (brakes on)")
	} else {
		v.playLedCue(7, "hop-on engage (brakes off)")
	}

	// Opportunistic steering-lock engagement: only if the handlebar
	// happens to be in lock-position right now. Synchronous pulse
	// (~1.1s) so run on a goroutine to avoid blocking the FSM.
	positioned, err := v.io.ReadDigitalInputDirect("handlebar_position")
	if err != nil {
		v.logger.Warnf("hop-on: failed to read handlebar position: %v", err)
	} else if positioned {
		v.logger.Infof("hop-on: handlebar in position, engaging steering lock")
		go func() {
			if err := v.pulseHandlebarLock(true); err != nil {
				v.logger.Warnf("hop-on: failed to pulse handlebar lock: %v", err)
				return
			}
			time.Sleep(handlebarLockRetryDelay)
			sensorVal, err := v.io.ReadDigitalInputDirect("handlebar_lock_sensor")
			if err != nil {
				v.logger.Warnf("hop-on: failed to read handlebar lock sensor after pulse: %v", err)
				return
			}
			v.mu.Lock()
			v.handlebarUnlocked = sensorVal
			if !sensorVal {
				v.hopOnLockedHandlebar = true
			}
			v.mu.Unlock()
			if sensorVal {
				v.logger.Warnf("hop-on: lock pulse fired but sensor still reads unlocked")
			} else {
				v.logger.Infof("hop-on: steering lock engaged")
			}
		}()
	} else {
		v.logger.Debugf("hop-on: handlebar not in position, skipping steering lock")
	}

	return nil
}

// ExitHopOn leaves hop-on / hop-off mode. Always cancels the auto-standby
// timer (EnterParked or another state's onEnter will rearm it as needed)
// and releases the steering lock if WE engaged it on entry. The
// user-facing reverse LED cue and hop-on-active publish are skipped if
// we entered silently (combo learning) — they were never set.
func (v *VehicleSystem) ExitHopOn(c *librefsm.Context) error {
	v.mu.Lock()
	silent := v.hopOnSilent
	v.hopOnSilent = false
	releaseHandlebar := v.hopOnLockedHandlebar
	v.hopOnLockedHandlebar = false
	v.mu.Unlock()

	if silent {
		v.logger.Infof("FSM: ExitHopOn (silent)")
	} else {
		v.logger.Infof("FSM: ExitHopOn")
	}

	if v.machine != nil {
		v.machine.StopTimer(fsm.TimerAutoStandby)
	}
	if err := v.redis.ClearAutoStandbyDeadline(); err != nil {
		v.logger.Warnf("Failed to clear auto-standby deadline on hop-on exit: %v", err)
	}
	v.mu.Lock()
	v.autoStandbyDeadline = time.Time{}
	v.mu.Unlock()

	if !silent {
		if err := v.redis.SetHopOnActive(false); err != nil {
			v.logger.Warnf("Failed to publish hop-on-active=false: %v", err)
		}

		// Reverse LED cue: same one we play for standby->parked.
		brakeLeft, brakeRight, _ := v.readBrakeStates()
		if brakeLeft || brakeRight {
			v.playLedCue(2, "hop-on release (brakes on)")
		} else {
			v.playLedCue(1, "hop-on release (brakes off)")
		}
	}

	// Release the steering lock only if we engaged it on entry. Silent
	// mode never engages it, so this is naturally a no-op for learning.
	if releaseHandlebar {
		v.logger.Infof("hop-on: releasing steering lock that we engaged")
		go func() {
			if err := v.pulseHandlebarLock(false); err != nil {
				v.logger.Warnf("hop-on: failed to pulse handlebar unlock: %v", err)
				return
			}
			time.Sleep(handlebarUnlockRetryDelay)
			sensorVal, err := v.io.ReadDigitalInputDirect("handlebar_lock_sensor")
			if err != nil {
				v.logger.Warnf("hop-on: failed to read handlebar lock sensor after unlock pulse: %v", err)
				return
			}
			v.mu.Lock()
			v.handlebarUnlocked = sensorVal
			v.mu.Unlock()
			if !sensorVal {
				v.logger.Warnf("hop-on: unlock pulse fired but sensor still reads locked")
			} else {
				v.logger.Infof("hop-on: steering lock released")
			}
		}()
	}

	return nil
}

// === Guards ===

func (v *VehicleSystem) IsDashboardReady(c *librefsm.Context) bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.dashboardReady
}

func (v *VehicleSystem) CanEnterReadyToDrive(c *librefsm.Context) bool {
	kickstandDown, err := v.io.ReadDigitalInput("kickstand")
	if err != nil {
		v.logger.Errorf("Failed to read kickstand in guard: %v", err)
		return false
	}
	// Kickstand must be UP (value false) AND dashboard ready AND handlebar unlocked
	return !kickstandDown && v.IsDashboardReady(c) && v.IsHandlebarUnlocked(c)
}

func (v *VehicleSystem) IsKickstandDown(c *librefsm.Context) bool {
	kickstandDown, err := v.io.ReadDigitalInput("kickstand")
	if err != nil {
		v.logger.Errorf("Failed to read kickstand: %v", err)
		return true
	}
	return kickstandDown
}

func (v *VehicleSystem) IsKickstandUp(c *librefsm.Context) bool {
	kickstandDown, err := v.io.ReadDigitalInput("kickstand")
	if err != nil {
		v.logger.Errorf("Failed to read kickstand: %v", err)
		return false // Fail closed - don't allow ready-to-drive if can't read
	}
	return !kickstandDown
}

func (v *VehicleSystem) IsSeatboxClosed(c *librefsm.Context) bool {
	seatboxClosed, err := v.io.ReadDigitalInput("seatbox_lock_sensor")
	if err != nil {
		v.logger.Errorf("Failed to read seatbox sensor: %v", err)
		return false
	}
	return seatboxClosed
}

func (v *VehicleSystem) IsHandlebarUnlocked(c *librefsm.Context) bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.handlebarUnlocked
}

func (v *VehicleSystem) AreBrakesPressed(c *librefsm.Context) bool {
	brakeLeft, _ := v.io.ReadDigitalInput("brake_left")
	brakeRight, _ := v.io.ReadDigitalInput("brake_right")
	return brakeLeft && brakeRight
}

// === Transition Actions ===

func (v *VehicleSystem) OnShutdownTimeout(c *librefsm.Context) error {
	v.logger.Infof("FSM: Shutdown timeout - transitioning to standby")

	// Check if hibernation was requested
	v.mu.Lock()
	hibernationRequest := v.hibernationRequest
	v.hibernationRequest = false
	v.mu.Unlock()

	if hibernationRequest {
		v.logger.Infof("Hibernation requested, sending hibernate command")
		if err := v.redis.SendCommand("scooter:power", "hibernate-manual"); err != nil {
			v.logger.Errorf("Failed to send hibernate command: %v", err)
		}
	}

	return nil
}

func (v *VehicleSystem) OnAutoStandbyTimeout(c *librefsm.Context) error {
	v.logger.Infof("FSM: Auto-standby timeout")
	return nil
}

func (v *VehicleSystem) OnHibernationComplete(c *librefsm.Context) error {
	v.logger.Infof("FSM: Hibernation complete - triggering hibernation")
	v.mu.Lock()
	v.hibernationRequest = true
	v.mu.Unlock()
	return nil
}

func (v *VehicleSystem) OnLockHibernate(c *librefsm.Context) error {
	v.logger.Infof("FSM: Lock-hibernate - setting hibernation request")
	v.mu.Lock()
	v.hibernationRequest = true
	v.mu.Unlock()

	// Send hibernate command immediately (will execute after shutdown completes)
	if err := v.redis.SendCommand("scooter:power", "hibernate-manual"); err != nil {
		v.logger.Errorf("Failed to send hibernate command: %v", err)
	}
	return nil
}

func (v *VehicleSystem) OnForceLock(c *librefsm.Context) error {
	v.logger.Infof("FSM: Force-lock - setting force-standby flag")
	v.mu.Lock()
	v.forceStandbyNoLock = true
	v.mu.Unlock()
	return nil
}

func (v *VehicleSystem) OnSeatboxButton(c *librefsm.Context) error {
	v.logger.Infof("FSM: Seatbox button pressed - opening seatbox")

	// 1. Publish event first (for immediate UI response via PUBSUB)
	if err := v.redis.PublishSeatboxOpened(); err != nil {
		v.logger.Warnf("Failed to publish seatbox opened event: %v", err)
	}

	// 2. Open physical seatbox lock (async, fire-and-forget)
	v.openSeatboxLock()

	return nil
}
