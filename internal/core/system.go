// File: internal/core/system.go
package core

import (
	"fmt"
	"log"
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

type VehicleSystem struct {
	state             types.SystemState
	dashboardReady    bool
	logger            *log.Logger
	io                *hardware.LinuxHardwareIO
	redis             *messaging.RedisClient
	mu                sync.RWMutex
	redisHost         string
	redisPort         int
	blinkerState      BlinkerState
	blinkerStopChan   chan struct{}
	initialized       bool
	handlebarUnlocked bool        // Track if handlebar has been unlocked in this power cycle
	handlebarTimer    *time.Timer // Timer for handlebar position window
	hibernationTimer  *time.Timer // Timer for brake-triggered hibernation
}

func NewVehicleSystem(redisHost string, redisPort int) *VehicleSystem {
	return &VehicleSystem{
		state:        types.StateInit,
		logger:       log.New(log.Writer(), "Vehicle: ", log.LstdFlags),
		io:           hardware.NewLinuxHardwareIO(),
		redisHost:    redisHost,
		redisPort:    redisPort,
		blinkerState: BlinkerOff,
		initialized:  false,
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
		LedCueCallback:    v.handleLedCueRequest,
		LedFadeCallback:   v.handleLedFadeRequest,
	})

	if err := v.redis.Connect(); err != nil {
		return fmt.Errorf("failed to connect to Redis: %w", err)
	}

	// Load initial state from Redis
	savedState, err := v.redis.GetVehicleState()
	if err != nil {
		v.logger.Printf("Failed to get saved state from Redis: %v", err)
		// Continue with default state
	} else if savedState != types.StateInit {
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

	// Reload PWM LED kernel module
	if err := exec.Command("rmmod", "imx_pwm_led").Run(); err != nil {
		v.logger.Printf("Warning: Failed to remove PWM LED module: %v", err)
	}
	if err := exec.Command("modprobe", "imx_pwm_led").Run(); err != nil {
		return fmt.Errorf("failed to load PWM LED module: %w", err)
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
		// Don't process kickstand state in STANDBY
		if currentState == types.StateStandby {
			v.logger.Printf("Skipping kickstand check in STANDBY state")
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

	// Handle inputs that should only work when not in standby
	if currentState == types.StateStandby {
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

	switch channel {
	case "horn_button":
		if err := v.io.WriteDigitalOutput("horn", value); err != nil {
			return err
		}
		return v.redis.SetHornButton(value)

	case "kickstand":
		v.logger.Printf("Kickstand changed: %v, current state: %s", value, currentState)
		if currentState == types.StateStandby {
			v.logger.Printf("Ignoring kickstand change in STANDBY state")
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
	v.logger.Printf("Processing keycard authentication")
	v.mu.RLock()
	currentState := v.state
	v.mu.RUnlock()

	v.logger.Printf("Current state during keycard auth: %s", currentState)

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

	// Check kickstand before going to STANDBY
	kickstandValue, err := v.io.ReadDigitalInput("kickstand")
	if err != nil {
		v.logger.Printf("Failed to read kickstand: %v", err)
		return fmt.Errorf("failed to read kickstand: %w", err)
	}
	if !kickstandValue {
		v.logger.Printf("Cannot transition to STANDBY: kickstand not down")
		return fmt.Errorf("cannot transition to STANDBY: kickstand not down")
	}

	v.logger.Printf("Transitioning to STANDBY from %s", currentState)
	return v.transitionTo(types.StateStandby)
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
	v.mu.Unlock()

	v.logger.Printf("State transition: %s -> %s", oldState, newState)

	if err := v.publishState(); err != nil {
		v.logger.Printf("Failed to publish state: %v", err)
		return fmt.Errorf("failed to publish state: %w", err)
	}

	v.logger.Printf("Applying state transition effects for %s", newState)
	switch newState {
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
		if oldState == types.StateParked {
			// Start handlebar lock attempt in background
			v.lockHandlebar()

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
				if err := v.io.PlayPwmCue(8); err != nil { // LED_PARKED_BRAKE_ON_TO_STANDBY
					v.logger.Printf("Failed to play LED cue: %v", err)
				}
			} else {
				if err := v.io.PlayPwmCue(7); err != nil { // LED_PARKED_BRAKE_OFF_TO_STANDBY
					v.logger.Printf("Failed to play LED cue: %v", err)
				}
			}
		}

		// Must set dashboard power off after playing the LED cue
		if err := v.io.WriteDigitalOutput("dashboard_power", false); err != nil {
			v.logger.Printf("Failed to disable dashboard power: %v", err)
			return err
		}

		v.logger.Printf("Dashboard power disabled")
		if err := v.io.PlayPwmCue(0); err != nil { // ALL_OFF
			v.logger.Printf("Failed to play LED cue: %v", err)
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
			return v.transitionTo(types.StateStandby)
		} else {
			return fmt.Errorf("vehicle must be parked to lock")
		}
	case "lock-hibernate":
		if currentState == types.StateParked {
			if err := v.transitionTo(types.StateStandby); err != nil {
				return err
			}

			go func() {
				time.Sleep(30 * time.Second)
				if err := v.handlePowerRequest("hibernate-manual"); err != nil {
					v.logger.Printf("Failed to execute hibernate: %v", err)
				}
			}()
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
