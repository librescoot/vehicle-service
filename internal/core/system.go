package core

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/librescoot/librefsm"

	"vehicle-service/internal/fsm"
	"vehicle-service/internal/hardware"
	"vehicle-service/internal/logger"
	"vehicle-service/internal/messaging"
	"vehicle-service/internal/types"
)

// Track blinker state
type BlinkerState int

const (
	BlinkerOff BlinkerState = iota
	BlinkerLeft
	BlinkerRight
	BlinkerBoth
)

const (
	keycardForceStandbyTaps = 3
	keycardTapMaxInterval   = 3 * time.Second // Max interval between taps to be part of a sequence

	// Hardware timing constants
	blinkerInterval       = 800 * time.Millisecond
	handlebarLockDuration = 1100 * time.Millisecond
	handlebarLockWindow   = 10 * time.Second
	seatboxLockDuration   = 200 * time.Millisecond
	parkDebounceTime      = 1 * time.Second

)


type VehicleSystem struct {
	state                  types.SystemState
	dashboardReady         bool
	logger                 *logger.Logger
	io                     *hardware.LinuxHardwareIO
	redis                  *messaging.RedisClient
	mu                     sync.RWMutex
	redisHost              string
	redisPort              int
	blinkerState           BlinkerState
	blinkerStopChan        chan struct{}
	initialized            bool
	handlebarUnlocked      bool        // Track if handlebar has been unlocked in this power cycle
	handlebarTimer         *time.Timer // Timer for handlebar position window
	readyToDriveEntryTime  time.Time   // Track when we entered ready-to-drive state for park debounce
	keycardTapCount        int
	lastKeycardTapTime     time.Time
	forceStandbyNoLock     bool
	hibernationRequest      bool                // Track if hibernation was requested during shutdown
	shutdownFromParked      bool                // Track if shutdown was initiated from parked state
	dbcUpdating             bool                // Track if DBC update is in progress
	deferredDashboardPower  *bool               // Deferred dashboard power state (nil = no change needed)
	brakeHibernationEnabled bool // Track if brake lever hibernation is enabled (default: true)
	autoStandbySeconds      int  // Auto-standby timeout in seconds (0 = disabled)
	hibernationForceTimer   *time.Timer         // Timer for forcing hibernation after 15s of brake hold
	machine                 *librefsm.Machine   // librefsm state machine
}

func NewVehicleSystem(redisHost string, redisPort int, l *logger.Logger) *VehicleSystem {
	return &VehicleSystem{
		state:                   types.StateInit,
		logger:                  l.WithTag("Vehicle"),
		io:                      hardware.NewLinuxHardwareIO(l.WithTag("Hardware")),
		redisHost:               redisHost,
		redisPort:               redisPort,
		blinkerState:            BlinkerOff,
		initialized:             false,
		keycardTapCount:         0,
		forceStandbyNoLock:      false,
		brakeHibernationEnabled: true, // Default to enabled for backward compatibility
		// lastKeycardTapTime will be zero value (time.IsZero() will be true)
	}
}

