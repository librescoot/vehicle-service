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
	case fsm.StateHibernation, fsm.StateHibernationInitialHold:
		return types.StateWaitingHibernation
	case fsm.StateHibernationAwaitingConfirm:
		return types.StateWaitingHibernationAdvanced
	case fsm.StateHibernationSeatbox:
		return types.StateWaitingHibernationSeatbox
	case fsm.StateHibernationConfirm:
		return types.StateWaitingHibernationConfirm
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

	// Update engine brake based on new state
	if err := v.updateEngineBrake(); err != nil {
		v.logger.Errorf("Failed to update engine brake after state transition: %v", err)
		return err
	}

	return nil
}

func (v *VehicleSystem) EnterParked(c *librefsm.Context) error {
	v.logger.Debugf("FSM: EnterParked")

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
	}

	// Start auto-standby timer using librefsm
	v.mu.RLock()
	seconds := v.autoStandbySeconds
	v.mu.RUnlock()

	if seconds > 0 && v.machine != nil {
		duration := time.Duration(seconds) * time.Second
		v.machine.StartTimer(fsm.TimerAutoStandby, duration, librefsm.Event{ID: fsm.EvAutoStandbyTimeout})
		v.logger.Infof("Started auto-standby timer: %d seconds", seconds)

		// Publish the deadline time so UI can display countdown
		deadline := time.Now().Add(duration)
		if err := v.redis.PublishAutoStandbyDeadline(deadline); err != nil {
			v.logger.Warnf("Failed to publish auto-standby deadline: %v", err)
		}
	}

	// Update engine brake based on new state
	if err := v.updateEngineBrake(); err != nil {
		v.logger.Errorf("Failed to update engine brake after state transition: %v", err)
		return err
	}

	return nil
}

func (v *VehicleSystem) EnterStandby(c *librefsm.Context) error {
	v.logger.Debugf("FSM: EnterStandby")

	v.mu.Lock()
	forcedStandby := v.forceStandbyNoLock
	if forcedStandby {
		v.forceStandbyNoLock = false
	}
	v.mu.Unlock()

	// Track standby entry time for MDB reboot timer
	v.logger.Debugf("Setting standby timer start for MDB reboot coordination")
	if err := v.redis.PublishStandbyTimerStart(); err != nil {
		v.logger.Warnf("Warning: Failed to set standby timer start: %v", err)
	}

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

	// Update engine brake based on new state
	if err := v.updateEngineBrake(); err != nil {
		v.logger.Errorf("Failed to update engine brake after state transition: %v", err)
		return err
	}

	return nil
}

func (v *VehicleSystem) EnterShuttingDown(c *librefsm.Context) error {
	v.logger.Debugf("FSM: EnterShuttingDown")

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
	if prevState == types.StateParked {
		v.logger.Debugf("Starting handlebar locking during shutdown (from parked state)")
		v.lockHandlebar()
	}

	// Note: The shutdown timer is handled by librefsm WithTimeout
	v.logger.Infof("Shutdown timer started via librefsm (4.0s)")

	// Update engine brake based on new state
	if err := v.updateEngineBrake(); err != nil {
		v.logger.Errorf("Failed to update engine brake after state transition: %v", err)
		return err
	}

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
	return nil
}

func (v *VehicleSystem) EnterHibernationInitialHold(c *librefsm.Context) error {
	v.logger.Infof("FSM: EnterHibernationInitialHold - both brakes held, starting 15s timer")
	// The 15s timer is handled by librefsm WithTimeout
	// Transition to waiting-hibernation state for Redis publish
	if err := v.redis.PublishVehicleState(types.StateWaitingHibernation); err != nil {
		v.logger.Warnf("Failed to publish hibernation state: %v", err)
	}
	return nil
}

func (v *VehicleSystem) EnterHibernationAwaitingConfirm(c *librefsm.Context) error {
	v.logger.Infof("FSM: EnterHibernationAwaitingConfirm - initial hold complete, awaiting confirmation")
	// Play notification sound/LED
	if err := v.redis.PublishVehicleState(types.StateWaitingHibernationAdvanced); err != nil {
		v.logger.Warnf("Failed to publish hibernation state: %v", err)
	}
	return nil
}

func (v *VehicleSystem) EnterHibernationSeatbox(c *librefsm.Context) error {
	v.logger.Infof("FSM: EnterHibernationSeatbox - please close seatbox")
	if err := v.redis.PublishVehicleState(types.StateWaitingHibernationSeatbox); err != nil {
		v.logger.Warnf("Failed to publish hibernation state: %v", err)
	}
	return nil
}

func (v *VehicleSystem) EnterHibernationConfirm(c *librefsm.Context) error {
	v.logger.Infof("FSM: EnterHibernationConfirm - final 3s confirmation")
	if err := v.redis.PublishVehicleState(types.StateWaitingHibernationConfirm); err != nil {
		v.logger.Warnf("Failed to publish hibernation state: %v", err)
	}
	return nil
}

// === Guards ===

func (v *VehicleSystem) CanUnlock(c *librefsm.Context) bool {
	return true
}

func (v *VehicleSystem) CanEnterReadyToDrive(c *librefsm.Context) bool {
	kickstandDown, err := v.io.ReadDigitalInput("kickstand")
	if err != nil {
		v.logger.Errorf("Failed to read kickstand in guard: %v", err)
		return false
	}
	// Kickstand must be UP (value false) to enter ready-to-drive
	return !kickstandDown && v.isReadyToDrive()
}

func (v *VehicleSystem) IsKickstandDown(c *librefsm.Context) bool {
	kickstandDown, err := v.io.ReadDigitalInput("kickstand")
	if err != nil {
		v.logger.Errorf("Failed to read kickstand: %v", err)
		return true
	}
	return kickstandDown
}

func (v *VehicleSystem) IsSeatboxClosed(c *librefsm.Context) bool {
	seatboxClosed, err := v.io.ReadDigitalInput("seatbox_lock_sensor")
	if err != nil {
		v.logger.Errorf("Failed to read seatbox sensor: %v", err)
		return false
	}
	return seatboxClosed
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
