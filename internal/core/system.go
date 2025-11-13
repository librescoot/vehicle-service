package core

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

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
	shutdownTimerDuration = 3500 * time.Millisecond
	handlebarLockWindow   = 10 * time.Second
	seatboxLockDuration   = 200 * time.Millisecond
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
	shutdownTimer          *time.Timer // Timer for shutting-down to standby transition
	keycardTapCount        int
	lastKeycardTapTime     time.Time
	forceStandbyNoLock     bool
	hibernationRequest     bool  // Track if hibernation was requested during shutdown
	shutdownFromParked     bool  // Track if shutdown was initiated from parked state
	dbcUpdating            bool  // Track if DBC update is in progress
	deferredDashboardPower *bool // Deferred dashboard power state (nil = no change needed)
	brakeHibernationEnabled bool  // Track if brake lever hibernation is enabled (default: true)
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

	if err := v.redis.Connect(); err != nil {
		return fmt.Errorf("failed to connect to Redis: %w", err)
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

	// Check if DBC update is in progress and restore dbcUpdating flag
	dbcStatus, err := v.redis.GetOtaStatus("dbc")
	if err != nil {
		v.logger.Warnf("Failed to get DBC OTA status on startup: %v", err)
	} else if dbcStatus == "downloading" || dbcStatus == "installing" || dbcStatus == "rebooting" {
		v.logger.Infof("DBC update in progress on startup (status=%s), restoring dbcUpdating flag", dbcStatus)
		v.mu.Lock()
		v.dbcUpdating = true
		v.mu.Unlock()
	}

	// Load initial state from Redis
	savedState, err := v.redis.GetVehicleState()
	if err != nil {
		v.logger.Warnf("Failed to get saved state from Redis: %v", err)
		// Continue with default state
	} else if savedState != types.StateInit && savedState != types.StateShuttingDown {
		v.logger.Infof("Restoring saved state from Redis: %s", savedState)
		v.mu.Lock()
		v.state = savedState
		v.mu.Unlock()

		// Set initial GPIO values based on saved state
		// If DBC update is in progress, dashboard must stay ON regardless of state
		v.mu.RLock()
		dashboardPower := savedState == types.StateReadyToDrive || savedState == types.StateParked || v.dbcUpdating
		v.mu.RUnlock()
		v.io.SetInitialValue("dashboard_power", dashboardPower)
		v.io.SetInitialValue("engine_power", savedState == types.StateReadyToDrive)

		// If restoring standby state, set the standby timer for MDB reboot coordination
		if savedState == types.StateStandby {
			v.logger.Infof("Restored standby state - setting standby timer start for MDB reboot coordination")
			if err := v.redis.PublishStandbyTimerStart(); err != nil {
				v.logger.Infof("Warning: Failed to set standby timer start on restore: %v", err)
			}
		}
	}

	// Reload PWM LED kernel module, log outcomes for diagnostics
	v.logger.Debugf("Reloading PWM LED kernel module (imx_pwm_led)")
	if err := exec.Command("rmmod", "imx_pwm_led").Run(); err != nil {
		v.logger.Warnf("Warning: Failed to remove PWM LED module: %v", err)
	} else {
		v.logger.Debugf("Successfully removed existing imx_pwm_led module (if present)")
	}
	if err := exec.Command("modprobe", "imx_pwm_led").Run(); err != nil {
		return fmt.Errorf("failed to load PWM LED module: %w", err)
	}
	v.logger.Debugf("Successfully inserted imx_pwm_led module, waiting for device nodes")

	// Give udev some time to create /dev/pwm_led* nodes (up to 1s)
	for i := 0; i < 10; i++ {
		if _, err := os.Stat("/dev/pwm_led0"); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Initialize hardware
	if err := v.io.Initialize(); err != nil {
		return fmt.Errorf("failed to initialize hardware: %w", err)
	}

	// Check initial handlebar lock sensor state
	handlebarLockSensorRaw, err := v.io.ReadDigitalInput("handlebar_lock_sensor")
	if err != nil {
		v.logger.Warnf("Warning: Failed to read initial handlebar lock sensor state: %v", err)
	} else if !handlebarLockSensorRaw { // Invert the logic: true (pressed) means unlocked, false (released) means locked. We want to know if it's locked initially.
		v.handlebarUnlocked = false // Sensor is released (false), meaning it's locked
		v.logger.Debugf("Initial state: handlebar is locked")
	} else {
		v.handlebarUnlocked = true // Sensor is pressed (true), meaning it's unlocked
		v.logger.Debugf("Initial state: handlebar is unlocked")
	}

	// Register input callbacks
	channels := []string{
		"brake_right", "brake_left", "horn_button", "seatbox_button",
		"kickstand", "blinker_right", "blinker_left", "handlebar_lock_sensor",
		"handlebar_position", "seatbox_lock_sensor", "48v_detect", "ecu_power",
	}

	v.logger.Debugf("Registering input callbacks for %d channels", len(channels))
	for _, ch := range channels {
		switch ch {
		case "handlebar_position":
			v.io.RegisterInputCallback(ch, v.handleHandlebarPosition)
		case "seatbox_lock_sensor":
			v.io.RegisterInputCallback(ch, func(channel string, value bool) error {
				return v.redis.SetSeatboxLockState(value)
			})
		default:
			v.io.RegisterInputCallback(ch, v.handleInputChange)
		}
		v.logger.Debugf("Registered callback for channel: %s", ch)
	}

	// Publish initial sensor states to Redis
	v.logger.Debugf("Publishing initial sensor states to Redis")

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
			v.logger.Warnf("Warning: Failed to read initial state for %s: %v", sensor, err)
			continue
		}
		v.logger.Debugf("Initial state %s: %v", sensor, value)

		if err := publisher(value); err != nil {
			v.logger.Warnf("Warning: Failed to publish initial state for %s to Redis: %v", sensor, err)
		}
	}

	// Now that hardware is initialized, restore state and outputs
	switch savedState {
	case types.StateReadyToDrive, types.StateParked:
		// Read brake states to determine which LED cue to play
		brakeLeft, brakeRight, err := v.readBrakeStates()
		if err != nil {
			v.logger.Errorf("%v", err)
			return err
		}
		brakesPressed := brakeLeft || brakeRight

		v.playLedCue(1, "standby to parked brake off")

		if savedState == types.StateReadyToDrive {
			v.playLedCue(3, "parked to drive")
		}

		if brakesPressed {
			v.playLedCue(4, "brake off to on")
		}
	case types.StateShuttingDown:
		v.transitionTo(types.StateStandby)
	}

	// Mark system as initialized
	v.initialized = true

	// Now handle the initial dashboard ready state if needed
	v.mu.RLock()
	isReady := v.dashboardReady
	v.mu.RUnlock()
	if isReady {
		if err := v.handleDashboardReady(true); err != nil {
			v.logger.Warnf("Failed to handle initial dashboard ready state: %v", err)
		}
	}

	if err := v.publishState(); err != nil {
		return fmt.Errorf("failed to publish initial state: %w", err)
	}

	// If we are still in Init state after initialization, transition to Standby
	if v.getCurrentState() == types.StateInit {
		v.logger.Infof("Initial state is Init, transitioning to Standby")
		if err := v.transitionTo(types.StateStandby); err != nil {
			// Log the error but continue startup, as standby is a safe default
			v.logger.Warnf("Warning: failed to transition to Standby from Init: %v", err)
		}
	}

	// Start Redis listeners now that everything is initialized
	if err := v.redis.StartListening(); err != nil {
		return fmt.Errorf("failed to start Redis listeners: %w", err)
	}

	v.logger.Infof("System started successfully")
	return nil
}

