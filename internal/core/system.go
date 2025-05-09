package core

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

	"vehicle-service/internal/hardware"
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
)

type VehicleSystem struct {
	state              types.SystemState
	dashboardReady     bool
	logger             *log.Logger
	io                 *hardware.LinuxHardwareIO
	redis              *messaging.RedisClient
	mu                 sync.RWMutex
	redisHost          string
	redisPort          int
	blinkerState       BlinkerState
	blinkerStopChan    chan struct{}
	initialized        bool
	handlebarUnlocked  bool        // Track if handlebar has been unlocked in this power cycle
	handlebarTimer     *time.Timer // Timer for handlebar position window
	hibernationTimer   *time.Timer // Timer for brake-triggered hibernation
	keycardTapCount    int
	lastKeycardTapTime time.Time
	forceStandbyNoLock bool
	pendingPowerCmd    string            // Queued power command during updates
	dbcUpdating        bool              // Track if DBC is currently updating
	previousState      types.SystemState // Previous state before entering updating state
}

func NewVehicleSystem(redisHost string, redisPort int) *VehicleSystem {
	return &VehicleSystem{
		state:              types.StateInit,
		logger:             log.New(log.Writer(), "Vehicle: ", log.LstdFlags),
		io:                 hardware.NewLinuxHardwareIO(),
		redisHost:          redisHost,
		redisPort:          redisPort,
		blinkerState:       BlinkerOff,
		initialized:        false,
		keycardTapCount:    0,
		forceStandbyNoLock: false,
		// lastKeycardTapTime will be zero value (time.IsZero() will be true)
	}
}

func (v *VehicleSystem) Start() error {
	v.logger.Printf("Starting vehicle system")

	// Initialize Redis client first (but don't start listeners yet)
	v.redis = messaging.NewRedisClient(v.redisHost, v.redisPort, messaging.Callbacks{
		DashboardCallback: v.handleDashboardReady,
		KeycardCallback:   v.keycardAuthPassed,
		SeatboxCallback:   v.handleSeatboxRequest,
		HornCallback:      v.handleHornRequest,
		BlinkerCallback:   v.handleBlinkerRequest,
		PowerCallback:     v.handlePowerRequest,
		StateCallback:     v.handleStateRequest,
		ForceLockCallback: v.handleForceLockRequest,
		LedCueCallback:    v.handleLedCueRequest,
		LedFadeCallback:   v.handleLedFadeRequest,
		UpdateCallback:    v.handleUpdateRequest,
	})

	if err := v.redis.Connect(); err != nil {
		return fmt.Errorf("failed to connect to Redis: %w", err)
	}

	// Load initial state from Redis
	savedState, err := v.redis.GetVehicleState()
	if err != nil {
		v.logger.Printf("Failed to get saved state from Redis: %v", err)
		// Continue with default state
	} else if savedState != types.StateInit && savedState != types.StateShuttingDown {
		v.logger.Printf("Restoring saved state from Redis: %s", savedState)
		v.mu.Lock()
		v.state = savedState
		v.mu.Unlock()

		// Set initial GPIO values based on saved state
		if savedState == types.StateReadyToDrive || savedState == types.StateParked {
			v.io.SetInitialValue("dashboard_power", true)
			if savedState == types.StateReadyToDrive {
				v.io.SetInitialValue("engine_power", true)
			}
		}
	}

	// Reload PWM LED kernel module, log outcomes for diagnostics
	v.logger.Printf("Reloading PWM LED kernel module (imx_pwm_led)")
	if err := exec.Command("rmmod", "imx_pwm_led").Run(); err != nil {
		v.logger.Printf("Warning: Failed to remove PWM LED module: %v", err)
	} else {
		v.logger.Printf("Successfully removed existing imx_pwm_led module (if present)")
	}
	if err := exec.Command("modprobe", "imx_pwm_led").Run(); err != nil {
		return fmt.Errorf("failed to load PWM LED module: %w", err)
	}
	v.logger.Printf("Successfully inserted imx_pwm_led module, waiting for device nodes")

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
		v.logger.Printf("Warning: Failed to read initial handlebar lock sensor state: %v", err)
	} else if !handlebarLockSensorRaw { // Invert the logic: true (pressed) means unlocked, false (released) means locked. We want to know if it's locked initially.
		v.handlebarUnlocked = false // Sensor is released (false), meaning it's locked
		v.logger.Printf("Initial state: handlebar is locked")
	} else {
		v.handlebarUnlocked = true // Sensor is pressed (true), meaning it's unlocked
		v.logger.Printf("Initial state: handlebar is unlocked")
	}

	// Register input callbacks
	channels := []string{
		"brake_right", "brake_left", "horn_button", "seatbox_button",
		"kickstand", "blinker_right", "blinker_left", "handlebar_lock_sensor",
		"handlebar_position", "seatbox_lock_sensor", "48v_detect",
	}

	v.logger.Printf("Registering input callbacks for %d channels", len(channels))
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
		v.logger.Printf("Registered callback for channel: %s", ch)
	}

	// Publish initial sensor states to Redis
	v.logger.Printf("Publishing initial sensor states to Redis")
	initialSensors := []string{
		"brake_right", "brake_left", "kickstand",
		"handlebar_lock_sensor", "seatbox_lock_sensor", "handlebar_position",
		// "48v_detect", // Add if a corresponding Redis key/method exists
	}
	for _, sensor := range initialSensors {
		value, err := v.io.ReadDigitalInput(sensor)
		if err != nil {
			v.logger.Printf("Warning: Failed to read initial state for %s: %v", sensor, err)
			continue
		}
		v.logger.Printf("Initial state %s: %v", sensor, value)

		var redisErr error
		switch sensor {
		case "brake_right":
			redisErr = v.redis.SetBrakeState("right", value)
		case "brake_left":
			redisErr = v.redis.SetBrakeState("left", value)
		case "kickstand":
			redisErr = v.redis.SetKickstandState(value)
		case "handlebar_lock_sensor":
			isLocked := !value // Invert logic: sensor true (pressed) = unlocked, Redis true = locked
			redisErr = v.redis.SetHandlebarLockState(isLocked)
		case "seatbox_lock_sensor":
			redisErr = v.redis.SetSeatboxLockState(value)
		case "handlebar_position":
			redisErr = v.redis.SetHandlebarPosition(value)
		}

		if redisErr != nil {
			v.logger.Printf("Warning: Failed to publish initial state for %s to Redis: %v", sensor, redisErr)
		}
	}

	// Now that hardware is initialized, restore state and outputs
	if savedState == types.StateReadyToDrive || savedState == types.StateParked {
		// Read brake states to determine which LED cue to play
		brakeLeft, err := v.io.ReadDigitalInput("brake_left")
		if err != nil {
			v.logger.Printf("Failed to read brake_left: %v", err)
			return err
		}
		brakeRight, err := v.io.ReadDigitalInput("brake_right")
		if err != nil {
			v.logger.Printf("Failed to read brake_right: %v", err)
			return err
		}
		brakesPressed := brakeLeft || brakeRight

		if err := v.io.PlayPwmCue(1); err != nil { // STANDBY_TO_PARKED_BRAKE_OFF
			v.logger.Printf("Failed to play LED cue: %v", err)
		}

		if savedState == types.StateReadyToDrive {
			if err := v.io.PlayPwmCue(3); err != nil { // PARKED_TO_DRIVE
				v.logger.Printf("Failed to play LED cue: %v", err)
			}
		}

		if brakesPressed {
			if err := v.io.PlayPwmCue(4); err != nil { // LED_BRAKE_OFF_TO_BRAKE_ON
				v.logger.Printf("Failed to play LED cue: %v", err)
			}
		}
	} else if savedState == types.StateShuttingDown {
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
			v.logger.Printf("Failed to handle initial dashboard ready state: %v", err)
		}
	}

	if err := v.publishState(); err != nil {
		return fmt.Errorf("failed to publish initial state: %w", err)
	}

	// If we are still in Init state after initialization, transition to Standby
	v.mu.RLock()
	currentState := v.state
	v.mu.RUnlock()
	if currentState == types.StateInit {
		v.logger.Printf("Initial state is Init, transitioning to Standby")
		if err := v.transitionTo(types.StateStandby); err != nil {
			// Log the error but continue startup, as standby is a safe default
			v.logger.Printf("Warning: failed to transition to Standby from Init: %v", err)
		}
	}

	// Start Redis listeners now that everything is initialized
	if err := v.redis.StartListening(); err != nil {
		return fmt.Errorf("failed to start Redis listeners: %w", err)
	}

	v.logger.Printf("System started successfully")
	return nil
}