func (v *VehicleSystem) Start() error {
	v.logger.Infof("Starting vehicle system")

	// Initialize Redis client first (but don't start listeners yet)
	v.redis = messaging.NewRedisClient(v.redisHost, v.redisPort, v.logger.WithTag("Redis"), messaging.Callbacks{
		DashboardCallback: v.handleDashboardReady,
		KeycardCallback:   v.keycardAuthPassed,
		SeatboxCallback:   v.handleSeatboxRequest,
		HornCallback:      v.handleHornRequest,
		BlinkerCallback:   v.handleBlinkerRequest,
		StateCallback:     v.handleStateRequest,
		ForceLockCallback: v.handleForceLockRequest,
		LedCueCallback:    v.handleLedCueRequest,
		LedFadeCallback:   v.handleLedFadeRequest,
		UpdateCallback:    v.handleUpdateRequest,
		HardwareCallback:  v.handleHardwareRequest,
		SettingsCallback:  v.handleSettingsUpdate,
	})

	// Connect to Redis first so we can retrieve saved state
	if err := v.redis.Connect(); err != nil {
		return fmt.Errorf("failed to connect to Redis: %w", err)
	}

	// Load initial state from Redis for restoration after FSM starts
	savedState, err := v.redis.GetVehicleState()
	if err != nil {
		v.logger.Infof("Failed to get saved state from Redis: %v", err)
		savedState = types.StateInit // Default to init if not found
	}

	// Initialize and start librefsm state machine (starts from init state)
	if err := v.initFSM(context.Background()); err != nil {
		return fmt.Errorf("failed to initialize FSM: %w", err)
	}

	// Read initial brake hibernation setting from Redis
	brakeHibernationSetting, err := v.redis.GetHashField("settings", "scooter.brake-hibernation")
	if err != nil {
		v.logger.Warnf("Failed to read brake hibernation setting on startup: %v", err)
		// Continue with default (enabled)
	} else if brakeHibernationSetting != "" {
		v.mu.Lock()
		switch brakeHibernationSetting {
		case "enabled":
			v.brakeHibernationEnabled = true
			v.logger.Infof("Brake hibernation setting on startup: enabled")
		case "disabled":
			v.brakeHibernationEnabled = false
			v.logger.Infof("Brake hibernation setting on startup: disabled")
		default:
			v.logger.Warnf("Unknown brake hibernation setting value on startup: '%s', using default (enabled)", brakeHibernationSetting)
			v.brakeHibernationEnabled = true
		}
		v.mu.Unlock()
	} else {
		v.logger.Infof("No brake hibernation setting found on startup, using default (enabled)")
	}

	// Read initial auto-standby setting from Redis
	autoStandbySetting, err := v.redis.GetHashField("settings", "scooter.auto-standby-seconds")
	if err != nil {
		v.logger.Warnf("Failed to read auto-standby setting on startup: %v", err)
		// Continue with default (0 = disabled)
	} else if autoStandbySetting != "" {
		seconds, parseErr := strconv.Atoi(autoStandbySetting)
		if parseErr != nil {
			v.logger.Warnf("Invalid auto-standby setting value on startup: '%s', using default (0)", autoStandbySetting)
			v.mu.Lock()
			v.autoStandbySeconds = 0
			v.mu.Unlock()
		} else {
			v.mu.Lock()
			v.autoStandbySeconds = seconds
			v.mu.Unlock()
			if seconds > 0 {
				v.logger.Infof("Auto-standby setting on startup: %d seconds", seconds)
			} else {
				v.logger.Infof("Auto-standby setting on startup: disabled")
			}
		}
	} else {
		v.logger.Infof("No auto-standby setting found on startup, using default (disabled)")
	}

	// Check if DBC update is in progress and restore dbcUpdating flag
	// First check the explicit vehicle:dbc-updating flag
	dbcUpdating, err := v.redis.GetDbcUpdating()
	if err != nil {
		v.logger.Warnf("Failed to get DBC updating flag on startup: %v", err)
	} else if dbcUpdating {
		v.logger.Infof("DBC updating flag set on startup, restoring dbcUpdating flag")
		v.mu.Lock()
		v.dbcUpdating = true
		v.mu.Unlock()
	}

	// Also check OTA status as a fallback/secondary check
	dbcStatus, err := v.redis.GetOtaStatus("dbc")
	if err != nil {
		v.logger.Warnf("Failed to get DBC OTA status on startup: %v", err)
	} else if dbcStatus == "downloading" || dbcStatus == "installing" || dbcStatus == "rebooting" {
		v.logger.Infof("DBC update in progress on startup (status=%s), restoring dbcUpdating flag", dbcStatus)
		v.mu.Lock()
		if !v.dbcUpdating {
			v.dbcUpdating = true
			// Sync the Redis flag if it wasn't already set
			if err := v.redis.SetDbcUpdating(true); err != nil {
				v.logger.Warnf("Failed to sync DBC updating flag to Redis: %v", err)
			}
		}
		v.mu.Unlock()
	}

	// Read dashboard power from Redis BEFORE hardware initialization
	// This ensures GPIO starts with correct value (no power interruption)
	if savedState != types.StateInit && savedState != types.StateShuttingDown {
		dashboardPower, err := v.redis.GetDashboardPower()
		if err != nil {
			v.logger.Warnf("Failed to read dashboard power from Redis: %v", err)
			// Fallback to state-based logic
			v.mu.RLock()
			dashboardPower = savedState == types.StateReadyToDrive || savedState == types.StateParked || v.dbcUpdating
			v.mu.RUnlock()
		}
		v.logger.Infof("Setting initial dashboard power: %v", dashboardPower)
		v.io.SetInitialValue("dashboard_power", dashboardPower)

		// Also set engine power initial value
		enginePower := (savedState == types.StateReadyToDrive || savedState == types.StateParked)
		v.io.SetInitialValue("engine_power", enginePower)
	}

	// Reload PWM LED kernel module, log outcomes for diagnostics
	v.logger.Infof("Reloading PWM LED kernel module (imx_pwm_led)")
	if err := exec.Command("rmmod", "imx_pwm_led").Run(); err != nil {
		v.logger.Infof("Warning: Failed to remove PWM LED module: %v", err)
	} else {
		v.logger.Infof("Successfully removed existing imx_pwm_led module (if present)")
	}
	if err := exec.Command("modprobe", "imx_pwm_led").Run(); err != nil {
		return fmt.Errorf("failed to load PWM LED module: %w", err)
	}
	v.logger.Infof("Successfully inserted imx_pwm_led module, waiting for device nodes")

	// Give udev some time to create /dev/pwm_led* nodes (up to 1s)
	for i := 0; i < 10; i++ {
		if _, err := os.Stat("/dev/pwm_led0"); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Initialize hardware (will use initial values set above)
	if err := v.io.Initialize(); err != nil {
		return fmt.Errorf("failed to initialize hardware: %w", err)
	}

	// Restore saved FSM state now that hardware is initialized
	if err := v.restoreFSMState(savedState); err != nil {
		return fmt.Errorf("failed to restore FSM state: %w", err)
	}

	// Check initial handlebar lock sensor state
	handlebarLockSensorRaw, err := v.io.ReadDigitalInput("handlebar_lock_sensor")
	if err != nil {
		v.logger.Infof("Warning: Failed to read initial handlebar lock sensor state: %v", err)
	} else if !handlebarLockSensorRaw { // Invert the logic: true (pressed) means unlocked, false (released) means locked. We want to know if it's locked initially.
		v.handlebarUnlocked = false // Sensor is released (false), meaning it's locked
		v.logger.Infof("Initial state: handlebar is locked")
	} else {
		v.handlebarUnlocked = true // Sensor is pressed (true), meaning it's unlocked
		v.logger.Infof("Initial state: handlebar is unlocked")
	}

	// Note: We do NOT read/sync dashboard power to Redis here
	// Dashboard power is preserved from Redis during power restoration later
	// Only state transitions should change dashboard power in Redis

	// Register input callbacks
	channels := []string{
		"brake_right", "brake_left", "horn_button", "seatbox_button",
		"kickstand", "blinker_right", "blinker_left", "handlebar_lock_sensor",
		"handlebar_position", "seatbox_lock_sensor", "48v_detect",
	}

	v.logger.Infof("Registering input callbacks for %d channels", len(channels))
	for _, ch := range channels {
		switch ch {
		case "handlebar_position":
			v.io.RegisterInputCallback(ch, v.handleHandlebarPosition)
		case "seatbox_lock_sensor":
			v.io.RegisterInputCallback(ch, func(channel string, value bool) error {
				// Update Redis
				if err := v.redis.SetSeatboxLockState(value); err != nil {
					return err
				}
				// Send physical event - FSM decides transitions based on current state
				if value {
					v.logger.Infof("Seatbox closed - sending EvSeatboxClosed")
					v.machine.Send(librefsm.Event{ID: fsm.EvSeatboxClosed})
				}
				return nil
			})
		default:
			v.io.RegisterInputCallback(ch, v.handleInputChange)
		}
		v.logger.Infof("Registered callback for channel: %s", ch)
	}

	// Publish initial sensor states to Redis
	v.logger.Infof("Publishing initial sensor states to Redis")

	// Map sensors to their Redis publisher functions
	sensorPublishers := map[string]func(bool) error{
		"brake_right":           func(val bool) error { return v.redis.SetBrakeState("right", val) },
		"brake_left":            func(val bool) error { return v.redis.SetBrakeState("left", val) },
		"kickstand":             v.redis.SetKickstandState,
		"handlebar_lock_sensor": func(val bool) error { return v.redis.SetHandlebarLockState(!val) }, // Invert: sensor true = unlocked, Redis true = locked
		"seatbox_lock_sensor":   v.redis.SetSeatboxLockState,
		"handlebar_position":    v.redis.SetHandlebarPosition,
	}

	for sensor, publisher := range sensorPublishers {
		value, err := v.io.ReadDigitalInput(sensor)
		if err != nil {
			v.logger.Infof("Warning: Failed to read initial state for %s: %v", sensor, err)
			continue
		}
		v.logger.Infof("Initial state %s: %v", sensor, value)

		if err := publisher(value); err != nil {
			v.logger.Infof("Warning: Failed to publish initial state for %s to Redis: %v", sensor, err)
		}
	}

	// Now that hardware is initialized, set engine brake based on state
	if savedState != types.StateInit && savedState != types.StateShuttingDown {
		// Apply engine brake based on state
		var engineBrake bool
		if savedState == types.StateReadyToDrive {
			// In drive mode, brake follows physical brake levers
			brakeLeft, err := v.io.ReadDigitalInput("brake_left")
			if err != nil {
				return fmt.Errorf("failed to read brake_left: %w", err)
			}
			brakeRight, err := v.io.ReadDigitalInput("brake_right")
			if err != nil {
				return fmt.Errorf("failed to read brake_right: %w", err)
			}
			engineBrake = brakeLeft || brakeRight
		} else {
			// In all other states, brake is always engaged (motor disabled)
			engineBrake = true
		}
		if err := v.io.WriteDigitalOutput("engine_brake", engineBrake); err != nil {
			return fmt.Errorf("failed to set engine brake: %w", err)
		}
	}

	// Play LED cues based on restored state
	if savedState == types.StateReadyToDrive || savedState == types.StateParked {
		// Read brake states to determine which LED cue to play
		brakeLeft, err := v.io.ReadDigitalInput("brake_left")
		if err != nil {
			v.logger.Infof("Failed to read brake_left: %v", err)
			return err
		}
		brakeRight, err := v.io.ReadDigitalInput("brake_right")
		if err != nil {
			v.logger.Infof("Failed to read brake_right: %v", err)
			return err
		}
		brakesPressed := brakeLeft || brakeRight

		if err := v.io.PlayPwmCue(1); err != nil { // STANDBY_TO_PARKED_BRAKE_OFF
			v.logger.Infof("Failed to play LED cue: %v", err)
		}

		if savedState == types.StateReadyToDrive {
			if err := v.io.PlayPwmCue(3); err != nil { // PARKED_TO_DRIVE
				v.logger.Infof("Failed to play LED cue: %v", err)
			}
		}

		if brakesPressed {
			if err := v.io.PlayPwmCue(4); err != nil { // LED_BRAKE_OFF_TO_BRAKE_ON
				v.logger.Infof("Failed to play LED cue: %v", err)
			}
		}
	} else if savedState == types.StateShuttingDown {
		// Restore to ShuttingDown - the FSM timeout will complete the shutdown normally
		if err := v.machine.SetState(fsm.StateShuttingDown); err != nil {
			v.logger.Errorf("Failed to restore shutting-down state: %v", err)
		}
	}

	// Mark system as initialized
	v.initialized = true

	// Now handle the initial dashboard ready state if needed
	v.mu.RLock()
	isReady := v.dashboardReady
	v.mu.RUnlock()
	if isReady {
		if err := v.handleDashboardReady(true); err != nil {
			v.logger.Infof("Failed to handle initial dashboard ready state: %v", err)
		}
	}

	if err := v.publishState(); err != nil {
		return fmt.Errorf("failed to publish initial state: %w", err)
	}

	// FSM's Init state has a 2-second timeout that transitions to Standby
	// if no other event (like EvDashboardReady) triggers first

	// Start Redis listeners now that everything is initialized
	if err := v.redis.StartListening(); err != nil {
		return fmt.Errorf("failed to start Redis listeners: %w", err)
	}

	v.logger.Infof("System started successfully")
	return nil
}

// checkHibernationConditions sends brake events - FSM decides what to do based on state
func (v *VehicleSystem) checkHibernationConditions() {
	// Check if brake hibernation is enabled
	v.mu.RLock()
	hibernationEnabled := v.brakeHibernationEnabled
	v.mu.RUnlock()

	if !hibernationEnabled {
		return
	}

	brakeLeft, err := v.io.ReadDigitalInput("brake_left")
	if err != nil {
		v.logger.Infof("Failed to read brake_left for hibernation check: %v", err)
		return
	}
	brakeRight, err := v.io.ReadDigitalInput("brake_right")
	if err != nil {
		v.logger.Infof("Failed to read brake_right for hibernation check: %v", err)
		return
	}
	bothBrakesPressed := brakeLeft && brakeRight

	// Send pure physical events - FSM decides transitions based on current state
	if bothBrakesPressed {
		v.logger.Debugf("Both brakes pressed - sending EvBrakesPressed")
		v.machine.Send(librefsm.Event{ID: fsm.EvBrakesPressed})
	} else {
		v.logger.Debugf("Brakes released - sending EvBrakesReleased")
		v.machine.Send(librefsm.Event{ID: fsm.EvBrakesReleased})
	}
}

// startAutoStandbyTimer starts the auto-standby timer using librefsm
func (v *VehicleSystem) startAutoStandbyTimer() {
	v.mu.RLock()
	seconds := v.autoStandbySeconds
	v.mu.RUnlock()

	if seconds <= 0 || v.machine == nil {
		return
	}

	v.logger.Infof("Starting auto-standby timer: %d seconds", seconds)

	duration := time.Duration(seconds) * time.Second
	v.machine.StartTimer(fsm.TimerAutoStandby, duration, librefsm.Event{ID: fsm.EvAutoStandbyTimeout})

	// Publish the deadline time so UI can display countdown
	deadline := time.Now().Add(duration)
	if err := v.redis.PublishAutoStandbyDeadline(deadline); err != nil {
		v.logger.Warnf("Failed to publish auto-standby deadline: %v", err)
	}

	v.logger.Infof("Auto-standby timer started successfully")
}

// cancelAutoStandbyTimer cancels the auto-standby timer
func (v *VehicleSystem) cancelAutoStandbyTimer() {
	v.logger.Debugf("Canceling auto-standby timer")

	if v.machine != nil {
		v.machine.StopTimer(fsm.TimerAutoStandby)
	}

	// Clear deadline from Redis
	if err := v.redis.ClearAutoStandbyDeadline(); err != nil {
		v.logger.Warnf("Failed to clear auto-standby deadline: %v", err)
	}

	v.logger.Debugf("Auto-standby timer canceled")
}

// resetAutoStandbyTimer resets the auto-standby timer
func (v *VehicleSystem) resetAutoStandbyTimer() {
	currentState := v.getCurrentState()

	v.mu.RLock()
	autoStandbySeconds := v.autoStandbySeconds
	v.mu.RUnlock()

	// Only reset if in parked state and auto-standby is enabled
	if currentState != types.StateParked || autoStandbySeconds <= 0 {
		return
	}

	v.logger.Debugf("Resetting auto-standby timer")
	v.cancelAutoStandbyTimer()
	v.startAutoStandbyTimer()
}

func (v *VehicleSystem) handleDashboardReady(ready bool) error {
	v.logger.Infof("Handling dashboard ready state: %v", ready)

	// Skip state transitions during initialization
	if !v.initialized {
		v.logger.Infof("Skipping dashboard ready handling - system not yet initialized")
		v.mu.Lock()
		v.dashboardReady = ready
		v.mu.Unlock()
		return nil
	}

	v.mu.Lock()
	v.dashboardReady = ready
	v.mu.Unlock()

	// Send appropriate event - FSM handles transitions based on current state and guards
	if ready {
		v.logger.Infof("Dashboard ready, sending EvDashboardReady")
		v.machine.Send(librefsm.Event{ID: fsm.EvDashboardReady})
	} else {
		v.logger.Infof("Dashboard not ready, sending EvDashboardNotReady")
		v.machine.Send(librefsm.Event{ID: fsm.EvDashboardNotReady})
	}

	return nil
}

func (v *VehicleSystem) handleInputChange(channel string, value bool) error {
	v.logger.Debugf("Input %s => %v", channel, value)

	// Publish button event via PUBSUB for immediate response in UI
	// This happens regardless of vehicle state, letting the UI decide what to do with it
	var buttonEvent string
	var shouldPublish bool = true

	// Set state string based on value
	state := "off"
	if value {
		state = "on"
	}

	switch channel {
	case "horn_button":
		buttonEvent = fmt.Sprintf("horn:%s", state)
	case "seatbox_button":
		buttonEvent = fmt.Sprintf("seatbox:%s", state)
	case "brake_right":
		buttonEvent = fmt.Sprintf("brake:right:%s", state)
	case "brake_left":
		buttonEvent = fmt.Sprintf("brake:left:%s", state)
	// Blinker buttons are handled separately in handleBlinkerChange
	case "blinker_right", "blinker_left":
		shouldPublish = false // Skip for blinkers as they're handled in handleBlinkerChange
	default:
		shouldPublish = false
	}

	if shouldPublish {
		if err := v.redis.PublishButtonEvent(buttonEvent); err != nil {
			v.logger.Infof("Warning: Failed to publish button event: %v", err)
			// Continue with normal processing even if PUBSUB fails
		}
	}

	// First check if we should handle this input in current state
	currentState := v.getCurrentState()

	// Handle inputs that should only work when not in standby
	if currentState == types.StateStandby {
		switch channel {
		case "horn_button", "seatbox_button", "brake_right", "brake_left",
			"blinker_right", "blinker_left":
			v.logger.Infof("Ignoring %s in standby state", channel)
			return nil
		}
	}

	// Physical blinker switch only works in active states (parked, ready-to-drive, waiting states)
	// In other states, blinker control comes from software commands only
	if channel == "blinker_right" || channel == "blinker_left" {
		isActive := currentState == types.StateParked ||
			currentState == types.StateReadyToDrive ||
			currentState == types.StateWaitingSeatbox ||
			currentState == types.StateWaitingHibernation ||
			currentState == types.StateWaitingHibernationAdvanced ||
			currentState == types.StateWaitingHibernationSeatbox ||
			currentState == types.StateWaitingHibernationConfirm

		if !isActive {
			v.logger.Infof("Ignoring physical blinker switch in %s state", currentState)
			return nil
		}
	}

	switch channel {
	case "horn_button":
		if err := v.io.WriteDigitalOutput("horn", value); err != nil {
			return err
		}
		return v.redis.SetHornButton(value)

	case "kickstand":
		v.logger.Infof("Kickstand changed: %v", value)

		// Reset auto-standby timer on kickstand movement
		v.resetAutoStandbyTimer()

		// Update Redis (skip in standby to avoid noise)
		if currentState != types.StateStandby {
			if err := v.redis.SetKickstandState(value); err != nil {
				return err
			}
		}

		// Debounce protection for kickstand down from ready-to-drive
		if value && currentState == types.StateReadyToDrive {
			v.mu.RLock()
			entryTime := v.readyToDriveEntryTime
			v.mu.RUnlock()

			if time.Since(entryTime) < parkDebounceTime {
				v.logger.Infof("Kickstand down ignored - debounce protection")
				return nil
			}
		}

		// Send FSM event - FSM handles all transitions based on current state
		if value {
			v.logger.Infof("Sending EvKickstandDown")
			v.machine.Send(librefsm.Event{ID: fsm.EvKickstandDown})
		} else {
			v.logger.Infof("Sending EvKickstandUp")
			v.machine.Send(librefsm.Event{ID: fsm.EvKickstandUp})
		}

	case "seatbox_button":
		if value {
			// Reset auto-standby timer on seatbox press
			v.resetAutoStandbyTimer()
			// Send physical event - FSM decides transitions based on current state
			v.logger.Infof("Seatbox button pressed - sending EvSeatboxButton")
			v.machine.Send(librefsm.Event{ID: fsm.EvSeatboxButton})
		}

		if err := v.redis.SetSeatboxButton(value); err != nil {
			return err
		}
		// Only open seatbox via button in parked mode
		if value && currentState == types.StateParked {
			return v.openSeatboxLock()
		}

	case "brake_right", "brake_left":
		if value {
			// Reset auto-standby timer on brake press
			v.resetAutoStandbyTimer()

			// Play brake on cue
			if err := v.io.PlayPwmCue(4); err != nil { // LED_BRAKE_OFF_TO_BRAKE_ON
				return err
			}
		} else {
			// Play brake off cue
			if err := v.io.PlayPwmCue(5); err != nil { // LED_BRAKE_ON_TO_BRAKE_OFF
				return err
			}
		}

		// Control engine brake based on state
		v.mu.RLock()
		currentState := v.state
		v.mu.RUnlock()

		// Check if either brake is pressed after this change
		brakeLeft, err := v.io.ReadDigitalInput("brake_left")
		if err != nil {
			return fmt.Errorf("failed to read brake_left: %w", err)
		}
		brakeRight, err := v.io.ReadDigitalInput("brake_right")
		if err != nil {
			return fmt.Errorf("failed to read brake_right: %w", err)
		}

		// Engine brake logic:
		// - In READY_TO_DRIVE: follows brake levers (enable if either pressed)
		// - In all other states: always engaged (motor disabled)
		var engineBrakeEngaged bool
		if currentState == types.StateReadyToDrive {
			engineBrakeEngaged = brakeLeft || brakeRight
		} else {
			engineBrakeEngaged = true // Always engaged (motor disabled) outside drive mode
		}

		if err := v.io.WriteDigitalOutput("engine_brake", engineBrakeEngaged); err != nil {
			return fmt.Errorf("failed to control engine brake: %w", err)
		}

		// Update Redis state
		side := "right"
		if channel == "brake_left" {
			side = "left"
		}
		if err := v.redis.SetBrakeState(side, value); err != nil {
			return fmt.Errorf("failed to set brake state in Redis: %w", err)
		}

		// Check for hibernation conditions (both brakes pressed in parked state)
		v.checkHibernationConditions()

		return nil // Return nil as the brake state was set in Redis

	case "blinker_right", "blinker_left":
		if err := v.handleBlinkerChange(channel, value); err != nil {
			return fmt.Errorf("failed to handle blinker change: %w", err)
		}

	case "handlebar_position":
		if err := v.redis.SetHandlebarPosition(value); err != nil {
			return err
		}

	case "handlebar_lock_sensor":
		// Invert the value: true (pressed) means unlocked, false (released) means locked.
		// Redis stores true for locked, false for unlocked.
		isLocked := !value
		if err := v.redis.SetHandlebarLockState(isLocked); err != nil {
			return err
		}
	}
	return nil
}

func (v *VehicleSystem) handleBlinkerChange(channel string, value bool) error {
	// Stop any existing blinker routine
	if v.blinkerStopChan != nil {
		close(v.blinkerStopChan)
		v.blinkerStopChan = nil
	}

	// Update Redis switch state first
	var switchState string
	var cue int

	if !value {
		// Both blinkers off
		switchState = "off"
		cue = 9 // LED_BLINK_NONE
		if err := v.io.PlayPwmCue(cue); err != nil {
			return err
		}
		if err := v.redis.SetBlinkerSwitch(switchState); err != nil {
			return err
		}

		// Also publish the button event directly via PUBSUB for immediate handling
		if channel == "blinker_right" {
			if err := v.redis.PublishButtonEvent("blinker:right:off"); err != nil {
				v.logger.Infof("Failed to publish blinker right button event: %v", err)
				// Continue with normal processing even if PUBSUB fails
			}
		} else {
			if err := v.redis.PublishButtonEvent("blinker:left:off"); err != nil {
				v.logger.Infof("Failed to publish blinker left button event: %v", err)
				// Continue with normal processing even if PUBSUB fails
			}
		}

		return v.redis.SetBlinkerState(switchState)
	}

	// Handle blinker activation
	if channel == "blinker_right" {
		switchState = "right"
		cue = 11 // LED_BLINK_RIGHT
		v.blinkerState = BlinkerRight

		// Publish button press event via PUBSUB for immediate handling
		if err := v.redis.PublishButtonEvent("blinker:right:on"); err != nil {
			v.logger.Infof("Failed to publish blinker right button event: %v", err)
			// Continue with normal processing even if PUBSUB fails
		}
	} else {
		switchState = "left"
		cue = 10 // LED_BLINK_LEFT
		v.blinkerState = BlinkerLeft

		// Publish button press event via PUBSUB for immediate handling
		if err := v.redis.PublishButtonEvent("blinker:left:on"); err != nil {
			v.logger.Infof("Failed to publish blinker left button event: %v", err)
			// Continue with normal processing even if PUBSUB fails
		}
	}

	// Set switch state when button is pressed
	if err := v.redis.SetBlinkerSwitch(switchState); err != nil {
		return err
	}

	// Start blinker routine that will update state
	v.blinkerStopChan = make(chan struct{})
	go v.runBlinker(cue, switchState, v.blinkerStopChan)

	return nil
}

func (v *VehicleSystem) runBlinker(cue int, state string, stopChan chan struct{}) {
	// Trigger first blink immediately
	if err := v.io.PlayPwmCue(cue); err != nil {
		v.logger.Infof("Error playing blinker cue: %v", err)
	}
	if err := v.redis.SetBlinkerState(state); err != nil {
		v.logger.Infof("Error updating blinker state: %v", err)
	}

	ticker := time.NewTicker(800 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-stopChan:
			return
		case <-ticker.C:
			if err := v.io.PlayPwmCue(cue); err != nil {
				v.logger.Infof("Error playing blinker cue: %v", err)
			}
			if err := v.redis.SetBlinkerState(state); err != nil {
				v.logger.Infof("Error updating blinker state: %v", err)
			}
		}
	}
}

