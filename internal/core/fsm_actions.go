package core

import (
	"context"
	"fmt"
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

// restorableStates lists the states a restart may resume into directly.
// Anything outside the list is refused instead of being handed to SetState:
// systemStateToStateID passes unrecognised strings straight through, so a
// typo or a stale value from a future release would become a StateID the
// machine has never heard of and fail identically on every restart, and
// StateUpdating has no transition in or out, so resuming into it would
// strand the machine for the rest of the power session.
var restorableStates = map[types.SystemState]bool{
	types.StateStandby:                    true,
	types.StateParked:                     true,
	types.StateReadyToDrive:               true,
	types.StateHopOn:                      true,
	types.StateHopOnLearning:              true,
	types.StateWaitingSeatbox:             true,
	types.StateWaitingHibernation:         true,
	types.StateWaitingHibernationAdvanced: true,
	types.StateWaitingHibernationSeatbox:  true,
	types.StateWaitingHibernationConfirm:  true,
}

// restoreFSMState restores the FSM to a saved state (must be called after hardware init).
//
// It reports nothing to its caller on purpose. A restore failure used to abort
// Start(), which main.go turns into a Fatalf: systemd would restart the service,
// read the same state back out of Redis, hit the same failure and die again, on a
// vehicle that is by definition unlocked and powered up. There is also nothing to
// abort for, because SetState commits currentState before running the entry action,
// so the machine is already in the restored state by the time an error comes back.
func (v *VehicleSystem) restoreFSMState(savedState types.SystemState) {
	// Nothing persisted: fresh boot or post-hibernation cold start. Stay in the
	// initial Standby state.
	if savedState == "" {
		return
	}

	// ShuttingDown is not refused, it is handed over: Start() restores it further
	// down, after the LED cue block, so the shutdown timeout finishes normally.
	if savedState == types.StateShuttingDown {
		return
	}

	if savedState == types.StateUpdating {
		v.declineRestore(fmt.Sprintf("saved state %q cannot be left once entered, forced to stand-by", savedState))
		return
	}

	if !restorableStates[savedState] {
		v.declineRestore(fmt.Sprintf("saved state %q is not a state this vehicle can restore, forced to stand-by", savedState))
		return
	}

	// A marker naming this exact state means the last attempt at it never ran
	// to a conclusion. Losing the error return covers every way SetState can
	// return; it does not cover a panic inside an entry action, an OOM kill, a
	// hardware call that never returns, or a watchdog reset, where the process
	// dies before any recovery code runs and Redis still names the state that
	// killed it.
	attempt, err := v.redis.GetRestoreAttempt()
	if err != nil {
		v.logger.Warnf("Failed to read the restore attempt marker: %v", err)
	} else if attempt == string(savedState) {
		v.clearRestoreAttempt()
		v.declineRestore(fmt.Sprintf("restore of saved state %q was already interrupted once, forced to stand-by", savedState))
		return
	}

	if err := v.redis.SetRestoreAttempt(string(savedState), restoreAttemptTTL); err != nil {
		v.logger.Warnf("Failed to record the restore attempt marker: %v", err)
	}

	v.logger.Infof("Restoring FSM to saved state: %s", savedState)
	if err := v.machine.SetState(systemStateToStateID(savedState)); err != nil {
		v.logger.Errorf("Failed to restore FSM state: %v", err)
		v.assertMotorSafeOutputs()
	}

	// Reached on success and on a handled failure alike: both mean the restore
	// ran to a conclusion, which is exactly what the marker is asking about.
	v.clearRestoreAttempt()
}

func (v *VehicleSystem) clearRestoreAttempt() {
	if err := v.redis.ClearRestoreAttempt(); err != nil {
		v.logger.Warnf("Failed to clear the restore attempt marker: %v", err)
	}
}

// declineRestore refuses a saved state and leaves the machine in Standby.
// description names the specific refusal; it is logged and it is what the rider
// sees, so it has to read as a statement about the vehicle rather than about
// this function.
func (v *VehicleSystem) declineRestore(description string) {
	v.logger.Errorf("Declining state restore: %s", description)
	v.assertMotorSafeOutputs()
	v.markSteeringLockPending()

	// Raised last, once the machine has settled in Standby and the outputs have
	// been asserted, so nothing on the way clears it.
	//
	// Nothing clears this within the power session either, and that is the
	// honest lifetime: the vehicle is not in the state it was left in, and it
	// will not be until the next boot, where the startup reconcile drops it.
	// Clearing it on the next successful transition would be worse than
	// useless: the dashboard polls vehicle:fault every 5s and needs roughly 8s
	// from power-on, and the transition that would clear it is the same one
	// that powers the dashboard, so the rider would never see it.
	if err := v.redis.RaiseFault(FaultStateRestoreRefused, description); err != nil {
		v.logger.Errorf("Failed to raise fault %d: %v", FaultStateRestoreRefused, err)
	}
}

// assertMotorSafeOutputs puts the two outputs that can let the vehicle move into
// the configuration every non-driving state agrees on. This is not a rollback of
// anything: a declined restore means the machine sits in Standby while the GPIO
// initial values were picked from a saved state that may well have been powered,
// and Standby itself never cuts engine power.
//
// Brake before power cut, matching the ordering EnterParked documents.
func (v *VehicleSystem) assertMotorSafeOutputs() {
	if err := v.writeOutput("engine_brake", true); err != nil {
		v.logger.Errorf("Failed to engage engine brake after declined restore: %v", err)
	}
	if err := v.setPower("engine_power", false); err != nil {
		v.logger.Errorf("Failed to cut engine power after declined restore: %v", err)
	}
}

// markSteeringLockPending notes that a declined restore may have left the
// steering unlocked. The actuation itself is deferred to
// lockSteeringAfterDeclinedRestore, which Start() calls once the rest of the
// boot is out of the way.
func (v *VehicleSystem) markSteeringLockPending() {
	v.steeringLockPending = true
}

// lockSteeringAfterDeclinedRestore engages the steering lock when a declined
// restore left the machine in Standby with the lock sensor reading unlocked.
// Standby was entered from machine.Start() with an empty FromState, so
// EnterStandby's from-parked arm never ran and nothing else will lock it.
//
// Start() calls this after it has registered the input callbacks and re-seeded
// the lock latch, never from inside the restore. lockHandlebar's positioning
// window works by installing its own temporary handlebar_position callback and
// waiting a minute for the bars to reach the detent; arming it during the
// restore means Start()'s own callback registration overwrites that callback a
// few lines later, the window expires against nothing, and the steering is
// never actuated on the one path that exists to lock it. Deferring also keeps
// the lock goroutine, which writes handlebarUnlocked under mu, from running
// against the unguarded writes to that field earlier in Start().
func (v *VehicleSystem) lockSteeringAfterDeclinedRestore() {
	if !v.steeringLockPending {
		return
	}
	v.steeringLockPending = false

	if v.machine.CurrentState() != fsm.StateStandby {
		return
	}
	sensorVal, err := v.io.ReadDigitalInputDirect("handlebar_lock_sensor")
	if err != nil {
		v.logger.Warnf("Failed to read handlebar lock sensor after declined restore: %v", err)
		return
	}
	if !sensorVal {
		return
	}
	v.logger.Infof("Declined restore left the steering unlocked, arming the lock")
	v.lockHandlebar(nil)
}

// === State Entry Actions ===

func (v *VehicleSystem) EnterReadyToDrive(c *librefsm.Context) error {
	v.logger.Debugf("FSM: EnterReadyToDrive")

	// Record entry time for park debounce protection
	v.mu.Lock()
	v.readyToDriveEntryTime = time.Now()
	v.mu.Unlock()

	v.unlockHandlebarIfNeeded()

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

	// The ECU got no power, which continuing does not make worse. The machine is
	// in ready-to-drive either way, and the engine brake, the LED cue and the
	// brake resync below all still need to run. writeOutput has raised the fault.
	if err := v.setPower("engine_power", true); err != nil {
		v.logger.Errorf("%v", err)
	}

	if err := v.setPower("dashboard_power", true); err != nil {
		v.logger.Errorf("%v", err)

		// Cutting engine power here is not a rollback of the transition, it is
		// compensation for the side effect this same function performed four
		// lines up. It buys the interval between now and the engine brake being
		// engaged in Parked. Do not delete it: continuing without it leaves a
		// powered ECU, no dashboard, and the engine brake written from the
		// levers below, meaning released whenever the rider is not squeezing.
		if err := v.setPower("engine_power", false); err != nil {
			v.logger.Errorf("%v", err)
		}

		// Leave ready-to-drive honestly rather than sitting in it with no
		// dashboard. The transition exists for exactly this and is unguarded.
		// Delivered after the machine has already left ready-to-drive it finds
		// no transition and is dropped, which is harmless.
		c.Send(librefsm.Event{ID: fsm.EvDashboardNotReady})

		// Returning skips the rest on purpose. Everything below describes a
		// vehicle that is about to leave ready-to-drive, and the engine brake
		// write in particular is the one that would release the brake. Not
		// writing it leaves the brake where it was, which outside drive mode is
		// engaged.
		//
		// dashboardReady stays as it is: this was a GPIO write failure on the
		// power line, and the dashboard may well still be up. Clearing the flag
		// would fabricate sensor state.
		return nil
	}

	// Check current brake state and set engine brake pin accordingly. A failed
	// read can only mean an unknown channel name, so carry on with the false,
	// false it returns rather than skipping the write and the LED cue.
	brakeLeft, brakeRight, err := v.readBrakeStates()
	if err != nil {
		v.logger.Errorf("%v during transition", err)
	}
	if err := v.writeOutput("engine_brake", brakeLeft || brakeRight); err != nil {
		v.logger.Errorf("Failed to set engine brake during transition: %v", err)
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

	v.unlockHandlebarIfNeeded()

	// Ensure backlight is enabled for user interaction
	if err := v.redis.SetBacklightEnabled(true); err != nil {
		v.logger.Warnf("Failed to enable backlight: %v", err)
	}

	// Always turn on dashboard power when entering parked state. A dark
	// dashboard is not a reason to skip the engine brake and the ECU power-up
	// below, which is the much larger hole the old early return left.
	if err := v.setPower("dashboard_power", true); err != nil {
		v.logger.Errorf("%v", err)
	}

	// Engage the engine brake before powering the ECU so the motor cannot turn
	// while the controller comes up. Without a confirmed brake there is no such
	// guarantee, so the ECU is driven dark instead: a powered controller with
	// the brake in an unknown state is the movement this ordering exists to
	// prevent. Parked is entered from ready-to-drive, where engine power is
	// already on and the brake has been following the levers, so skipping the
	// power-up would not be enough.
	if err := v.writeOutput("engine_brake", true); err != nil {
		v.logger.Errorf("Failed to engage engine brake: %v", err)
		if err := v.setPower("engine_power", false); err != nil {
			v.logger.Errorf("%v", err)
		}

		// The controller stays dark from here until something enters a state
		// that powers it, which can be days. The brake write failure raises its
		// own code, but that one clears on the next lever edge that succeeds,
		// so without this the vehicle sits parked with a dark ECU and an empty
		// fault set. setPower clears this code once engine power is actually
		// back on.
		desc := fmt.Sprintf("engine controller held unpowered, the engine brake could not be engaged: %v", err)
		if raiseErr := v.redis.RaiseFault(FaultEcuHeldUnpowered, desc); raiseErr != nil {
			v.logger.Errorf("Failed to raise fault %d: %v", FaultEcuHeldUnpowered, raiseErr)
		}
	} else if err := v.setPower("engine_power", true); err != nil {
		v.logger.Errorf("%v", err)
	}

	prevState := stateIDToSystemState(c.FromState)
	if prevState == types.StateReadyToDrive {
		v.playLedCue(6, "drive to parked")
	}

	if prevState == types.StateStandby {
		// A failed read can only mean an unknown channel name. Carry on with the
		// false, false it returns rather than skipping the blinker switch
		// restore below for a condition that cannot occur.
		brakeLeft, brakeRight, err := v.readBrakeStates()
		if err != nil {
			v.logger.Errorf("%v", err)
		}
		brakesPressed := brakeLeft || brakeRight

		if brakesPressed {
			v.playLedCue(2, "standby to parked brake on")
		} else {
			v.playLedCue(1, "standby to parked brake off")
		}

		// Restore blinker if the physical switch is still held
		if left, err := v.io.ReadDigitalInput("blinker_left"); err == nil && left {
			if err := v.handleBlinkerChange("blinker_left", true); err != nil {
				v.logger.Warnf("Failed to restore left blinker: %v", err)
			}
		} else if right, err := v.io.ReadDigitalInput("blinker_right"); err == nil && right {
			if err := v.handleBlinkerChange("blinker_right", true); err != nil {
				v.logger.Warnf("Failed to restore right blinker: %v", err)
			}
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
		v.mu.RLock()
		override := v.handlebarUnlockedOverride
		v.mu.RUnlock()
		if override {
			v.logger.Debugf("Service mode: skipping handlebar re-lock on standby")
		} else {
			v.logger.Infof("Locking handlebar (direct transition from parked)")
			v.lockHandlebar(nil)

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
	}

	// Turn off dashboard power when entering standby, unless a DBC update or a
	// map download is still running. The map download hold is capped by
	// startMapHoldTimer, which applies this deferral when it expires.
	v.mu.Lock()
	if v.dbcUpdating || v.mapDownloading {
		if v.dbcUpdating {
			v.logger.Debugf("DBC update in progress, deferring dashboard power OFF until update completes")
		} else {
			v.logger.Debugf("Map download in progress, deferring dashboard power OFF for up to %v", dbcMapDownloadHoldMax)
		}
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

	// Start handlebar locking immediately, unless service mode holds it released.
	v.mu.RLock()
	override := v.handlebarUnlockedOverride
	v.mu.RUnlock()
	if override {
		v.logger.Debugf("Service mode: skipping handlebar lock during shutdown")
	} else {
		v.logger.Debugf("Starting handlebar locking during shutdown")
		v.lockHandlebar(nil)
	}

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
	v.mu.Lock()
	updating := v.dbcUpdating
	hibernating := v.hibernationRequest
	// A map download also defers the shutdown, but only for a capped window,
	// and never against hibernation: the MDB is about to cut power anyway.
	mapHolding := v.mapDownloading && !hibernating
	if mapHolding {
		v.startMapHoldTimer()
	} else if v.mapDownloading {
		v.mapDownloading = false
		v.stopMapHoldTimer()
	}
	v.mu.Unlock()
	if mapHolding {
		v.logger.Infof("Deferring DBC poweroff, map download in progress (up to %v)", dbcMapDownloadHoldMax)
	}
	if (!updating && !mapHolding) || hibernating {
		if updating && hibernating {
			v.logger.Infof("Hibernate requested, forcing DBC shutdown despite active update")
			v.mu.Lock()
			v.dbcUpdating = false
			v.deferredDashboardPower = nil
			v.mu.Unlock()
			if err := v.redis.RemoveInhibitor("dbc-update"); err != nil {
				v.logger.Warnf("Failed to remove DBC update inhibitor: %v", err)
			}
			if err := v.redis.RemoveInhibitor("install:dbc"); err != nil {
				v.logger.Warnf("Failed to remove DBC install inhibitor: %v", err)
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
	} else if updating {
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
	v.keylessCountdownActive = false
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
//
// Like every other exit action here it returns nil unconditionally, and that
// has to stay true. executeTransition bails out on an exit error before
// currentState is reassigned but after the state timers have been cancelled and
// whatever OnExit bodies it already reached have run, and it never fires the
// state change callback. Returning the ClearAutoStandbyDeadline error would
// therefore leave the machine reporting the state it is leaving, with its timers
// already torn down and no observer told. The error return exists for interface
// conformance; treat it as required to be nil and log failures inline.
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
	v.keylessCountdownActive = false
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
	return nil
}

func (v *VehicleSystem) EnterHibernationInitialHold(c *librefsm.Context) error {
	v.logger.Infof("FSM: EnterHibernationInitialHold - both brakes held, starting 15s timer")
	return nil
}

func (v *VehicleSystem) EnterHibernationAwaitingConfirm(c *librefsm.Context) error {
	v.logger.Infof("FSM: EnterHibernationAwaitingConfirm - initial hold complete, awaiting confirmation")

	// Force hibernation if both levers stay held. A state-scoped timer, so
	// leaving the state cancels it and the "are we still here" recheck a
	// hand-rolled timer needs disappears. The brake recheck lives on the
	// transition guard.
	//
	// Not WithTimeout on the state: that is a single field, already spent on
	// HibernationConfirmTimeout, and a second one would silently replace it.
	c.StartTimer(fsm.TimerHibernationForce, fsm.HibernationForceTimeout,
		librefsm.Event{ID: fsm.EvHibernationForceTimeout})

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

	// Steering-lock engagement with the same positioning grace window we use
	// for parked->standby: if the handlebar is already in lock-position it
	// locks immediately, otherwise the rider has handlebarLockWindow to move it
	// all the way left to trigger the lock. The onLocked callback records that
	// hop-on owns the lock so ExitHopOn releases the one it engaged.
	v.logger.Infof("hop-on: arming steering lock (positioning window)")
	v.lockHandlebar(func() {
		v.mu.Lock()
		v.hopOnLockedHandlebar = true
		v.mu.Unlock()
		v.logger.Infof("hop-on: steering lock engaged")
	})

	return nil
}

// ExitHopOn leaves the locked hop-on mode. Releases the steering lock if
// WE engaged it on entry. The auto-standby timer is parent-owned, so no
// timer plumbing is needed here.
func (v *VehicleSystem) ExitHopOn(c *librefsm.Context) error {
	// Tear down any still-open positioning window so it can't lock the
	// handlebar after we've left hop-on. The Parked exit path also cancels via
	// EnterParked->unlockHandlebarIfNeeded, but force-lock->Standby and
	// WaitingSeatbox do not, so cancel here unconditionally.
	v.cancelHandlebarLock()

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

// CanAbortShutdown reports whether a shutdown in progress can still be turned
// around. False once the DBC has been told to halt.
func (v *VehicleSystem) CanAbortShutdown(c *librefsm.Context) bool {
	return !v.dbcPoweroffSent.Load()
}

// IsBrakeHibernationEnabled reports the brake-hold hibernation setting.
func (v *VehicleSystem) IsBrakeHibernationEnabled(c *librefsm.Context) bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.brakeHibernationEnabled
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
