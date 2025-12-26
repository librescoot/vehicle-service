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
	// Kickstand must be UP (value false) AND dashboard ready to enter ready-to-drive
	return !kickstandDown && v.IsDashboardReady(c)
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

	// 2. Open physical seatbox lock
	if err := v.openSeatboxLock(); err != nil {
		v.logger.Errorf("Failed to open seatbox lock: %v", err)
		return err
	}

	return nil
}