func (v *VehicleSystem) keycardAuthPassed() error {
	v.logger.Infof("Processing keycard authentication tap")

	// --- Force Standby Check (3 taps + brake) ---
	brakeLeft, errL := v.io.ReadDigitalInput("brake_left")
	if errL != nil {
		v.logger.Debugf("Warning: Failed to read brake_left for keycard auth, assuming not pressed: %v", errL)
		brakeLeft = false
	}
	brakeRight, errR := v.io.ReadDigitalInput("brake_right")
	if errR != nil {
		v.logger.Debugf("Warning: Failed to read brake_right for keycard auth, assuming not pressed: %v", errR)
		brakeRight = false
	}
	brakePressed := brakeLeft || brakeRight

	currentTime := time.Now()
	sendForceLock := false

	v.mu.Lock()
	if v.lastKeycardTapTime.IsZero() || currentTime.Sub(v.lastKeycardTapTime) > keycardTapMaxInterval {
		v.keycardTapCount = 1
		v.logger.Debugf("Keycard tap sequence: Start/Reset. Count: 1")
	} else {
		v.keycardTapCount++
		v.logger.Debugf("Keycard tap sequence: Incremented. Count: %d", v.keycardTapCount)
	}
	v.lastKeycardTapTime = currentTime

	if v.keycardTapCount >= keycardForceStandbyTaps {
		if brakePressed {
			v.logger.Infof("Force standby condition met: %d taps, brake pressed", v.keycardTapCount)
			sendForceLock = true
		} else {
			v.logger.Debugf("Force standby condition NOT met: %d taps, but brake not pressed", v.keycardTapCount)
		}
		v.keycardTapCount = 0
	}
	v.mu.Unlock()

	// Send the appropriate event - FSM handles all transition logic
	if sendForceLock {
		v.logger.Infof("Sending EvForceLock")
		return v.machine.SendSync(librefsm.Event{ID: fsm.EvForceLock})
	}

	v.logger.Infof("Sending EvKeycardAuth")
	return v.machine.SendSync(librefsm.Event{ID: fsm.EvKeycardAuth})
}

