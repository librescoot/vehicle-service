package core

import (
	"time"

	"vehicle-service/internal/hardware"
	"vehicle-service/internal/messaging"
	"vehicle-service/internal/types"
)

// MessagingClient defines the interface for Redis messaging operations needed by VehicleSystem
type MessagingClient interface {
	SetCallbacks(callbacks messaging.Callbacks)
	Connect() error
	StartListening() error
	Close() error

	// State management
	GetVehicleState() (types.SystemState, error)
	PublishVehicleState(state types.SystemState) error

	// Dashboard
	GetDashboardPower() (bool, error)
	SetDashboardPower(enabled bool) error
	DeleteDashboardReadyFlag() error

	// OTA/DBC
	GetDbcUpdating() (bool, error)
	SetDbcUpdating(updating bool) error
	GetOtaStatus(component string) (string, error)

	// Settings
	GetHashField(hash, field string) (string, error)

	// Auto-standby
	PublishAutoStandbyDeadline(deadline time.Time) error
	ClearAutoStandbyDeadline() error
	PublishStandbyTimerStart() error

	// Sensors and switches
	SetBrakeState(side string, pressed bool) error
	SetKickstandState(deployed bool) error
	SetHandlebarLockState(locked bool) error
	SetHandlebarPosition(isOnPlace bool) error
	SetSeatboxLockState(locked bool) error
	SetBlinkerSwitch(state string) error
	SetBlinkerState(state string) error
	SetHornButton(pressed bool) error
	SetSeatboxButton(pressed bool) error

	// Events
	PublishButtonEvent(event string) error
	PublishSeatboxOpened() error

	// Commands
	SendCommand(channel, command string) error
}

// HardwareIO defines the interface for hardware I/O operations needed by VehicleSystem
type HardwareIO interface {
	Initialize() error
	Cleanup()

	// Digital I/O
	ReadDigitalInput(channel string) (bool, error)
	WriteDigitalOutput(channel string, value bool) error
	SetInitialValue(name string, value bool)
	RegisterInputCallback(channel string, callback hardware.InputCallback)

	// PWM control
	PlayPwmCue(idx int) error
	PlayPwmFade(ch int, idx int) error
}