// readBrakeStates reads both brake lever states and returns them
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

// setPower controls a power output (dashboard_power or engine_power) with consistent logging
func (v *VehicleSystem) setPower(component string, enabled bool) error {
	if err := v.io.WriteDigitalOutput(component, enabled); err != nil {
		action := "enable"
		if !enabled {
			action = "disable"
		}
		return fmt.Errorf("failed to %s %s: %w", action, component, err)
	}

	state := "enabled"
	if !enabled {
		state = "disabled"
	}
	v.logger.Debugf("%s %s", component, state)
	return nil
}

// getCurrentState returns the current system state in a thread-safe manner
func (v *VehicleSystem) getCurrentState() types.SystemState {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.state
}

// checkHibernationConditions checks if hibernation should be triggered and sends command to pm-service
func (v *VehicleSystem) checkHibernationConditions() {
	// Only check hibernation conditions in parked state
	if v.getCurrentState() != types.StateParked {
		return
	}

	// Check if brake hibernation is enabled
	v.mu.RLock()
	hibernationEnabled := v.brakeHibernationEnabled
	v.mu.RUnlock()

	if !hibernationEnabled {
		v.logger.Debugf("Brake hibernation is disabled, skipping hibernation check")
		return
	}

	brakeLeft, brakeRight, err := v.readBrakeStates()
	if err != nil {
		v.logger.Errorf("Hibernation check: %v", err)
		return
	}

	// If both brakes are pressed in parked state, send hibernation command to pm-service
	if brakeLeft && brakeRight {
		v.logger.Infof("Both brakes pressed in PARKED state, sending hibernation command to pm-service")
		if err := v.redis.SendCommand("scooter:power", "hibernate-manual"); err != nil {
			v.logger.Errorf("Failed to send hibernate command: %v", err)
		}
	}
}