// readBrakeStates reads both brake states and returns them
func (v *VehicleSystem) readBrakeStates() (left, right bool, err error) {
	left, err = v.io.ReadDigitalInput("brake_left")
	if err != nil {
		return false, false, fmt.Errorf("failed to read brake_left: %w", err)
	}
	right, err = v.io.ReadDigitalInput("brake_right")
	if err != nil {
		return false, false, fmt.Errorf("failed to read brake_right: %w", err)
	}
	return left, right, nil
}

// handleDashboardPowerChange manages the dashboard ready flag when dashboard power changes.
// If we're cutting power to the dashboard, clear the ready flag immediately since the
// dashboard won't have a chance to clear it itself before losing power.
func (v *VehicleSystem) handleDashboardPowerChange(enabled bool) error {
	if !enabled {
		v.mu.RLock()
		currentlyReady := v.dashboardReady
		v.mu.RUnlock()

		if currentlyReady {
			v.logger.Debugf("Clearing dashboard ready flag before power cut")

			v.mu.Lock()
			v.dashboardReady = false
			v.mu.Unlock()

			if err := v.redis.DeleteDashboardReadyFlag(); err != nil {
				v.logger.Warnf("Failed to clear dashboard ready flag: %v", err)
				return err
			}
		}
	}
	return nil
}