// triggerHibernation checks conditions and initiates hibernation if met.
func (v *VehicleSystem) triggerHibernation() {
	v.logger.Printf("Hibernation timer expired, checking conditions...")

	brakeLeft, err := v.io.ReadDigitalInput("brake_left")
	if err != nil {
		v.logger.Printf("Failed to read brake_left for hibernation trigger: %v", err)
		return
	}
	brakeRight, err := v.io.ReadDigitalInput("brake_right")
	if err != nil {
		v.logger.Printf("Failed to read brake_right for hibernation trigger: %v", err)
		return
	}

	v.mu.RLock()
	currentState := v.state
	v.mu.RUnlock()

	if brakeLeft && brakeRight && currentState == types.StateParked {
		v.logger.Printf("Conditions met, initiating hibernation.")
		if err := v.handlePowerRequest("hibernate-manual"); err != nil {
			v.logger.Printf("Failed to execute hibernate from timer: %v", err)
		}
	} else {
		v.logger.Printf("Conditions not met for hibernation.")
	}

	// Reset the timer field after it has fired
	v.mu.Lock()
	v.hibernationTimer = nil
	v.mu.Unlock()
}

func (v *VehicleSystem) handleDashboardReady(ready bool) error {
	v.logger.Printf("Handling dashboard ready state: %v", ready)

	// Skip state transitions during initialization
	if !v.initialized {
		v.logger.Printf("Skipping dashboard ready handling - system not yet initialized")
		v.mu.Lock()
		v.dashboardReady = ready
		v.mu.Unlock()
		return nil
	}

	v.mu.Lock()
	v.dashboardReady = ready
	currentState := v.state
	v.mu.Unlock()

	v.logger.Printf("Current state: %s, Dashboard ready: %v", currentState, ready)

	if !ready && currentState == types.StateReadyToDrive {
		v.logger.Printf("Dashboard not ready, transitioning to PARKED")
		return v.transitionTo(types.StateParked)
	}

	// Only try to transition to READY_TO_DRIVE if dashboard is ready
	if ready {
		// Don't process kickstand state in STANDBY or UPDATING state
		if currentState == types.StateStandby || currentState == types.StateUpdating {
			v.logger.Printf("Skipping kickstand check in %s state", currentState)
			return nil
		}

		kickstandValue, err := v.io.ReadDigitalInput("kickstand")
		if err != nil {
			v.logger.Printf("Failed to read kickstand: %v", err)
			return err
		}

		v.logger.Printf("Kickstand state: %v", kickstandValue)
		if !kickstandValue && v.isReadyToDrive() {
			v.logger.Printf("Dashboard ready and kickstand up, transitioning to READY_TO_DRIVE")
			return v.transitionTo(types.StateReadyToDrive)
		} else if kickstandValue {
			v.logger.Printf("Kickstand down, staying in/transitioning to PARKED")
			return v.transitionTo(types.StateParked)
		}
	}

	return nil
}

