package fsm

import "github.com/librescoot/librefsm"

// Actions is implemented by VehicleSystem for FSM callbacks and guards.
type Actions interface {
	EnterReadyToDrive(c *librefsm.Context) error
	EnterParked(c *librefsm.Context) error
	EnterStandby(c *librefsm.Context) error
	EnterShuttingDown(c *librefsm.Context) error
	EnterWaitingSeatbox(c *librefsm.Context) error

	// At-rest parent: owns the auto-standby timer for the parked family.
	EnterAtRest(c *librefsm.Context) error
	ExitAtRest(c *librefsm.Context) error

	EnterHibernation(c *librefsm.Context) error
	ExitHibernation(c *librefsm.Context) error
	EnterHibernationInitialHold(c *librefsm.Context) error
	EnterHibernationAwaitingConfirm(c *librefsm.Context) error
	EnterHibernationSeatbox(c *librefsm.Context) error
	EnterHibernationConfirm(c *librefsm.Context) error

	EnterHopOn(c *librefsm.Context) error
	ExitHopOn(c *librefsm.Context) error
	EnterHopOnLearning(c *librefsm.Context) error
	ExitHopOnLearning(c *librefsm.Context) error

	CanEnterReadyToDrive(c *librefsm.Context) bool // Requires both dashboard readiness and raised kickstand.
	IsDashboardReady(c *librefsm.Context) bool
	IsKickstandUp(c *librefsm.Context) bool
	IsKickstandDown(c *librefsm.Context) bool
	IsSeatboxClosed(c *librefsm.Context) bool
	AreBrakesPressed(c *librefsm.Context) bool
	IsHandlebarUnlocked(c *librefsm.Context) bool
	CanAbortShutdown(c *librefsm.Context) bool // Only before the DBC has been told to halt.
	IsBrakeHibernationEnabled(c *librefsm.Context) bool

	OnShutdownTimeout(c *librefsm.Context) error
	OnAutoStandbyTimeout(c *librefsm.Context) error
	OnHibernationComplete(c *librefsm.Context) error
	OnLockHibernate(c *librefsm.Context) error // Requests hibernation before shutdown.
	OnForceLock(c *librefsm.Context) error     // Forces standby.
	OnSeatboxButton(c *librefsm.Context) error
}

// FSMData carries transition history and one-shot control flags in Context.Data.
type FSMData struct {
	PreviousState librefsm.StateID

	ForcedStandby      bool // Skip handlebar locking on standby entry.
	HibernationRequest bool // Hibernate after shutdown.
	ShutdownFromParked bool // Shutdown originated while parked.

	DashboardReady bool
}
