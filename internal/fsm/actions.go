package fsm

import "github.com/librescoot/librefsm"

// Actions defines the interface for vehicle state machine actions.
// VehicleSystem implements this interface to handle state entry/exit
// and provide guards for conditional transitions.
type Actions interface {
	// State entry actions
	EnterReadyToDrive(c *librefsm.Context) error
	EnterParked(c *librefsm.Context) error
	EnterStandby(c *librefsm.Context) error
	EnterShuttingDown(c *librefsm.Context) error

	// State exit actions
	ExitParked(c *librefsm.Context) error

	// Hibernation state actions
	EnterHibernation(c *librefsm.Context) error
	ExitHibernation(c *librefsm.Context) error
	EnterHibernationInitialHold(c *librefsm.Context) error
	EnterHibernationAwaitingConfirm(c *librefsm.Context) error
	EnterHibernationSeatbox(c *librefsm.Context) error
	EnterHibernationConfirm(c *librefsm.Context) error

	// Guards for conditional transitions
	CanEnterReadyToDrive(c *librefsm.Context) bool // True when both kickstand up AND dashboard ready
	IsDashboardReady(c *librefsm.Context) bool     // True when dashboard has booted
	IsKickstandUp(c *librefsm.Context) bool        // True when kickstand is up (riding position)
	IsKickstandDown(c *librefsm.Context) bool      // True when kickstand is down (parked position)
	IsSeatboxClosed(c *librefsm.Context) bool
	AreBrakesPressed(c *librefsm.Context) bool

	// Transition actions
	OnShutdownTimeout(c *librefsm.Context) error
	OnAutoStandbyTimeout(c *librefsm.Context) error
	OnHibernationComplete(c *librefsm.Context) error
	OnLockHibernate(c *librefsm.Context) error // Sets hibernation flag before shutdown
	OnForceLock(c *librefsm.Context) error     // Sets force-standby flag
}

// FSMData holds data passed through the FSM context.
// Stored in librefsm.Context.Data for access in callbacks.
type FSMData struct {
	// PreviousState tracks the state we came from for conditional logic
	PreviousState librefsm.StateID

	// Flags for special transition behaviors
	ForcedStandby      bool // Skip handlebar lock on standby entry
	HibernationRequest bool // Request hibernation after shutdown
	ShutdownFromParked bool // Track if shutdown was initiated from parked

	// Additional context for guards
	DashboardReady bool
}
