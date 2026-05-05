package fsm

import (
	"time"

	"github.com/librescoot/librefsm"
)

// Timing constants
const (
	ShutdownTimeout           = 5 * time.Second
	WaitingSeatboxTimeout     = 30 * time.Second
	HibernationInitialTimeout = 15 * time.Second
	HibernationConfirmTimeout = 30 * time.Second
	HibernationForceTimeout   = 30 * time.Second
	HibernationFinalTimeout   = 3 * time.Second
)

// NewDefinition creates the vehicle FSM definition.
// The actions parameter provides the implementation for state entry/exit
// and guards.
func NewDefinition(actions Actions) *librefsm.Definition {
	return librefsm.NewDefinition().
		// Basic states
		State(StateStandby,
			librefsm.WithOnEnter(actions.EnterStandby),
		).

		// At-rest parent grouping Parked, HopOn, HopOnLearning. Owns the
		// auto-standby timer: armed once on entry from outside the group,
		// cancelled once on exit to Drive/Shutdown. Sibling transitions
		// inside the group don't fire OnEnter/OnExit on this parent
		// (LCA-based transition semantics in librefsm), so the timer
		// runs continuously across hop-on detours without manual handoff.
		State(StateAtRest,
			librefsm.WithDefaultChild(StateParked),
			librefsm.WithOnEnter(actions.EnterAtRest),
			librefsm.WithOnExit(actions.ExitAtRest),
		).
		State(StateParked,
			librefsm.WithParent(StateAtRest),
			librefsm.WithOnEnter(actions.EnterParked),
		).
		State(StateReadyToDrive,
			librefsm.WithOnEnter(actions.EnterReadyToDrive),
		).
		State(StateWaitingSeatbox,
			librefsm.WithTimeout(WaitingSeatboxTimeout, EvWaitingSeatboxTimeout),
			librefsm.WithOnEnter(actions.EnterWaitingSeatbox),
		).
		State(StateUpdating).
		State(StateShuttingDown,
			librefsm.WithTimeout(ShutdownTimeout, EvShutdownTimeout, actions.OnShutdownTimeout),
			librefsm.WithOnEnter(actions.EnterShuttingDown),
		).

		// Hibernation parent state (for shared behavior)
		State(StateHibernation,
			librefsm.WithOnEnter(actions.EnterHibernation),
			librefsm.WithOnExit(actions.ExitHibernation),
		).

		// Hibernation substates (hierarchical)
		State(StateHibernationInitialHold,
			librefsm.WithParent(StateHibernation),
			librefsm.WithTimeout(HibernationInitialTimeout, EvHibernationInitialTimeout),
			librefsm.WithOnEnter(actions.EnterHibernationInitialHold),
		).
		State(StateHibernationAwaitingConfirm,
			librefsm.WithParent(StateHibernation),
			librefsm.WithTimeout(HibernationConfirmTimeout, EvHibernationConfirmTimeout),
			librefsm.WithOnEnter(actions.EnterHibernationAwaitingConfirm),
		).
		State(StateHibernationSeatbox,
			librefsm.WithParent(StateHibernation),
			librefsm.WithOnEnter(actions.EnterHibernationSeatbox),
		).
		State(StateHibernationConfirm,
			librefsm.WithParent(StateHibernation),
			librefsm.WithTimeout(HibernationFinalTimeout, EvHibernationFinalTimeout),
			librefsm.WithOnEnter(actions.EnterHibernationConfirm),
		).

		// Hop-on / hop-off mode (locked) — child of StateAtRest. Inputs
		// declared here are dropped at the FSM level, equivalent to the
		// pre-refactor handleInputChange early-return. The auto-standby
		// timer keeps running on the parent across the engage/release
		// detour without any manual deadline handoff.
		State(StateHopOn,
			librefsm.WithParent(StateAtRest),
			librefsm.WithOnEnter(actions.EnterHopOn),
			librefsm.WithOnExit(actions.ExitHopOn),
			librefsm.WithBlockedEvents(
				EvKickstandUp, EvKickstandDown,
				EvBrakesPressed, EvBrakesReleased,
				EvSeatboxButton, EvSeatboxClosed,
				EvDashboardReady, EvDashboardNotReady,
			),
		).

		// Hop-on combo learning — same input gating as StateHopOn but
		// no user-facing side-effects. Used while the dashboard records
		// the user's combo. Externally parked-equivalent.
		State(StateHopOnLearning,
			librefsm.WithParent(StateAtRest),
			librefsm.WithOnEnter(actions.EnterHopOnLearning),
			librefsm.WithOnExit(actions.ExitHopOnLearning),
			librefsm.WithBlockedEvents(
				EvKickstandUp, EvKickstandDown,
				EvBrakesPressed, EvBrakesReleased,
				EvSeatboxButton, EvSeatboxClosed,
				EvDashboardReady, EvDashboardNotReady,
			),
		).

		// === Transitions ===

		// From Standby - unlock events transition to Parked
		Transition(StateStandby, EvUnlock, StateParked).
		Transition(StateStandby, EvKeycardAuth, StateParked).             // Keycard tap unlocks from standby
		Transition(StateStandby, EvDbcUpdateComplete, StateShuttingDown). // DBC update complete, give DBC time to poweroff

		// === StateAtRest parent: family-wide transitions ===
		// These apply uniformly to Parked, HopOn, and HopOnLearning.
		// librefsm walks current state first then ancestors in
		// findAllTransitions, so child-specific overrides (like
		// Parked's EvUnlock -> RTD) win over these for the source
		// state that defines them.
		Transition(StateAtRest, EvLock, StateShuttingDown,
			librefsm.WithGuard(actions.IsSeatboxClosed),
		).
		Transition(StateAtRest, EvLock, StateWaitingSeatbox). // Fallback if seatbox open
		Transition(StateAtRest, EvForceLock, StateStandby,
			librefsm.WithAction(actions.OnForceLock),
		).
		Transition(StateAtRest, EvKeycardAuth, StateShuttingDown, // Keycard tap locks
			librefsm.WithGuards(actions.IsKickstandDown, actions.IsSeatboxClosed),
		).
		Transition(StateAtRest, EvKeycardAuth, StateWaitingSeatbox,
			librefsm.WithGuard(actions.IsKickstandDown), // Seatbox open, kickstand down
		).
		Transition(StateAtRest, EvAutoStandbyTimeout, StateShuttingDown).

		// From Parked - unlock/kickstand-up/dashboard-ready to ReadyToDrive if conditions met.
		Transition(StateParked, EvUnlock, StateReadyToDrive,
			librefsm.WithGuard(actions.CanEnterReadyToDrive), // Requires both kickstand up AND dashboard ready
		).
		Transition(StateParked, EvKickstandUp, StateReadyToDrive,
			librefsm.WithGuards(actions.IsDashboardReady, actions.IsHandlebarUnlocked),
		).
		Transition(StateParked, EvDashboardReady, StateReadyToDrive,
			librefsm.WithGuards(actions.IsKickstandUp, actions.IsHandlebarUnlocked),
		).
		Transition(StateParked, EvLockHibernate, StateShuttingDown,
			librefsm.WithAction(actions.OnLockHibernate),
		).
		// Manual ready-to-drive: seatbox button with kickstand up and both brakes pressed
		Transition(StateParked, EvSeatboxButton, StateReadyToDrive,
			librefsm.WithGuards(actions.IsKickstandUp, actions.AreBrakesPressed),
		).
		// Normal seatbox opening: button pressed in parked with kickstand down
		Transition(StateParked, EvSeatboxButton, StateParked,
			librefsm.WithGuard(actions.IsKickstandDown),
			librefsm.WithAction(actions.OnSeatboxButton),
		).
		// Hibernation entry: both brakes pressed in parked state
		Transition(StateParked, EvBrakesPressed, StateHibernationInitialHold,
			librefsm.WithGuard(actions.AreBrakesPressed),
		).
		// Hop-on / hop-off engage variants
		Transition(StateParked, EvHopOnEngage, StateHopOn).
		Transition(StateParked, EvHopOnLearningEngage, StateHopOnLearning).

		// From HopOn — release (combo matched) or mobile-app unlock both
		// drop back to the default child of StateAtRest. The standard
		// escape paths (lock/force-lock/keycard/auto-standby) live on
		// StateAtRest above and are inherited via the parent walk.
		// Physical inputs like kickstand-up are pre-empted by
		// BlockedEvents on the state itself.
		Transition(StateHopOn, EvHopOnRelease, StateParked).
		Transition(StateHopOn, EvUnlock, StateParked).

		// From HopOnLearning — same exit shape as HopOn.
		Transition(StateHopOnLearning, EvHopOnRelease, StateParked).
		Transition(StateHopOnLearning, EvUnlock, StateParked).

		// From ReadyToDrive
		Transition(StateReadyToDrive, EvKickstandDown, StateParked).
		Transition(StateReadyToDrive, EvDashboardNotReady, StateParked). // Safety: dashboard disconnect
		Transition(StateReadyToDrive, EvLock, StateShuttingDown,
			librefsm.WithGuard(actions.IsSeatboxClosed),
		).
		Transition(StateReadyToDrive, EvLock, StateWaitingSeatbox). // Fallback if seatbox open
		Transition(StateReadyToDrive, EvForceLock, StateStandby,
			librefsm.WithAction(actions.OnForceLock),
		).
		Transition(StateReadyToDrive, EvKeycardAuth, StateShuttingDown, // Requires kickstand down (never true in RTD)
			librefsm.WithGuard(actions.IsKickstandDown),
		).

		// From ShuttingDown
		Transition(StateShuttingDown, EvShutdownTimeout, StateStandby).
		Transition(StateShuttingDown, EvUnlock, StateParked).

		// From WaitingSeatbox - timeout or seatbox closed proceeds with lock
		Transition(StateWaitingSeatbox, EvWaitingSeatboxTimeout, StateShuttingDown).
		Transition(StateWaitingSeatbox, EvSeatboxClosed, StateShuttingDown).
		Transition(StateWaitingSeatbox, EvKeycardAuth, StateShuttingDown). // Second tap forces lock
		Transition(StateWaitingSeatbox, EvLock, StateShuttingDown).        // Explicit lock command forces lock
		Transition(StateWaitingSeatbox, EvUnlock, StateParked).            // Unlock cancels
		Transition(StateWaitingSeatbox, EvForceLock, StateStandby,
			librefsm.WithAction(actions.OnForceLock),
		).

		// Hibernation flow - all events are physical inputs
		// Manual ready-to-drive: seatbox button with kickstand up cancels hibernation and enters RTD
		Transition(StateHibernationInitialHold, EvSeatboxButton, StateReadyToDrive,
			librefsm.WithGuards(actions.IsKickstandUp, actions.AreBrakesPressed),
		).
		// Initial hold -> either advance (timeout) or cancel (brakes released)
		Transition(StateHibernationInitialHold, EvHibernationInitialTimeout, StateHibernationAwaitingConfirm).
		Transition(StateHibernationInitialHold, EvBrakesReleased, StateParked).

		// Awaiting confirm -> keycard tap confirms, seatbox button cancels
		Transition(StateHibernationAwaitingConfirm, EvKeycardAuth, StateHibernationConfirm,
			librefsm.WithGuard(actions.IsSeatboxClosed),
		).
		Transition(StateHibernationAwaitingConfirm, EvKeycardAuth, StateHibernationSeatbox).
		Transition(StateHibernationAwaitingConfirm, EvSeatboxButton, StateParked).
		Transition(StateHibernationAwaitingConfirm, EvHibernationForceTimeout, StateHibernationConfirm).
		Transition(StateHibernationAwaitingConfirm, EvHibernationConfirmTimeout, StateParked).

		// Seatbox state -> wait for seatbox to close, button cancels
		Transition(StateHibernationSeatbox, EvSeatboxClosed, StateHibernationConfirm).
		Transition(StateHibernationSeatbox, EvSeatboxButton, StateParked).

		// Final confirm -> execute hibernation after timeout, button cancels
		Transition(StateHibernationConfirm, EvHibernationFinalTimeout, StateShuttingDown,
			librefsm.WithAction(actions.OnHibernationComplete),
		).
		Transition(StateHibernationConfirm, EvSeatboxButton, StateParked).

		// Global transitions from any hibernation state (via parent)
		// Physical events that cancel hibernation from any substate
		Transition(StateHibernation, EvUnlock, StateParked).
		// Raising the kickstand cancels hibernation. If the rider is also
		// ready to drive (dashboard up, handlebar unlocked) skip the Parked
		// hop and go straight to RTD — otherwise the parent fallback to
		// Parked would land us in a state where the (already-raised)
		// kickstand can never produce a fresh edge to fire Parked->RTD.
		Transition(StateHibernation, EvKickstandUp, StateReadyToDrive,
			librefsm.WithGuards(actions.IsDashboardReady, actions.IsHandlebarUnlocked),
		).
		Transition(StateHibernation, EvKickstandUp, StateParked).

		// Initial state
		Initial(StateStandby)
}