// triggerShutdownTimeout handles the shutdown timer expiration and transitions to standby
func (v *VehicleSystem) triggerShutdownTimeout() {
	v.logger.Debugf("Shutdown timer expired, transitioning to standby...")

	v.mu.RLock()
	hibernationRequested := v.hibernationRequest
	v.mu.RUnlock()

	// Transition to standby
	if err := v.transitionTo(types.StateStandby); err != nil {
		v.logger.Errorf("Failed to transition to standby after shutdown timeout: %v", err)
		return
	}

	// If hibernation was requested, execute it after the state transition
	if hibernationRequested {
		v.logger.Infof("Hibernation was requested, executing hibernation...")
		if err := v.redis.SendCommand("scooter:power", "hibernate-manual"); err != nil {
			v.logger.Errorf("Failed to send hibernate command after shutdown: %v", err)
		}
	}

	// Reset the timer field after it has fired
	v.mu.Lock()
	v.shutdownTimer = nil
	v.hibernationRequest = false
	v.mu.Unlock()
}

func (v *VehicleSystem) handleDashboardReady(ready bool) error {
	v.logger.Debugf("Handling dashboard ready state: %v", ready)

	// Skip state transitions during initialization
	if !v.initialized {
		v.logger.Debugf("Skipping dashboard ready handling - system not yet initialized")
		v.mu.Lock()
		v.dashboardReady = ready
		v.mu.Unlock()
		return nil
	}

	v.mu.Lock()
	v.dashboardReady = ready
	currentState := v.state
	v.mu.Unlock()

	v.logger.Debugf("Current state: %s, Dashboard ready: %v", currentState, ready)

	if !ready && currentState == types.StateReadyToDrive {
		v.logger.Infof("Dashboard not ready, transitioning to PARKED")
		return v.transitionTo(types.StateParked)
	}

	// Only try to transition to READY_TO_DRIVE if dashboard is ready
	if ready {
		// Don't process kickstand state in STANDBY or SHUTTING_DOWN states
		if currentState == types.StateStandby || currentState == types.StateShuttingDown {
			v.logger.Debugf("Skipping kickstand check in %s state", currentState)
			return nil
		}

		kickstandValue, err := v.io.ReadDigitalInput("kickstand")
		if err != nil {
			v.logger.Errorf("Failed to read kickstand: %v", err)
			return err
		}

		v.logger.Debugf("Kickstand state: %v", kickstandValue)
		if !kickstandValue && v.isReadyToDrive() {
			v.logger.Infof("Dashboard ready and kickstand up, transitioning to READY_TO_DRIVE")
			return v.transitionTo(types.StateReadyToDrive)
		} else if kickstandValue {
			v.logger.Debugf("Kickstand down, staying in/transitioning to PARKED")
			return v.transitionTo(types.StateParked)
		}
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
			v.logger.Warnf("Warning: Failed to publish button event: %v", err)
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
			v.logger.Debugf("Ignoring %s in standby state", channel)
			return nil
		}
	}

	// Check for manual ready-to-drive activation when in parked state
	if currentState == types.StateParked && channel == "seatbox_button" && value {
		// Check if dashboard has not signaled readiness
		v.mu.RLock()
		dashboardReady := v.dashboardReady
		v.mu.RUnlock()

		if !dashboardReady {
			// Check if kickstand is up
			kickstandValue, err := v.io.ReadDigitalInput("kickstand")
			if err != nil {
				v.logger.Errorf("Failed to read kickstand: %v", err)
				return err
			}

			if !kickstandValue {
				// Check if both brakes are held
				brakeLeft, brakeRight, err := v.readBrakeStates()
				if err != nil {
					v.logger.Errorf("%v", err)
					return err
				}

				if brakeLeft && brakeRight {
					v.logger.Infof("Manual ready-to-drive activation: kickstand up, both brakes held, seatbox button pressed")

					// Blink the main light once for confirmation
					v.playLedCue(3, "parked to drive")

					// Set the scooter to ready-to-drive
					return v.transitionTo(types.StateReadyToDrive)
				}
			}
		}
	}

	switch channel {
	case "horn_button":
		if err := v.io.WriteDigitalOutput("horn", value); err != nil {
			return err
		}
		return v.redis.SetHornButton(value)

	case "kickstand":
		v.logger.Debugf("Kickstand changed: %v, current state: %s", value, currentState)
		if currentState == types.StateStandby {
			v.logger.Debugf("Ignoring kickstand change in %s state", currentState)
			return nil
		}
		if err := v.redis.SetKickstandState(value); err != nil {
			return err
		}
		if value {
			// Kickstand down - always go to PARKED
			v.logger.Infof("Kickstand down, transitioning to PARKED")
			return v.transitionTo(types.StateParked)
		} else if v.isReadyToDrive() {
			// Kickstand up and dashboard ready - go to READY_TO_DRIVE
			v.logger.Infof("Kickstand up and dashboard ready, transitioning to READY_TO_DRIVE")
			return v.transitionTo(types.StateReadyToDrive)
		}

	case "seatbox_button":
		if err := v.redis.SetSeatboxButton(value); err != nil {
			return err
		}
		// Only open seatbox via button in parked mode
		if value && currentState == types.StateParked {
			return v.openSeatboxLock()
		}

	case "brake_right", "brake_left":
		if value {
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

		// Control engine brake in drive mode
		v.mu.RLock()
		inDriveMode := v.state == types.StateReadyToDrive
		v.mu.RUnlock()

		if inDriveMode {
			// Check if either brake is pressed after this change
			brakeLeft, brakeRight, err := v.readBrakeStates()
			if err != nil {
				return err
			}

			// Enable engine brake if either brake is pressed, disable if both are released
			if err := v.io.WriteDigitalOutput("engine_brake", brakeLeft || brakeRight); err != nil {
				return fmt.Errorf("failed to control engine brake: %w", err)
			}
		}

		// Update Redis state
		side := "right"
		if channel == "brake_left" {
			side = "left"
		}
		if err := v.redis.SetBrakeState(side, value); err != nil {
			return fmt.Errorf("failed to set brake state in Redis: %w", err)
		}

		// Check for immediate hibernation trigger (both brakes pressed in parked state)
		// pm-service will handle the hibernation timer, vehicle-service just detects the condition
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
				v.logger.Warnf("Failed to publish blinker right button event: %v", err)
				// Continue with normal processing even if PUBSUB fails
			}
		} else {
			if err := v.redis.PublishButtonEvent("blinker:left:off"); err != nil {
				v.logger.Warnf("Failed to publish blinker left button event: %v", err)
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
			v.logger.Warnf("Failed to publish blinker right button event: %v", err)
			// Continue with normal processing even if PUBSUB fails
		}
	} else {
		switchState = "left"
		cue = 10 // LED_BLINK_LEFT
		v.blinkerState = BlinkerLeft

		// Publish button press event via PUBSUB for immediate handling
		if err := v.redis.PublishButtonEvent("blinker:left:on"); err != nil {
			v.logger.Warnf("Failed to publish blinker left button event: %v", err)
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
		v.logger.Errorf("Error playing blinker cue: %v", err)
	}
	if err := v.redis.SetBlinkerState(state); err != nil {
		v.logger.Errorf("Error updating blinker state: %v", err)
	}

	ticker := time.NewTicker(blinkerInterval)
	defer ticker.Stop()

	for {
		select {
		case <-stopChan:
			return
		case <-ticker.C:
			if err := v.io.PlayPwmCue(cue); err != nil {
				v.logger.Errorf("Error playing blinker cue: %v", err)
			}
			if err := v.redis.SetBlinkerState(state); err != nil {
				v.logger.Errorf("Error updating blinker state: %v", err)
			}
		}
	}
}

