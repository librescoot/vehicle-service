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
		return types.StateHopOn
	case fsm.StateHopOnLearning:
		return types.StateHopOnLearning
	case fsm.StateAtRest:
		// Parent grouping; never current as a leaf, but the converter
		// is defensive against transient lookups.
		return types.StateParked
	default:
		return types.SystemState(string(id))
	}
}

// systemStateToStateID converts types.SystemState to librefsm StateID
func systemStateToStateID(s types.SystemState) librefsm.StateID {
	switch s {
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
	case types.StateHopOn:
		return fsm.StateHopOn
	case types.StateHopOnLearning:
		return fsm.StateHopOnLearning
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

// restoreFSMState restores the FSM to a saved state (must be called after hardware init).
// Empty savedState means no state was persisted (fresh boot or post-hibernation cold
// start) — leave the FSM in its initial Standby state. ShuttingDown is also skipped
// so that a crash mid-shutdown doesn't strand us there; we'd rather fall through to
// Standby and let the user re-engage the lock cleanly.
func (v *VehicleSystem) restoreFSMState(savedState types.SystemState) error {
	if savedState != "" && savedState != types.StateShuttingDown {
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

	// Aborted shutdown: EnterShuttingDown played cue 7/8 (lights fading off).
	// Replay cue 1/2 to bring them back. This branch only runs when the DBC
	// was NOT told to halt (dbcPoweroffSent=false) — the unlock handler
	// queues the unlock otherwise, so we never arrive here with a halted
	// DBC.
	if prevState == types.StateShuttingDown {
		brakeLeft, brakeRight, _ := v.readBrakeStates()
		if brakeLeft || brakeRight {
			v.playLedCue(2, "shutting-down to parked brake on (unlock aborted shutdown)")
		} else {
			v.playLedCue(1, "shutting-down to parked brake off (unlock aborted shutdown)")
		}
	}

	// The auto-standby timer is owned by EnterAtRest / ExitAtRest now —
	// see the StateAtRest parent in fsm.NewDefinition. EnterParked fires
	// on every entry into the Parked leaf (including HopOn -> Parked),
	// but the at-rest parent is unaffected by sibling transitions, so
	// the timer keeps running through every hop-on detour without any
	// manual deadline handoff.

	return nil
}

func (v *VehicleSystem) EnterStandby(c *librefsm.Context) error {
	v.logger.Debugf("FSM: EnterStandby")

	v.cancelHandlebarUnlock()

	// Re-trust the lock sensor on standby entry. Covers OTA, vehicle-service
	// crash/restart, or manual physical intervention while powered off.
	// The async lockHandlebar() below will refine this once it completes.
	v.resyncHandlebarLatchFromSensor()

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

	// DBC is definitively off now (either via poweroff + GPIO cut, or just
	// the GPIO cut when an update deferral kept the flag clear). Reset the
	// tracker so the next ShuttingDown entry starts fresh.
	v.dbcPoweroffSent.Store(false)

	// Replay any unlock that was deferred during a committed shutdown.
	// The GPIO is now cut and the DBC has had its 5s to halt cleanly;
	// a fresh EvUnlock from Standby will cycle the GPIO back on via
	// EnterParked's setPower("dashboard_power", true). Dispatched on a
	// goroutine so machine.Send doesn't re-enter the FSM from inside an
	// onEnter callback.
	if v.pendingUnlock.CompareAndSwap(true, false) {
		v.logger.Infof("Replaying deferred unlock from shutdown")
		go v.machine.Send(librefsm.Event{ID: fsm.EvUnlock})
	}

	return nil
}

func (v *VehicleSystem) EnterShuttingDown(c *librefsm.Context) error {
	v.logger.Debugf("FSM: EnterShuttingDown")

	// Reset the DBC poweroff tracker. We may or may not publish this
	// cycle (update-in-progress path skips); set it true only on a
	// successful publish below so the unlock handler can gate the
	// abort path accurately.
	v.dbcPoweroffSent.Store(false)

	// A new lock intent overrides any unlock that was queued during a
	// previous committed shutdown — user's most recent action wins.
	v.pendingUnlock.Store(false)

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
	// GPIO cut in EnterStandby (5s later) is the hard backstop.
	//
	// If a DBC update is in progress, skip the poweroff so the DBC can keep
	// updating during standby. But if hibernation was requested, the MDB is
	// about to power off and the DBC will lose power anyway, so shut it down
	// cleanly instead of deferring.
	//
	// Once the publish succeeds, dbcPoweroffSent is set and the abort path
	// (ShuttingDown -> Parked on EvUnlock) is gated off in the unlock
	// handler: a late unlock gets queued and replayed from Standby.
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
		} else {
			// The DBC is now halting. This commit is irreversible: even if
			// the user sends unlock within the 5s shutdown window, we can
			// no longer abort cleanly because the DBC kernel is already on
			// its way out. The unlock handler reads this flag to decide
			// whether to queue the request for post-standby replay.
			v.dbcPoweroffSent.Store(true)
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

// === StateAtRest parent (auto-standby owner) ===

// EnterAtRest fires once on entry to the parked-family group from outside
// (Standby/Init/ShuttingDown -> unlock). Sibling transitions inside the
// group (Parked <-> HopOn <-> HopOnLearning) leave StateAtRest active and
// do NOT re-fire this — see librefsm machine.go LCA semantics.
func (v *VehicleSystem) EnterAtRest(c *librefsm.Context) error {
	v.logger.Debugf("FSM: EnterAtRest (parent)")

	v.mu.RLock()
	seconds := v.autoStandbySeconds
	v.mu.RUnlock()

	if seconds <= 0 || v.machine == nil {
		return nil
	}

	duration := time.Duration(seconds) * time.Second
	deadline := time.Now().Add(duration)

	v.mu.Lock()
	v.autoStandbyDeadline = deadline
	v.mu.Unlock()

	v.machine.StartTimer(fsm.TimerAutoStandby, duration, librefsm.Event{ID: fsm.EvAutoStandbyTimeout})
	v.logger.Infof("Started auto-standby timer: %d seconds", seconds)
	if err := v.redis.PublishAutoStandbyDeadline(deadline); err != nil {
		v.logger.Warnf("Failed to publish auto-standby deadline: %v", err)
	}
	return nil
}

// ExitAtRest fires once on exit to a non-parked-family state (Drive,
// Shutdown, Hibernation, Standby). Cancels the auto-standby timer.
func (v *VehicleSystem) ExitAtRest(c *librefsm.Context) error {
	v.logger.Debugf("FSM: ExitAtRest (parent)")
	if v.machine != nil {
		v.machine.StopTimer(fsm.TimerAutoStandby)
	}
	if err := v.redis.ClearAutoStandbyDeadline(); err != nil {
		v.logger.Warnf("Failed to clear auto-standby deadline: %v", err)
	}
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

// EnterHopOn enters the locked hop-on mode. The scooter stays powered up;
// the dashboard renders a lock screen. Physical inputs are dropped at the
// FSM level via the BlockedEvents declared on StateHopOn. The auto-standby
// timer is owned by the StateAtRest parent and continues running across
// the engage/release detour without manual handoff.
func (v *VehicleSystem) EnterHopOn(c *librefsm.Context) error {
	v.logger.Infof("FSM: EnterHopOn")

	// Play the same LED cue we use for parked->standby (cue 7/8 picked
	// by current brake state). Backlight handling lives entirely in the
	// dashboard now (HopOnStore::activate keeps it on for 30s so the
	// user sees the lock screen, then disables it via onBacklightDelayElapsed).
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
				v.setHandlebarLatch(true)
				v.logger.Infof("hop-on: steering lock engaged")
			}
		}()
	} else {
		v.logger.Debugf("hop-on: handlebar not in position, skipping steering lock")
	}

	return nil
}

// ExitHopOn leaves the locked hop-on mode. Releases the steering lock if
// WE engaged it on entry. The auto-standby timer is parent-owned, so no
// timer plumbing is needed here.
func (v *VehicleSystem) ExitHopOn(c *librefsm.Context) error {
	v.mu.Lock()
	releaseHandlebar := v.hopOnLockedHandlebar
	v.hopOnLockedHandlebar = false
	v.mu.Unlock()

	v.logger.Infof("FSM: ExitHopOn")

	// Reverse LED cue: same one we play for standby->parked.
	brakeLeft, brakeRight, _ := v.readBrakeStates()
	if brakeLeft || brakeRight {
		v.playLedCue(2, "hop-on release (brakes on)")
	} else {
		v.playLedCue(1, "hop-on release (brakes off)")
	}

	// Release the steering lock only if we engaged it on entry.
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
				v.setHandlebarLatch(false)
				v.logger.Infof("hop-on: steering lock released")
			}
		}()
	}

	return nil
}

// EnterHopOnLearning enters combo-learning mode. Borrows hop-on's input
// gating so the user can press buttons to record a combo without the
// scooter honking, blinking, opening the seatbox, or transitioning out.
// No user-facing side-effects: no LED cue, no steering-lock attempt, no
// backlight kill — the dashboard renders its own learn overlay. The
// auto-standby timer continues to run on the StateAtRest parent.
func (v *VehicleSystem) EnterHopOnLearning(c *librefsm.Context) error {
	v.logger.Infof("FSM: EnterHopOnLearning")
	return nil
}

// ExitHopOnLearning leaves combo-learning mode. Mirror of EnterHopOnLearning
// — nothing to undo, since nothing was set up.
func (v *VehicleSystem) ExitHopOnLearning(c *librefsm.Context) error {
	v.logger.Infof("FSM: ExitHopOnLearning")
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
