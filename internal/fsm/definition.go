package fsm

import (
	"time"

	"github.com/librescoot/librefsm"
)

// Timing constants
const (
	InitTimeout               = 2 * time.Second
	ShutdownTimeout           = 4 * time.Second
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
		State(StateInit,
			librefsm.WithTimeout(InitTimeout, EvInitTimeout),
		).
		State(StateStandby,
			librefsm.WithOnEnter(actions.EnterStandby),
		).
		State(StateParked,
			librefsm.WithOnEnter(actions.EnterParked),
			librefsm.WithOnExit(actions.ExitParked),
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

		// === Transitions ===

		// From Init
		Transition(StateInit, EvInitTimeout, StateStandby).

		// From Standby - unlock events transition to Parked
		Transition(StateStandby, EvUnlock, StateParked).
		Transition(StateStandby, EvKeycardAuth, StateParked). // Keycard tap unlocks from standby

		// From Parked - unlock/kickstand-up/dashboard-ready to ReadyToDrive if conditions met
		Transition(StateParked, EvUnlock, StateReadyToDrive,
			librefsm.WithGuard(actions.CanEnterReadyToDrive), // Requires both kickstand up AND dashboard ready
		).
		Transition(StateParked, EvKickstandUp, StateReadyToDrive,
			librefsm.WithGuard(actions.IsDashboardReady),
		).
		Transition(StateParked, EvDashboardReady, StateReadyToDrive,
			librefsm.WithGuard(actions.IsKickstandUp),
		).
		Transition(StateParked, EvLock, StateShuttingDown,
			librefsm.WithGuard(actions.IsSeatboxClosed),
		).
		Transition(StateParked, EvLock, StateWaitingSeatbox). // Fallback if seatbox open
		Transition(StateParked, EvLockHibernate, StateShuttingDown,
			librefsm.WithAction(actions.OnLockHibernate),
		).
		Transition(StateParked, EvForceLock, StateStandby,
			librefsm.WithAction(actions.OnForceLock),
		).
		Transition(StateParked, EvKeycardAuth, StateShuttingDown, // Keycard tap locks from parked
			librefsm.WithGuards(actions.IsKickstandDown, actions.IsSeatboxClosed),
		).
		Transition(StateParked, EvKeycardAuth, StateWaitingSeatbox,
			librefsm.WithGuard(actions.IsKickstandDown), // Seatbox open, kickstand down
		).
		Transition(StateParked, EvAutoStandbyTimeout, StateShuttingDown).
		// Manual ready-to-drive: seatbox button with kickstand up and both brakes pressed
		Transition(StateParked, EvSeatboxButton, StateReadyToDrive,
			librefsm.WithGuards(actions.IsKickstandUp, actions.AreBrakesPressed),
		).
		// Normal seatbox opening: button pressed in parked (fallback if guards above fail)
		Transition(StateParked, EvSeatboxButton, StateParked,
			librefsm.WithAction(actions.OnSeatboxButton),
		).
		// Hibernation entry: both brakes pressed in parked state
		Transition(StateParked, EvBrakesPressed, StateHibernationInitialHold,
			librefsm.WithGuard(actions.AreBrakesPressed),
		).

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
		Transition(StateHibernation, EvKickstandUp, StateParked).

		// Initial state
		Initial(StateInit)
}