func (v *VehicleSystem) handleInputChange(channel string, value bool) error {
	v.logger.Printf("Input %s => %v", channel, value)

	// First check if we should handle this input in current state
	v.mu.RLock()
	currentState := v.state
	v.mu.RUnlock()

	// Handle inputs that should only work when not in standby / updating
	if currentState == types.StateStandby || currentState == types.StateUpdating {
		switch channel {
		case "horn_button", "seatbox_button", "brake_right", "brake_left",
			"blinker_right", "blinker_left":
			v.logger.Printf("Ignoring %s in standby state", channel)
			return nil
		}
	}

	// Ignore seatbox button in ready-to-drive mode
	if currentState == types.StateReadyToDrive && channel == "seatbox_button" {
		v.logger.Printf("Ignoring seatbox button in ready-to-drive state")
		return nil
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
				v.logger.Printf("Failed to read kickstand: %v", err)
				return err
			}

			if !kickstandValue {
				// Check if both brakes are held
				brakeLeft, err := v.io.ReadDigitalInput("brake_left")
				if err != nil {
					v.logger.Printf("Failed to read brake_left: %v", err)
					return err
				}
				brakeRight, err := v.io.ReadDigitalInput("brake_right")
				if err != nil {
					v.logger.Printf("Failed to read brake_right: %v", err)
					return err
				}

				if brakeLeft && brakeRight {
					v.logger.Printf("Manual ready-to-drive activation: kickstand up, both brakes held, seatbox button pressed")

					// Blink the main light once for confirmation
					if err := v.io.PlayPwmCue(3); err != nil { // LED_PARKED_TO_DRIVE
						v.logger.Printf("Failed to play LED cue: %v", err)
					}

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
		v.logger.Printf("Kickstand changed: %v, current state: %s", value, currentState)
		if currentState == types.StateStandby || currentState == types.StateUpdating {
			v.logger.Printf("Ignoring kickstand change in %s state", currentState)
			return nil
		}
		if err := v.redis.SetKickstandState(value); err != nil {
			return err
		}
		if value {
			// Kickstand down - always go to PARKED
			v.logger.Printf("Kickstand down, transitioning to PARKED")
			return v.transitionTo(types.StateParked)
		} else if v.isReadyToDrive() {
			// Kickstand up and dashboard ready - go to READY_TO_DRIVE
			v.logger.Printf("Kickstand up and dashboard ready, transitioning to READY_TO_DRIVE")
			return v.transitionTo(types.StateReadyToDrive)
		}

	case "seatbox_button":
		if err := v.redis.SetSeatboxButton(value); err != nil {
			return err
		}
		if value {
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
			brakeLeft, err := v.io.ReadDigitalInput("brake_left")
			if err != nil {
				return fmt.Errorf("failed to read brake_left: %w", err)
			}
			brakeRight, err := v.io.ReadDigitalInput("brake_right")
			if err != nil {
				return fmt.Errorf("failed to read brake_right: %w", err)
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

		// Check for hibernation trigger condition
		brakeLeft, err := v.io.ReadDigitalInput("brake_left")
		if err != nil {
			v.logger.Printf("Failed to read brake_left for hibernation check: %v", err)
			// Continue without triggering hibernation
		}
		brakeRight, err := v.io.ReadDigitalInput("brake_right")
		if err != nil {
			v.logger.Printf("Failed to read brake_right for hibernation check: %v", err)
			// Continue without triggering hibernation
		}

		v.mu.RLock()
		currentState := v.state
		v.mu.RUnlock()

		if brakeLeft && brakeRight && currentState == types.StateParked {
			if v.hibernationTimer == nil {
				v.logger.Printf("Both brakes pressed in PARKED state, starting hibernation timer")
				v.hibernationTimer = time.AfterFunc(10*time.Second, v.triggerHibernation)
			}
		} else {
			if v.hibernationTimer != nil {
				v.logger.Printf("Hibernation conditions not met, stopping timer")
				v.hibernationTimer.Stop()
				v.hibernationTimer = nil
			}
		}

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
		return v.redis.SetBlinkerState(switchState)
	}

	// Handle blinker activation
	if channel == "blinker_right" {
		switchState = "right"
		cue = 11 // LED_BLINK_RIGHT
		v.blinkerState = BlinkerRight
	} else {
		switchState = "left"
		cue = 10 // LED_BLINK_LEFT
		v.blinkerState = BlinkerLeft
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
		v.logger.Printf("Error playing blinker cue: %v", err)
	}
	if err := v.redis.SetBlinkerState(state); err != nil {
		v.logger.Printf("Error updating blinker state: %v", err)
	}

	ticker := time.NewTicker(800 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-stopChan:
			return
		case <-ticker.C:
			if err := v.io.PlayPwmCue(cue); err != nil {
				v.logger.Printf("Error playing blinker cue: %v", err)
			}
			if err := v.redis.SetBlinkerState(state); err != nil {
				v.logger.Printf("Error updating blinker state: %v", err)
			}
		}
	}
}

func (v *VehicleSystem) keycardAuthPassed() error {
	v.logger.Printf("Processing keycard authentication tap")

	// --- Force Standby Check ---
	brakeLeft, errL := v.io.ReadDigitalInput("brake_left")
	if errL != nil {
		v.logger.Printf("Warning: Failed to read brake_left for keycard auth, assuming not pressed: %v", errL)
		brakeLeft = false
	}
	brakeRight, errR := v.io.ReadDigitalInput("brake_right")
	if errR != nil {
		v.logger.Printf("Warning: Failed to read brake_right for keycard auth, assuming not pressed: %v", errR)
		brakeRight = false
	}
	brakePressed := brakeLeft || brakeRight

	currentTime := time.Now()
	performForcedStandby := false

	v.mu.Lock()
	if v.lastKeycardTapTime.IsZero() || currentTime.Sub(v.lastKeycardTapTime) > keycardTapMaxInterval {
		v.keycardTapCount = 1
		v.logger.Printf("Keycard tap sequence: Start/Reset. Count: 1")
	} else {
		v.keycardTapCount++
		v.logger.Printf("Keycard tap sequence: Incremented. Count: %d", v.keycardTapCount)
	}
	v.lastKeycardTapTime = currentTime

	if v.keycardTapCount >= keycardForceStandbyTaps {
		if brakePressed {
			v.logger.Printf("Force standby condition met: %d taps, brake pressed.", v.keycardTapCount)
			v.forceStandbyNoLock = true
			performForcedStandby = true
		} else {
			v.logger.Printf("Force standby condition NOT met: %d taps, but brake not pressed. Resetting count.", v.keycardTapCount)
		}
		v.keycardTapCount = 0 // Reset after 3 taps, regardless of brake, for the next sequence
	}
	v.mu.Unlock()

	if performForcedStandby {
		// Check if we're in the middle of a DBC update
		v.mu.RLock()
		dbcUpdating := v.dbcUpdating
		v.mu.RUnlock()

		// If DBC is updating, transition to UPDATING state instead of STANDBY
		if dbcUpdating {
			v.logger.Printf("Transitioning to UPDATING (forced, no lock) due to DBC update.")
			// The forceStandbyNoLock flag will be read and reset by transitionTo
			return v.transitionTo(types.StateUpdating)
		} else {
			v.logger.Printf("Transitioning to STANDBY (forced, no lock).")
			// The forceStandbyNoLock flag will be read and reset by transitionTo
			return v.transitionTo(types.StateStandby)
		}
	}
	// --- End Force Standby Check ---

	// ----- Original keycardAuthPassed logic continues if not forced standby -----
	v.mu.RLock()
	currentState := v.state
	v.mu.RUnlock()

	v.logger.Printf("Current state during keycard auth (normal flow): %s", currentState)

	// Special handling for keycard auth during updates
	if currentState == types.StateUpdating {
		v.mu.RLock()
		dbcUpdating := v.dbcUpdating
		v.mu.RUnlock()

		if dbcUpdating {
			// If DBC is updating, allow keycard events but don't turn off dashboard power
			v.logger.Printf("Allowing keycard authentication during DBC update, but keeping dashboard power on")

			// For updating state, handle like standby - transition to parked or ready-to-drive based on kickstand
			v.logger.Printf("Reading kickstand state")
			kickstandValue, err := v.io.ReadDigitalInput("kickstand")
			if err != nil {
				v.logger.Printf("Failed to read kickstand: %v", err)
				return fmt.Errorf("failed to read kickstand: %w", err)
			}

			newState := types.StateReadyToDrive
			if kickstandValue {
				newState = types.StateParked
			}
			v.logger.Printf("Transitioning from UPDATING to %s (kickstand=%v)", newState, kickstandValue)
			return v.transitionTo(newState)
		}
	}

	if currentState == types.StateStandby {
		v.logger.Printf("Reading kickstand state")
		kickstandValue, err := v.io.ReadDigitalInput("kickstand")
		if err != nil {
			v.logger.Printf("Failed to read kickstand: %v", err)
			return fmt.Errorf("failed to read kickstand: %w", err)
		}

		newState := types.StateReadyToDrive
		if kickstandValue {
			newState = types.StateParked
		}
		v.logger.Printf("Transitioning from STANDBY to %s (kickstand=%v)", newState, kickstandValue)
		return v.transitionTo(newState)
	}

	// Check if we're in the middle of a DBC update
	v.mu.RLock()
	dbcUpdating := v.dbcUpdating
	v.mu.RUnlock()

	// Check kickstand before going to STANDBY or UPDATING
	kickstandValue, err := v.io.ReadDigitalInput("kickstand")
	if err != nil {
		v.logger.Printf("Failed to read kickstand: %v", err)
		return fmt.Errorf("failed to read kickstand: %w", err)
	}
	if !kickstandValue {
		v.logger.Printf("Cannot transition to STANDBY/UPDATING: kickstand not down")
		return fmt.Errorf("cannot transition to STANDBY/UPDATING: kickstand not down")
	}

	// If DBC is updating, transition back to UPDATING state instead of STANDBY
	if dbcUpdating {
		v.logger.Printf("DBC is updating, transitioning to UPDATING from %s", currentState)
		return v.transitionTo(types.StateUpdating)
	} else {
		v.logger.Printf("Transitioning to STANDBY from %s", currentState)
		return v.transitionTo(types.StateStandby)
	}
}

func (v *VehicleSystem) isReadyToDrive() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.dashboardReady
}

func (v *VehicleSystem) transitionTo(newState types.SystemState) error {
	v.mu.Lock()
	if newState == v.state {
		v.logger.Printf("State transition skipped - already in state %s", newState)
		v.mu.Unlock()
		return nil
	}

	oldState := v.state
	v.state = newState

	// Save previous state when entering updating state
	if newState == types.StateUpdating {
		v.previousState = oldState
	}

	v.mu.Unlock()

	// Set CPU governor when leaving standby
	if oldState == types.StateStandby && newState != types.StateStandby {
		v.logger.Printf("Leaving Standby: Setting CPU governor to ondemand")
		if err := exec.Command("sh", "-c", "echo ondemand > /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor").Run(); err != nil {
			v.logger.Printf("Warning: Failed to set CPU governor to ondemand: %v", err)
			// Not returning an error here as it's not critical for state transition
		}
	}

	v.logger.Printf("State transition: %s -> %s", oldState, newState)

	if err := v.publishState(); err != nil {
		v.logger.Printf("Failed to publish state: %v", err)
		return fmt.Errorf("failed to publish state: %w", err)
	}

	v.logger.Printf("Applying state transition effects for %s", newState)
	switch newState {
	case types.StateUpdating:
		// When entering updating state, ensure dashboard is powered on
		if err := v.io.WriteDigitalOutput("dashboard_power", true); err != nil {
			v.logger.Printf("Failed to enable dashboard power: %v", err)
			return err
		}
		v.logger.Printf("Dashboard power enabled for update")

		// Clear any pending power commands when entering update state
		v.mu.Lock()
		v.pendingPowerCmd = ""
		v.mu.Unlock()

	case types.StateReadyToDrive:
		// Check if handlebar needs to be unlocked
		handlebarPos, err := v.io.ReadDigitalInput("handlebar_position")
		if err != nil {
			v.logger.Printf("Failed to read handlebar position: %v", err)
			return err
		}
		if handlebarPos && !v.handlebarUnlocked {
			if err := v.unlockHandlebar(); err != nil {
				v.logger.Printf("Failed to unlock handlebar: %v", err)
				return err
			}
		}

		if err := v.io.WriteDigitalOutput("engine_power", true); err != nil {
			v.logger.Printf("Failed to enable engine power: %v", err)
			return err
		}
		v.logger.Printf("Engine power enabled")

		if err := v.io.WriteDigitalOutput("dashboard_power", true); err != nil {
			v.logger.Printf("Failed to enable dashboard power: %v", err)
			return err
		}
		v.logger.Printf("Dashboard power enabled")

		if oldState == types.StateParked {
			if err := v.io.PlayPwmCue(3); err != nil { // LED_PARKED_TO_DRIVE
				v.logger.Printf("Failed to play LED cue: %v", err)
			}
		}

	case types.StateParked:
		// Check if handlebar needs to be unlocked
		handlebarPos, err := v.io.ReadDigitalInput("handlebar_position")
		if err != nil {
			v.logger.Printf("Failed to read handlebar position: %v", err)
			return err
		}
		if handlebarPos && !v.handlebarUnlocked {
			if err := v.unlockHandlebar(); err != nil {
				v.logger.Printf("Failed to unlock handlebar: %v", err)
				return err
			}
		}

		if err := v.io.WriteDigitalOutput("engine_power", false); err != nil {
			v.logger.Printf("Failed to disable engine power: %v", err)
			return err
		}
		v.logger.Printf("Engine power disabled")

		if err := v.io.WriteDigitalOutput("dashboard_power", true); err != nil {
			v.logger.Printf("Failed to enable dashboard power: %v", err)
			return err
		}
		v.logger.Printf("Dashboard power enabled")

		if oldState == types.StateReadyToDrive {
			if err := v.io.PlayPwmCue(6); err != nil { // LED_DRIVE_TO_PARKED
				v.logger.Printf("Failed to play LED cue: %v", err)
			}
		}

		if oldState == types.StateStandby {
			brakeLeft, err := v.io.ReadDigitalInput("brake_left")
			if err != nil {
				v.logger.Printf("Failed to read brake_left: %v", err)
				return err
			}
			brakeRight, err := v.io.ReadDigitalInput("brake_right")
			if err != nil {
				v.logger.Printf("Failed to read brake_right: %v", err)
				return err
			}
			brakesPressed := brakeLeft || brakeRight

			if brakesPressed {
				if err := v.io.PlayPwmCue(2); err != nil { // LED_STANDBY_TO_PARKED_BRAKE_ON
					v.logger.Printf("Failed to play LED cue: %v", err)
				}
			} else {
				if err := v.io.PlayPwmCue(1); err != nil { // LED_STANDBY_TO_PARKED_BRAKE_OFF
					v.logger.Printf("Failed to play LED cue: %v", err)
				}
			}
		}

	case types.StateStandby:
		v.mu.Lock()
		forcedStandby := v.forceStandbyNoLock
		dbcUpdating := v.dbcUpdating
		if forcedStandby {
			v.forceStandbyNoLock = false // Reset the flag
		}
		v.mu.Unlock()

		// Set CPU governor to powersave
		v.logger.Printf("Entering Standby: Setting CPU governor to powersave")
		if err := exec.Command("sh", "-c", "echo powersave > /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor").Run(); err != nil {
			v.logger.Printf("Warning: Failed to set CPU governor to powersave: %v", err)
			// Not returning an error here as it's not critical for state transition
		}

		isFromParked := (oldState == types.StateParked)

		if forcedStandby {
			v.logger.Printf("Forced standby: skipping handlebar lock.")
		} else if isFromParked {
			// Normal standby transition from Parked: lock handlebar
			v.lockHandlebar()
		}
		// If not forced and not from Parked (e.g. Init -> Standby), no specific handlebar lock action here.

		// LED Cues specifically for Parked -> Standby transition.
		// These are skipped if it's a forced standby that might originate from a different state.
		if isFromParked {
			brakeLeft, err := v.io.ReadDigitalInput("brake_left")
			if err != nil {
				v.logger.Printf("Failed to read brake_left for standby cue: %v", err)
				// Continue without returning error, best effort for cues
			}
			brakeRight, err := v.io.ReadDigitalInput("brake_right")
			if err != nil {
				v.logger.Printf("Failed to read brake_right for standby cue: %v", err)
				// Continue
			}
			brakesPressed := brakeLeft || brakeRight

			if brakesPressed {
				if err := v.io.PlayPwmCue(8); err != nil { // LED_PARKED_BRAKE_ON_TO_STANDBY
					v.logger.Printf("Failed to play LED cue (8): %v", err)
				}
			} else {
				if err := v.io.PlayPwmCue(7); err != nil { // LED_PARKED_BRAKE_OFF_TO_STANDBY
					v.logger.Printf("Failed to play LED cue (7): %v", err)
				}
			}
		}

		// Common actions for ALL transitions to Standby (forced or not)
		// Keep dashboard power on if DBC is updating, otherwise turn it off
		if !dbcUpdating {
			if err := v.io.WriteDigitalOutput("dashboard_power", false); err != nil {
				v.logger.Printf("Failed to disable dashboard power: %v", err)
				// Consider if this error should halt the transition or just be logged.
				// For now, logging and continuing to ensure other shutdown steps occur.
			}
			v.logger.Printf("Dashboard power disabled")
		} else {
			v.logger.Printf("Keeping dashboard power on for DBC update")
		}

		// Final "all off" cue for standby.
		if err := v.io.PlayPwmCue(0); err != nil { // ALL_OFF
			v.logger.Printf("Failed to play LED cue ALL_OFF (0): %v", err)
		}

	}

	v.logger.Printf("State transition completed successfully")
	return nil
}

func (v *VehicleSystem) openSeatboxLock() error {
	if err := v.io.WriteDigitalOutput("seatbox_lock", true); err != nil {
		return err
	}

	time.Sleep(200 * time.Millisecond)

	if err := v.io.WriteDigitalOutput("seatbox_lock", false); err != nil {
		return err
	}

	v.logger.Printf("Seatbox lock toggled for 0.2s")
	return nil
}

func (v *VehicleSystem) publishState() error {
	v.mu.RLock()
	state := v.state
	v.mu.RUnlock()

	return v.redis.PublishVehicleState(state)
}

func (v *VehicleSystem) Shutdown() {
	// Stop blinker if running
	if v.blinkerStopChan != nil {
		close(v.blinkerStopChan)
		v.blinkerStopChan = nil
	}

	v.transitionTo(types.StateShuttingDown)
	if v.redis != nil {
		v.redis.Close()
	}
	if v.io != nil {
		v.io.Cleanup()
	}
}

func (v *VehicleSystem) handleSeatboxRequest(on bool) error {
	v.logger.Printf("Handling seatbox request: %v", on)
	if on {
		return v.openSeatboxLock()
	}
	return nil
}

func (v *VehicleSystem) handleHornRequest(on bool) error {
	v.logger.Printf("Handling horn request: %v", on)
	return v.io.WriteDigitalOutput("horn", on)
}

func (v *VehicleSystem) handleBlinkerRequest(state string) error {
	v.logger.Printf("Handling blinker request: %s", state)

	// Stop any existing blinker routine
	if v.blinkerStopChan != nil {
		close(v.blinkerStopChan)
		v.blinkerStopChan = nil
	}

	// Update Redis switch state first
	var cue int
	switch state {
	case "off":
		cue = 9 // LED_BLINK_NONE
		v.blinkerState = BlinkerOff
	case "left":
		cue = 10 // LED_BLINK_LEFT
		v.blinkerState = BlinkerLeft
	case "right":
		cue = 11 // LED_BLINK_RIGHT
		v.blinkerState = BlinkerRight
	case "both":
		cue = 12 // LED_BLINK_BOTH
		v.blinkerState = BlinkerBoth
	default:
		return fmt.Errorf("invalid blinker state: %s", state)
	}

	if err := v.io.PlayPwmCue(cue); err != nil {
		return err
	}

	if err := v.redis.SetBlinkerSwitch(state); err != nil {
		return err
	}

	if state != "off" {
		// Start blinker routine for non-off states
		v.blinkerStopChan = make(chan struct{})
		go v.runBlinker(cue, state, v.blinkerStopChan)
	}

	return v.redis.SetBlinkerState(state)
}

func (v *VehicleSystem) handlePowerRequest(action string) error {
	v.logger.Printf("Handling power request: %s", action)

	// Check if we're in updating state
	v.mu.RLock()
	currentState := v.state
	v.mu.RUnlock()

	if currentState == types.StateUpdating {
		v.logger.Printf("System is in updating state, queueing power request: %s", action)
		v.mu.Lock()
		v.pendingPowerCmd = action
		v.mu.Unlock()
		return nil
	}

	switch action {
	case "hibernate-manual":
		v.Shutdown()
		// Execute shutdown until we have a proper nRF communication
		return exec.Command("systemctl", "poweroff").Run()
	case "reboot":
		v.Shutdown()
		// Execute system reboot command
		return exec.Command("systemctl", "reboot").Run()
	default:
		return fmt.Errorf("invalid power action: %s", action)
	}
}

func (v *VehicleSystem) lockHandlebar() {
	// Run the lock operation in a goroutine
	go func() {
		handlebarPos, err := v.io.ReadDigitalInput("handlebar_position")
		if err != nil {
			v.logger.Printf("Failed to read handlebar position: %v", err)
			return
		}

		if handlebarPos {
			// Handlebar is in position, lock it immediately
			if err := v.io.WriteDigitalOutput("handlebar_lock_close", true); err != nil {
				v.logger.Printf("Failed to activate handlebar lock close: %v", err)
				return
			}
			time.Sleep(1100 * time.Millisecond)
			if err := v.io.WriteDigitalOutput("handlebar_lock_close", false); err != nil {
				v.logger.Printf("Failed to deactivate handlebar lock close: %v", err)
				return
			}
			v.logger.Printf("Handlebar locked")

			// Reset unlock state after successful lock
			v.mu.Lock()
			v.handlebarUnlocked = false
			v.mu.Unlock()
			v.logger.Printf("Reset handlebar unlock state after successful lock")
			return
		}

		// Start 10 second timer for handlebar position
		if v.handlebarTimer != nil {
			v.handlebarTimer.Stop()
		}
		v.handlebarTimer = time.NewTimer(10 * time.Second)

		// Create a cleanup function to restore the original callback
		cleanup := func() {
			v.handlebarTimer = nil
			// Re-register the original handlebar position callback
			v.io.RegisterInputCallback("handlebar_position", v.handleHandlebarPosition)
			v.logger.Printf("Restored original handlebar position callback")
		}

		// Register temporary callback for handlebar position during lock window
		v.io.RegisterInputCallback("handlebar_position", func(channel string, value bool) error {
			if !value {
				return nil // Only care about activation
			}

			// Check if we're still in the window
			if v.handlebarTimer == nil {
				v.logger.Printf("Lock window has expired")
				return nil
			}

			// Stop the timer
			v.handlebarTimer.Stop()

			// Lock the handlebar
			if err := v.io.WriteDigitalOutput("handlebar_lock_close", true); err != nil {
				cleanup()
				v.logger.Printf("Failed to activate handlebar lock close: %v", err)
				return err
			}
			time.Sleep(1100 * time.Millisecond)
			if err := v.io.WriteDigitalOutput("handlebar_lock_close", false); err != nil {
				cleanup()
				v.logger.Printf("Failed to deactivate handlebar lock close: %v", err)
				return err
			}
			v.logger.Printf("Handlebar locked")

			// Reset unlock state after successful lock
			v.mu.Lock()
			v.handlebarUnlocked = false
			v.mu.Unlock()
			v.logger.Printf("Reset handlebar unlock state after successful lock")

			// Restore original callback
			cleanup()
			return nil
		})

		v.logger.Printf("Started 10 second window for handlebar lock")

		// Wait for timer expiration
		<-v.handlebarTimer.C
		cleanup()
		v.logger.Printf("Handlebar lock window expired")
	}()
}

func (v *VehicleSystem) unlockHandlebar() error {
	if err := v.io.WriteDigitalOutput("handlebar_lock_open", true); err != nil {
		return fmt.Errorf("failed to activate handlebar lock open: %w", err)
	}
	time.Sleep(1100 * time.Millisecond)
	if err := v.io.WriteDigitalOutput("handlebar_lock_open", false); err != nil {
		return fmt.Errorf("failed to deactivate handlebar lock open: %w", err)
	}
	v.handlebarUnlocked = true
	v.logger.Printf("Handlebar unlocked")
	return nil
}

func (v *VehicleSystem) handleHandlebarPosition(channel string, value bool) error {
	if !value {
		return nil // Only care about activation
	}

	v.mu.RLock()
	state := v.state
	unlocked := v.handlebarUnlocked
	v.mu.RUnlock()

	// Only unlock if we haven't unlocked yet in this power cycle
	if !unlocked && (state == types.StateParked || state == types.StateReadyToDrive) {
		return v.unlockHandlebar()
	}

	return nil
}

func (v *VehicleSystem) handleStateRequest(state string) error {
	v.logger.Printf("Handling state request: %s", state)
	v.mu.RLock()
	currentState := v.state
	v.mu.RUnlock()

	// Don't allow state changes during updates
	if currentState == types.StateUpdating {
		v.logger.Printf("Ignoring state request during update: %s", state)
		return nil
	}

	switch state {
	case "unlock":
		kickstandValue, err := v.io.ReadDigitalInput("kickstand")
		if err != nil {
			v.logger.Printf("Failed to read kickstand: %v", err)
			if currentState == types.StateStandby {
				return v.transitionTo(types.StateParked)
			} else {
				return nil
			}

		}
		if v.isReadyToDrive() && !kickstandValue {
			return v.transitionTo(types.StateReadyToDrive)
		} else {
			if currentState == types.StateStandby {
				return v.transitionTo(types.StateParked)
			} else {
				return nil
			}
		}
	case "lock":
		if currentState == types.StateParked {
			// Check if we're in the middle of a DBC update
			v.mu.RLock()
			dbcUpdating := v.dbcUpdating
			v.mu.RUnlock()

			// If DBC is updating, transition to UPDATING state instead of STANDBY
			if dbcUpdating {
				v.logger.Printf("DBC is updating, transitioning to UPDATING instead of STANDBY")
				return v.transitionTo(types.StateUpdating)
			} else {
				return v.transitionTo(types.StateStandby)
			}
		} else {
			return fmt.Errorf("vehicle must be parked to lock")
		}
	case "lock-hibernate":
		if currentState == types.StateParked {
			// Check if we're in the middle of a DBC update
			v.mu.RLock()
			dbcUpdating := v.dbcUpdating
			v.mu.RUnlock()

			// If DBC is updating, transition to UPDATING state instead of STANDBY
			if dbcUpdating {
				v.logger.Printf("DBC is updating, transitioning to UPDATING instead of STANDBY for lock-hibernate")
				if err := v.transitionTo(types.StateUpdating); err != nil {
					return err
				}
			} else {
				if err := v.transitionTo(types.StateStandby); err != nil {
					return err
				}
			}

			// Only schedule hibernation if DBC is not updating
			if !dbcUpdating {
				go func() {
					time.Sleep(30 * time.Second)
					if err := v.handlePowerRequest("hibernate-manual"); err != nil {
						v.logger.Printf("Failed to execute hibernate: %v", err)
					}
				}()
			}
			return nil
		} else {
			return fmt.Errorf("vehicle must be parked to lock")
		}
	default:
		return fmt.Errorf("invalid state request: %s", state)
	}
}

func (v *VehicleSystem) handleLedCueRequest(cueIndex int) error {
	v.logger.Printf("Handling LED cue request: %d", cueIndex)
	return v.io.PlayPwmCue(cueIndex)
}

func (v *VehicleSystem) handleLedFadeRequest(ledChannel int, fadeIndex int) error {
	v.logger.Printf("Handling LED fade request: channel=%d, index=%d", ledChannel, fadeIndex)
	return v.io.PlayPwmFade(ledChannel, fadeIndex)
}

// handleForceLockRequest is called when a "force-lock" command is received via Redis.
// It initiates a forced transition to standby or updating state, skipping the handlebar lock.
func (v *VehicleSystem) handleForceLockRequest() error {
	// Check if we're in the middle of a DBC update
	v.mu.RLock()
	dbcUpdating := v.dbcUpdating
	v.mu.RUnlock()

	v.mu.Lock()
	v.forceStandbyNoLock = true
	v.mu.Unlock()

	// If DBC is updating, transition to UPDATING state instead of STANDBY
	if dbcUpdating {
		v.logger.Printf("Handling force-lock request: DBC is updating, transitioning to UPDATING (no lock).")
		return v.transitionTo(types.StateUpdating)
	} else {
		v.logger.Printf("Handling force-lock request: transitioning to STANDBY (no lock).")
		return v.transitionTo(types.StateStandby)
	}
}

// handleUpdateRequest handles update requests from the update-service
func (v *VehicleSystem) handleUpdateRequest(action string) error {
	v.logger.Printf("Handling update request: %s", action)

	switch action {
	case "start":
		// Transition to updating state
		v.logger.Printf("Starting update process")
		return v.transitionTo(types.StateUpdating)

	case "start-dbc":
		// Mark DBC as updating and ensure dashboard is powered on
		v.mu.Lock()
		v.dbcUpdating = true
		v.mu.Unlock()

		v.logger.Printf("Starting DBC update process")

		// Ensure dashboard is powered on
		if err := v.io.WriteDigitalOutput("dashboard_power", true); err != nil {
			v.logger.Printf("Failed to enable dashboard power for DBC update: %v", err)
			return err
		}

		// If we're not already in updating state, transition to it
		v.mu.RLock()
		currentState := v.state
		v.mu.RUnlock()

		if currentState != types.StateUpdating {
			return v.transitionTo(types.StateUpdating)
		}

		return nil

	case "complete-dbc":
		// Mark DBC update as complete
		v.mu.Lock()
		v.dbcUpdating = false
		v.mu.Unlock()

		v.logger.Printf("DBC update process complete")

		// If we're in standby state, we can now turn off the dashboard
		v.mu.RLock()
		currentState := v.state
		v.mu.RUnlock()

		if currentState == types.StateStandby {
			if err := v.io.WriteDigitalOutput("dashboard_power", false); err != nil {
				v.logger.Printf("Failed to disable dashboard power after DBC update: %v", err)
				return err
			}
			v.logger.Printf("Dashboard power disabled after DBC update")
		}

		return nil

	case "complete":
		// Update is complete, check if there's a pending power command
		v.mu.RLock()
		pendingCmd := v.pendingPowerCmd
		dbcUpdating := v.dbcUpdating
		previousState := v.previousState
		v.mu.RUnlock()

		v.logger.Printf("Update process complete, pending power command: %s, DBC updating: %v", pendingCmd, dbcUpdating)

		// Clear the pending command
		v.mu.Lock()
		v.pendingPowerCmd = ""
		v.mu.Unlock()

		// If DBC is still updating, don't transition back to previous state yet
		if dbcUpdating {
			v.logger.Printf("DBC is still updating, staying in updating state")
			return nil
		}

		// Transition back to previous state
		if err := v.transitionTo(previousState); err != nil {
			return err
		}

		// Execute any pending power command
		if pendingCmd != "" {
			v.logger.Printf("Executing pending power command: %s", pendingCmd)
			return v.handlePowerRequest(pendingCmd)
		}

		return nil

	case "cycle-dashboard-power":
		// Cycle dashboard power to reboot the DBC
		v.logger.Printf("Cycling dashboard power to reboot DBC")

		// Turn off dashboard power
		if err := v.io.WriteDigitalOutput("dashboard_power", false); err != nil {
			v.logger.Printf("Failed to disable dashboard power: %v", err)
			return err
		}
		time.Sleep(1 * time.Second)
		// Turn dashboard power back on
		if err := v.io.WriteDigitalOutput("dashboard_power", true); err != nil {
			v.logger.Printf("Failed to re-enable dashboard power: %v", err)
			return err
		}
		v.logger.Printf("Dashboard power cycled successfully")
		return nil

	default:
		return fmt.Errorf("invalid update action: %s", action)
	}
}

// EnableDashboardForUpdate turns on the dashboard without entering ready-to-drive state
func (v *VehicleSystem) EnableDashboardForUpdate() error {
	v.logger.Printf("Enabling dashboard for update")

	// Turn on dashboard power
	if err := v.io.WriteDigitalOutput("dashboard_power", true); err != nil {
		v.logger.Printf("Failed to enable dashboard power: %v", err)
		return err
	}

	v.logger.Printf("Dashboard power enabled for update")
	return nil
}
