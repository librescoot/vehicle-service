package types

type SystemState string

const (
	StateInit               SystemState = "init"
	StateStandby            SystemState = "stand-by"
	StateParked             SystemState = "parked"
	StateReadyToDrive       SystemState = "ready-to-drive"
	StateWaitingSeatbox     SystemState = "waiting-seatbox"
	StateShuttingDown       SystemState = "shutting-down"
	StateWaitingHibernation SystemState = "waiting-hibernation"
)