func (v *VehicleSystem) keycardAuthPassed() error {
	v.logger.Debugf("Processing keycard authentication tap")

	// --- Force Standby Check ---
	brakeLeft, brakeRight, err := v.readBrakeStates()
	if err != nil {
		v.logger.Warnf("Warning: %v for keycard auth, assuming not pressed", err)
		brakeLeft, brakeRight = false, false
	}
	brakePressed := brakeLeft || brakeRight

	currentTime := time.Now()
	performForcedStandby := false

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
			v.logger.Infof("Force standby condition met: %d taps, brake pressed.", v.keycardTapCount)
			v.forceStandbyNoLock = true
			performForcedStandby = true
		} else {
			v.logger.Debugf("Force standby condition NOT met: %d taps, but brake not pressed. Resetting count.", v.keycardTapCount)
		}
		v.keycardTapCount = 0 // Reset after 3 taps, regardless of brake, for the next sequence
	}
	v.mu.Unlock()

	if performForcedStandby {
		v.logger.Infof("Transitioning to SHUTTING_DOWN (forced, no lock).")
		// The forceStandbyNoLock flag will be read and reset by transitionTo when transitioning to standby
		return v.transitionTo(types.StateShuttingDown)
	}
	// --- End Force Standby Check ---

	// ----- Original keycardAuthPassed logic continues if not forced standby -----
	currentState := v.getCurrentState()

	v.logger.Debugf("Current state during keycard auth (normal flow): %s", currentState)

	if currentState == types.StateStandby {
		v.logger.Debugf("Reading kickstand state")
		kickstandValue, err := v.io.ReadDigitalInput("kickstand")
		if err != nil {
			v.logger.Errorf("Failed to read kickstand: %v", err)
			return fmt.Errorf("failed to read kickstand: %w", err)
		}

		newState := types.StateReadyToDrive
		if kickstandValue {
			newState = types.StateParked
		}
		v.logger.Infof("Transitioning from STANDBY to %s (kickstand=%v)", newState, kickstandValue)
		return v.transitionTo(newState)
	}

	// Check kickstand before going to STANDBY
	kickstandValue, err := v.io.ReadDigitalInput("kickstand")
	if err != nil {
		v.logger.Errorf("Failed to read kickstand: %v", err)
		return fmt.Errorf("failed to read kickstand: %w", err)
	}
	if !kickstandValue {
		v.logger.Warnf("Cannot transition to STANDBY: kickstand not down")
		return fmt.Errorf("cannot transition to STANDBY: kickstand not down")
	}

	v.logger.Infof("Transitioning to SHUTTING_DOWN from %s", currentState)
	if err := v.transitionTo(types.StateShuttingDown); err != nil {
		return err
	}

	return nil
}

func (v *VehicleSystem) isReadyToDrive() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.dashboardReady
}

// pulseOutput activates an output for a specified duration then deactivates it
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
	if v.shutdownTimer != nil {
		v.shutdownTimer.Stop()
		v.shutdownTimer = nil
	}

	if v.redis != nil {
		v.redis.Close()
	}
	if v.io != nil {
		v.io.Cleanup()
	}
}