// setPower controls a power output (dashboard_power or engine_power) with consistent logging
func (v *VehicleSystem) setPower(component string, enabled bool) error {
	// Handle dashboard ready clearing BEFORE changing power state
	if component == "dashboard_power" {
		if err := v.handleDashboardPowerChange(enabled); err != nil {
			v.logger.Warnf("Failed to handle dashboard power change: %v", err)
			// Continue anyway - don't block power change
		}
	}

	if err := v.io.WriteDigitalOutput(component, enabled); err != nil {
		action := "enable"
		if !enabled {
			action = "disable"
		}
		return fmt.Errorf("failed to %s %s: %w", action, component, err)
	}

	// Persist dashboard_power state to Redis
	if component == "dashboard_power" {
		if err := v.redis.SetDashboardPower(enabled); err != nil {
			v.logger.Warnf("Failed to persist dashboard power state to Redis: %v", err)
			// Don't return error - hardware state was set successfully
		}
	}

	state := "enabled"
	if !enabled {
		state = "disabled"
	}
	v.logger.Debugf("%s %s", component, state)
	return nil
}

// pulseOutput activates an output for a duration then deactivates it
func (v *VehicleSystem) pulseOutput(name string, duration time.Duration) error {
	if err := v.io.WriteDigitalOutput(name, true); err != nil {
		return err
	}
	time.Sleep(duration)
	if err := v.io.WriteDigitalOutput(name, false); err != nil {
		return err
	}
	return nil
}

// playLedCue plays an LED cue and logs any errors with context
func (v *VehicleSystem) playLedCue(cue int, description string) {
	if err := v.io.PlayPwmCue(cue); err != nil {
		v.logger.Infof("Failed to play LED cue %d (%s): %v", cue, description, err)
	}
}

func (v *VehicleSystem) openSeatboxLock() error {
	if err := v.pulseOutput("seatbox_lock", seatboxLockDuration); err != nil {
		return err
	}
	v.logger.Infof("Seatbox lock toggled for 0.2s")
	return nil
}

func (v *VehicleSystem) publishState() error {
	state := v.getCurrentState()

	v.logger.Debugf("Publishing state to Redis: %s", state)
	if err := v.redis.PublishVehicleState(state); err != nil {
		return err
	}
	return nil
}

func (v *VehicleSystem) Shutdown() {
	// Stop blinker if running
	if v.blinkerStopChan != nil {
		close(v.blinkerStopChan)
		v.blinkerStopChan = nil
	}

	// Stop any running timers
	if v.handlebarTimer != nil {
		v.handlebarTimer.Stop()
		v.handlebarTimer = nil
	}

	if v.redis != nil {
		v.redis.Close()
	}
	if v.io != nil {
		v.io.Cleanup()
	}
}

