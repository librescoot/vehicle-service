package fsm

import "github.com/librescoot/librefsm"

// Vehicle states
const (
	StateInit           librefsm.StateID = "init"
	StateStandby        librefsm.StateID = "stand-by"
	StateParked         librefsm.StateID = "parked"
	StateReadyToDrive   librefsm.StateID = "ready-to-drive"
	StateWaitingSeatbox librefsm.StateID = "waiting-seatbox"
	StateShuttingDown   librefsm.StateID = "shutting-down"
	StateUpdating       librefsm.StateID = "updating"

	// Hibernation parent state and substates (hierarchical)
	StateHibernation                librefsm.StateID = "hibernation"
	StateHibernationInitialHold     librefsm.StateID = "hibernation-initial-hold"
	StateHibernationAwaitingConfirm librefsm.StateID = "hibernation-awaiting-confirm"
	StateHibernationSeatbox         librefsm.StateID = "hibernation-seatbox"
	StateHibernationConfirm         librefsm.StateID = "hibernation-confirm"

	// At-rest parent grouping the parked-family leaves. Owns the
	// auto-standby timer so sibling transitions (Parked <-> HopOn <->
	// HopOnLearning) don't tear down or re-arm it.
	StateAtRest librefsm.StateID = "at-rest"

	// Hop-on / hop-off mode (locked): scooter stays powered up but every
	// physical input is dropped at the FSM level via BlockedEvents (no
	// horn, no blinker, no kickstand->RTD, no seatbox open). The dashboard
	// renders a lock screen; only an explicit release combo, mobile-app
	// unlock, or a standard escape (lock/keycard/force-lock/auto-standby)
	// exits.
	StateHopOn librefsm.StateID = "hop-on"

	// Hop-on combo learning: same input gating as StateHopOn but no
	// user-facing side-effects (no LED cue, no steering-lock attempt, no
	// lock-screen). Used while the dashboard records the user's combo.
	StateHopOnLearning librefsm.StateID = "hop-on-learning"
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
	EvDbcUpdateComplete librefsm.EventID = "dbc-update-complete"

	// Physical inputs
	EvKickstandDown  librefsm.EventID = "kickstand-down"
	EvKickstandUp    librefsm.EventID = "kickstand-up"
	EvBrakesPressed  librefsm.EventID = "brakes-pressed"
	EvBrakesReleased librefsm.EventID = "brakes-released"
	EvSeatboxButton  librefsm.EventID = "seatbox-button"

	// Timer events
	EvInitTimeout               librefsm.EventID = "init-timeout"
	EvShutdownTimeout           librefsm.EventID = "shutdown-timeout"
	EvAutoStandbyTimeout        librefsm.EventID = "auto-standby-timeout"
	EvWaitingSeatboxTimeout     librefsm.EventID = "waiting-seatbox-timeout"
	EvHibernationInitialTimeout librefsm.EventID = "hibernation-initial-timeout"
	EvHibernationConfirmTimeout librefsm.EventID = "hibernation-confirm-timeout"
	EvHibernationForceTimeout   librefsm.EventID = "hibernation-force-timeout"
	EvHibernationFinalTimeout   librefsm.EventID = "hibernation-final-timeout"

	// Seatbox events
	EvSeatboxClosed librefsm.EventID = "seatbox-closed"

	// Hop-on / hop-off mode events
	EvHopOnEngage         librefsm.EventID = "hop-on-engage"
	EvHopOnLearningEngage librefsm.EventID = "hop-on-learning-engage"
	EvHopOnRelease        librefsm.EventID = "hop-on-release"
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
