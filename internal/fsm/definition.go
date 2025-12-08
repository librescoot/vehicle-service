package fsm

import (
	"time"

	"github.com/librescoot/librefsm"
)

// Timing constants
const (
	ShutdownTimeout           = 4 * time.Second
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
		State(StateInit).
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
		State(StateWaitingSeatbox).
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
		Transition(StateInit, EvDashboardReady, StateParked).

		// From Standby - always go to Parked first (dashboard is booting)
		Transition(StateStandby, EvUnlock, StateParked).
		Transition(StateStandby, EvDashboardReady, StateParked).

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
		Transition(StateParked, EvLock, StateShuttingDown).
		Transition(StateParked, EvLockHibernate, StateShuttingDown,
			librefsm.WithAction(actions.OnLockHibernate),
		).
		Transition(StateParked, EvForceLock, StateStandby,
			librefsm.WithAction(actions.OnForceLock),
		).
		Transition(StateParked, EvAutoStandbyTimeout, StateShuttingDown).
		Transition(StateParked, EvHibernationStart, StateHibernationInitialHold,
			librefsm.WithGuard(actions.AreBrakesPressed),
		).

		// From ReadyToDrive
		Transition(StateReadyToDrive, EvKickstandDown, StateParked).
		Transition(StateReadyToDrive, EvDashboardNotReady, StateParked). // Safety: dashboard disconnect
		Transition(StateReadyToDrive, EvLock, StateShuttingDown).

		// From ShuttingDown
		Transition(StateShuttingDown, EvShutdownTimeout, StateStandby,
			librefsm.WithAction(actions.OnShutdownTimeout),
		).
		Transition(StateShuttingDown, EvUnlock, StateParked).

		// Hibernation flow
		// Initial hold -> either advance or cancel
		Transition(StateHibernationInitialHold, EvHibernationInitialTimeout, StateHibernationAwaitingConfirm).
		Transition(StateHibernationInitialHold, EvBrakesReleased, StateParked). // Cancel if brakes released early
		Transition(StateHibernationInitialHold, EvHibernationCancel, StateParked).

		// Awaiting confirm -> confirm, seatbox check, or timeout/cancel
		Transition(StateHibernationAwaitingConfirm, EvHibernationConfirm, StateHibernationConfirm,
			librefsm.WithGuard(actions.IsSeatboxClosed),
		).
		Transition(StateHibernationAwaitingConfirm, EvHibernationConfirm, StateHibernationSeatbox,
			// If seatbox not closed, go to seatbox state
		).
		Transition(StateHibernationAwaitingConfirm, EvHibernationForceTimeout, StateHibernationConfirm).
		Transition(StateHibernationAwaitingConfirm, EvHibernationConfirmTimeout, StateParked).
		Transition(StateHibernationAwaitingConfirm, EvHibernationCancel, StateParked).

		// Seatbox state -> wait for seatbox to close
		Transition(StateHibernationSeatbox, EvSeatboxClosed, StateHibernationConfirm).
		Transition(StateHibernationSeatbox, EvHibernationCancel, StateParked).

		// Final confirm -> execute hibernation
		Transition(StateHibernationConfirm, EvHibernationFinalTimeout, StateShuttingDown,
			librefsm.WithAction(actions.OnHibernationComplete),
		).
		Transition(StateHibernationConfirm, EvHibernationCancel, StateParked).

		// Global cancel from any hibernation state (handled by parent exit)
		Transition(StateHibernation, EvUnlock, StateParked).

		// Initial state
		Initial(StateInit)
}
