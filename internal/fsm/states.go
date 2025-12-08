package fsm

import "github.com/librescoot/librefsm"

// Vehicle states
const (
	StateInit                       librefsm.StateID = "init"
	StateStandby                    librefsm.StateID = "stand-by"
	StateParked                     librefsm.StateID = "parked"
	StateReadyToDrive               librefsm.StateID = "ready-to-drive"
	StateWaitingSeatbox             librefsm.StateID = "waiting-seatbox"
	StateShuttingDown               librefsm.StateID = "shutting-down"
	StateUpdating                   librefsm.StateID = "updating"

	// Hibernation parent state and substates (hierarchical)
	StateHibernation                librefsm.StateID = "hibernation"
	StateHibernationInitialHold     librefsm.StateID = "hibernation-initial-hold"
	StateHibernationAwaitingConfirm librefsm.StateID = "hibernation-awaiting-confirm"
	StateHibernationSeatbox         librefsm.StateID = "hibernation-seatbox"
	StateHibernationConfirm         librefsm.StateID = "hibernation-confirm"
)

// Vehicle events
const (
	// External commands (from Redis)
	EvUnlock            librefsm.EventID = "unlock"
	EvLock              librefsm.EventID = "lock"
	EvLockHibernate     librefsm.EventID = "lock-hibernate"
	EvForceLock         librefsm.EventID = "force-lock"
	EvDashboardReady    librefsm.EventID = "dashboard-ready"
	EvDashboardNotReady librefsm.EventID = "dashboard-not-ready"
	EvKeycardAuth       librefsm.EventID = "keycard-auth"
	EvUpdateStart       librefsm.EventID = "update-start"
	EvUpdateComplete    librefsm.EventID = "update-complete"

	// Physical inputs
	EvKickstandDown   librefsm.EventID = "kickstand-down"
	EvKickstandUp     librefsm.EventID = "kickstand-up"
	EvBrakesPressed   librefsm.EventID = "brakes-pressed"
	EvBrakesReleased  librefsm.EventID = "brakes-released"
	EvSeatboxButton   librefsm.EventID = "seatbox-button"

	// Timer events
	EvShutdownTimeout           librefsm.EventID = "shutdown-timeout"
	EvAutoStandbyTimeout        librefsm.EventID = "auto-standby-timeout"
	EvHibernationInitialTimeout librefsm.EventID = "hibernation-initial-timeout"
	EvHibernationConfirmTimeout librefsm.EventID = "hibernation-confirm-timeout"
	EvHibernationForceTimeout   librefsm.EventID = "hibernation-force-timeout"
	EvHibernationFinalTimeout   librefsm.EventID = "hibernation-final-timeout"

	// Hibernation control
	EvHibernationStart   librefsm.EventID = "hibernation-start"
	EvHibernationCancel  librefsm.EventID = "hibernation-cancel"
	EvHibernationConfirm librefsm.EventID = "hibernation-confirm"
	EvSeatboxClosed      librefsm.EventID = "seatbox-closed"
)

// Timer names for imperative timers
const (
	TimerShutdown           = "shutdown"
	TimerAutoStandby        = "auto-standby"
	TimerHandlebarLock      = "handlebar-lock"
	TimerHibernationInitial = "hibernation-initial"
	TimerHibernationConfirm = "hibernation-confirm"
	TimerHibernationForce   = "hibernation-force"
	TimerHibernationFinal   = "hibernation-final"
)
