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
	shutdownTimerDuration = 4000 * time.Millisecond
	handlebarLockWindow   = 10 * time.Second
	seatboxLockDuration   = 200 * time.Millisecond
	parkDebounceTime      = 1 * time.Second

	// Hibernation timing constants
	hibernationInitialHoldDuration = 15 * time.Second // Initial brake hold to enter waiting-hibernation
	hibernationConfirmationTimeout = 30 * time.Second // Timeout when in waiting-hibernation with no brake activity
	hibernationForceHoldDuration   = 30 * time.Second // Total continuous hold for auto-confirm hibernation
)

// Manual hibernation state machine
type ManualHibernationState int

const (
	HibernationIdle         ManualHibernationState = iota
	HibernationInitialHold                         // 0-15s: both brakes held
	HibernationConfirmation                        // 15s+: awaiting confirmation
)

func (s ManualHibernationState) String() string {
	switch s {
	case HibernationIdle:
		return "idle"
	case HibernationInitialHold:
		return "initial-hold"
	case HibernationConfirmation:
		return "confirmation"
	default:
		return "unknown"
	}
}

// HibernationManager manages the manual hibernation state machine
type HibernationManager struct {
	mu                   sync.RWMutex
	state                ManualHibernationState
	startTime            time.Time   // When initial brake hold started
	reholdStartTime      time.Time   // When brakes were pressed again after release
	initialTimer         *time.Timer // Initial hold timer
	confirmationTimer    *time.Timer // Confirmation timeout timer
	reholdTimer          *time.Timer // Timer for re-hold force confirm (20s after re-press)
	bothBrakesHeld       bool        // Current brake state
	brakesReleased       bool        // Whether brakes were released during confirmation
	logger               *logger.Logger
	redis                *messaging.RedisClient
	onHibernationConfirm func()                        // Callback to execute hibernation
	transitionTo         func(types.SystemState) error // Callback to transition vehicle state
	isKickstandDown      func() (bool, error)          // Callback to check if kickstand is down (true = down)
	isSeatboxClosed      func() (bool, error)          // Callback to check if seatbox is closed (true = closed)
}

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
	readyToDriveEntryTime  time.Time   // Track when we entered ready-to-drive state for park debounce
	keycardTapCount        int
	lastKeycardTapTime     time.Time
	forceStandbyNoLock     bool
	hibernationRequest      bool                // Track if hibernation was requested during shutdown
	shutdownFromParked      bool                // Track if shutdown was initiated from parked state
	dbcUpdating             bool                // Track if DBC update is in progress
	deferredDashboardPower  *bool               // Deferred dashboard power state (nil = no change needed)
	brakeHibernationEnabled bool                // Track if brake lever hibernation is enabled (default: true)
	hibernationManager      *HibernationManager // Manual hibernation state machine
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

	// Initialize hibernation manager
	v.hibernationManager = &HibernationManager{
		state:  HibernationIdle,
		logger: v.logger.WithTag("Hibernation"),
		redis:  v.redis,
		onHibernationConfirm: func() {
			v.logger.Infof("Hibernation confirmed, initiating proper shutdown sequence")
			v.mu.Lock()
			v.hibernationRequest = true
			v.mu.Unlock()
			if err := v.transitionTo(types.StateShuttingDown); err != nil {
				v.logger.Infof("Failed to transition to shutdown for hibernation: %v", err)
			}
		},
		transitionTo: v.transitionTo,
		isKickstandDown: func() (bool, error) {
			// kickstand input: true = down, false = up
			return v.io.ReadDigitalInput("kickstand")
		},
		isSeatboxClosed: func() (bool, error) {
			// seatbox_lock_sensor: true = closed, false = open
			return v.io.ReadDigitalInput("seatbox_lock_sensor")
		},
	}

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
		v.logger.Infof("Failed to get saved state from Redis: %v", err)
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

	// Initialize hardware
	if err := v.io.Initialize(); err != nil {
		return fmt.Errorf("failed to initialize hardware: %w", err)
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
				return v.redis.SetSeatboxLockState(value)
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

	// Now that hardware is initialized, restore state and outputs
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
			v.logger.Infof("Failed to handle initial dashboard ready state: %v", err)
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
			v.logger.Infof("Warning: failed to transition to Standby from Init: %v", err)
		}
	}

	// Start Redis listeners now that everything is initialized
	if err := v.redis.StartListening(); err != nil {
		return fmt.Errorf("failed to start Redis listeners: %w", err)
	}

	v.logger.Infof("System started successfully")
	return nil
}

// getCurrentState returns the current system state in a thread-safe manner
func (v *VehicleSystem) getCurrentState() types.SystemState {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.state
}

// checkHibernationConditions checks if hibernation should be triggered using state machine
func (v *VehicleSystem) checkHibernationConditions() {
	currentState := v.getCurrentState()

	// Only check hibernation conditions in parked or hibernation states
	if currentState != types.StateParked &&
	   currentState != types.StateWaitingHibernation &&
	   currentState != types.StateWaitingHibernationSeatbox &&
	   currentState != types.StateWaitingHibernationConfirm {
		v.hibernationManager.cancelHibernation()
		return
	}

	// Check if brake hibernation is enabled
	v.mu.RLock()
	hibernationEnabled := v.brakeHibernationEnabled
	v.mu.RUnlock()

	if !hibernationEnabled {
		v.logger.Infof("Brake hibernation is disabled, skipping hibernation check")
		v.hibernationManager.cancelHibernation()
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

	// Update hibernation manager with current brake state
	v.hibernationManager.updateBrakeState(brakeLeft && brakeRight)
}


// Hibernation Manager Methods

// updateBrakeState updates the hibernation state machine based on brake input
func (hm *HibernationManager) updateBrakeState(bothBrakesPressed bool) {
	hm.mu.Lock()
	defer hm.mu.Unlock()

	previousBrakeState := hm.bothBrakesHeld
	hm.bothBrakesHeld = bothBrakesPressed

	switch hm.state {
	case HibernationIdle:
		if bothBrakesPressed {
			// Start hibernation sequence
			hm.startInitialHold()
		}

	case HibernationInitialHold:
		if !bothBrakesPressed {
			// Brakes released during initial hold - cancel
			hm.logger.Infof("Brakes released during initial hold - cancelling hibernation")
			hm.resetToIdle()
		}
		// If brakes still held, timer will handle transition to confirmation

	case HibernationConfirmation:
		if previousBrakeState && !bothBrakesPressed {
			// Brakes just released - start confirmation timeout
			hm.logger.Infof("Brakes released - starting confirmation timeout")
			hm.brakesReleased = true
			hm.startConfirmationTimeout()
		} else if !previousBrakeState && bothBrakesPressed {
			// Brakes pressed again after release
			if hm.brakesReleased {
				hm.logger.Infof("Both brakes pressed again - resetting timeout and starting rehold timer")
				hm.startReholdTimer()
			}
		} else if previousBrakeState && bothBrakesPressed {
			// Brakes still held or touched - reset timeout timer
			hm.resetConfirmationTimeout()
		}
		// Force hibernation after continuous hold is handled by timer in transitionToConfirmation()
	}
}

// startInitialHold begins the initial brake hold phase
func (hm *HibernationManager) startInitialHold() {
	hm.state = HibernationInitialHold
	hm.startTime = time.Now()
	hm.brakesReleased = false

	hm.logger.Infof("Starting hibernation initial hold (%.0fs)", hibernationInitialHoldDuration.Seconds())

	// Start timer for initial hold
	hm.initialTimer = time.AfterFunc(hibernationInitialHoldDuration, func() {
		hm.mu.Lock()
		defer hm.mu.Unlock()

		if hm.state == HibernationInitialHold && hm.bothBrakesHeld {
			hm.logger.Infof("Initial hold completed - transitioning to waiting-hibernation")

			// Transition vehicle to waiting-hibernation state after 20s hold
			if hm.transitionTo != nil {
				if err := hm.transitionTo(types.StateWaitingHibernation); err != nil {
					hm.logger.Infof("Failed to transition to waiting-hibernation: %v", err)
					return
				}
			}

			// Enter confirmation phase
			hm.transitionToConfirmation()
		}
	})
}

// transitionToConfirmation moves to the confirmation phase
func (hm *HibernationManager) transitionToConfirmation() {
	hm.state = HibernationConfirmation

	// Stop initial timer if running
	if hm.initialTimer != nil {
		hm.initialTimer.Stop()
		hm.initialTimer = nil
	}

	hm.logger.Infof("Hibernation confirmation phase started")

	// Start a timer for force hibernation (remaining time to reach total duration)
	remainingTime := hibernationForceHoldDuration - time.Since(hm.startTime)
	if remainingTime > 0 {
		hm.initialTimer = time.AfterFunc(remainingTime, func() {
			hm.mu.Lock()
			defer hm.mu.Unlock()

			// If brakes are still held continuously (never released), trigger force hibernation
			if hm.state == HibernationConfirmation && hm.bothBrakesHeld && !hm.brakesReleased {
				hm.logger.Infof("Force hibernation triggered - %.0fs continuous hold", hibernationForceHoldDuration.Seconds())
				hm.confirmHibernation(true) // Auto-confirm - skip seatbox check
			}
		})
	}

	// Check if seatbox is open - if so, transition to seatbox notification state
	if hm.isSeatboxClosed != nil {
		seatboxClosed, err := hm.isSeatboxClosed()
		if err != nil {
			hm.logger.Infof("Failed to read seatbox state: %v", err)
		} else if !seatboxClosed {
			hm.logger.Infof("Seatbox is open - showing seatbox notification")
			if hm.transitionTo != nil {
				if err := hm.transitionTo(types.StateWaitingHibernationSeatbox); err != nil {
					hm.logger.Infof("Failed to transition to waiting-hibernation-seatbox: %v", err)
				}
			}
			return
		}
	}

	// Seatbox is closed, stay in standard confirmation state
	// (no separate state needed, waiting-hibernation covers this)
}

// startConfirmationTimeout starts the 30s timeout for confirmation after brake release
func (hm *HibernationManager) startConfirmationTimeout() {
	// Stop any existing confirmation timer
	if hm.confirmationTimer != nil {
		hm.confirmationTimer.Stop()
	}

	// Stop the force hibernation timer since brakes were released
	if hm.initialTimer != nil {
		hm.initialTimer.Stop()
		hm.initialTimer = nil
	}

	// Stop any rehold timer
	if hm.reholdTimer != nil {
		hm.reholdTimer.Stop()
		hm.reholdTimer = nil
	}

	hm.confirmationTimer = time.AfterFunc(hibernationConfirmationTimeout, func() {
		hm.mu.Lock()
		defer hm.mu.Unlock()

		// Only timeout if brakes are NOT held
		if hm.state == HibernationConfirmation && !hm.bothBrakesHeld {
			hm.logger.Infof("Confirmation timeout - cancelling hibernation")
			hm.resetToIdle()
		}
	})

	hm.logger.Infof("Confirmation timeout started (%.0fs)", hibernationConfirmationTimeout.Seconds())
}

// resetConfirmationTimeout resets the confirmation timeout (called when brakes touched)
func (hm *HibernationManager) resetConfirmationTimeout() {
	if hm.confirmationTimer != nil {
		hm.confirmationTimer.Stop()
		hm.confirmationTimer = time.AfterFunc(hibernationConfirmationTimeout, func() {
			hm.mu.Lock()
			defer hm.mu.Unlock()

			// Only timeout if brakes are NOT held
			if hm.state == HibernationConfirmation && !hm.bothBrakesHeld {
				hm.logger.Infof("Confirmation timeout - cancelling hibernation")
				hm.resetToIdle()
			}
		})
		hm.logger.Infof("Confirmation timeout reset (%.0fs)", hibernationConfirmationTimeout.Seconds())
	}
}

// startReholdTimer starts a timer for force confirmation after brakes are pressed again
func (hm *HibernationManager) startReholdTimer() {
	// Stop any existing rehold timer
	if hm.reholdTimer != nil {
		hm.reholdTimer.Stop()
	}

	// Stop confirmation timeout since user is actively holding brakes
	if hm.confirmationTimer != nil {
		hm.confirmationTimer.Stop()
		hm.confirmationTimer = nil
	}

	hm.reholdStartTime = time.Now()
	hm.reholdTimer = time.AfterFunc(hibernationInitialHoldDuration, func() {
		hm.mu.Lock()
		defer hm.mu.Unlock()

		// If brakes still held, trigger force confirmation
		if hm.state == HibernationConfirmation && hm.bothBrakesHeld {
			hm.logger.Infof("Force hibernation triggered - %.0fs rehold", hibernationInitialHoldDuration.Seconds())
			hm.confirmHibernation(true) // Auto-confirm - skip seatbox check
		}
	})

	hm.logger.Infof("Rehold timer started (%.0fs)", hibernationInitialHoldDuration.Seconds())
}

// confirmHibernation executes the hibernation after checking safety conditions
// autoConfirm: if true, skips seatbox check (for continuous hold or rehold)
func (hm *HibernationManager) confirmHibernation(autoConfirm bool) {
	if autoConfirm {
		hm.logger.Infof("Hibernation auto-confirmed (%.0fs continuous or %.0fs rehold), checking safety conditions",
			hibernationForceHoldDuration.Seconds(), hibernationInitialHoldDuration.Seconds())
	} else {
		hm.logger.Infof("Hibernation confirmed, checking safety conditions")
	}

	// Check kickstand state - must be down (always required)
	if hm.isKickstandDown != nil {
		kickstandDown, err := hm.isKickstandDown()
		if err != nil {
			hm.logger.Infof("Failed to read kickstand state: %v, aborting hibernation", err)
			hm.resetToIdle()
			return
		}
		if !kickstandDown {
			hm.logger.Infof("Kickstand is up, aborting hibernation")
			hm.resetToIdle()
			return
		}
	}

	// Check seatbox state - must be closed (skip for auto-confirm)
	if !autoConfirm && hm.isSeatboxClosed != nil {
		seatboxClosed, err := hm.isSeatboxClosed()
		if err != nil {
			hm.logger.Infof("Failed to read seatbox state: %v, aborting hibernation", err)
			hm.resetToIdle()
			return
		}
		if !seatboxClosed {
			hm.logger.Infof("Seatbox is open, transitioning to seatbox notification state")
			if hm.transitionTo != nil {
				if err := hm.transitionTo(types.StateWaitingHibernationSeatbox); err != nil {
					hm.logger.Infof("Failed to transition to waiting-hibernation-seatbox: %v", err)
				}
			}
			// Don't reset to idle - stay in confirmation waiting for seatbox to close
			return
		}
	} else if autoConfirm {
		hm.logger.Infof("Auto-confirm mode: skipping seatbox check (warehouse mode)")
	}

	hm.logger.Infof("Safety checks passed, transitioning to confirmation state")

	// Transition to 3-second confirmation state
	if hm.transitionTo != nil {
		if err := hm.transitionTo(types.StateWaitingHibernationConfirm); err != nil {
			hm.logger.Infof("Failed to transition to waiting-hibernation-confirm: %v", err)
			hm.resetToIdle()
			return
		}
	}

	// Start 3-second non-abortable confirmation timer
	time.AfterFunc(3*time.Second, func() {
		hm.logger.Infof("Confirmation timer complete, executing hibernation")
		// Execute hibernation callback
		if hm.onHibernationConfirm != nil {
			hm.onHibernationConfirm()
		}
		// Reset to idle
		hm.resetToIdle()
	})
}

// cancelHibernation cancels the hibernation sequence
func (hm *HibernationManager) cancelHibernation() {
	hm.mu.Lock()
	defer hm.mu.Unlock()

	if hm.state != HibernationIdle {
		hm.logger.Infof("Hibernation cancelled")
		hm.resetToIdle()
	}
}

// resetToIdle resets the hibernation manager to idle state
func (hm *HibernationManager) resetToIdle() {
	// Stop all timers
	if hm.initialTimer != nil {
		hm.initialTimer.Stop()
		hm.initialTimer = nil
	}
	if hm.confirmationTimer != nil {
		hm.confirmationTimer.Stop()
		hm.confirmationTimer = nil
	}
	if hm.reholdTimer != nil {
		hm.reholdTimer.Stop()
		hm.reholdTimer = nil
	}

	// Reset state
	hm.state = HibernationIdle
	hm.bothBrakesHeld = false
	hm.brakesReleased = false
	hm.startTime = time.Time{}
	hm.reholdStartTime = time.Time{}

	// Transition vehicle back to parked state
	if hm.transitionTo != nil {
		if err := hm.transitionTo(types.StateParked); err != nil {
			hm.logger.Infof("Failed to transition back to parked: %v", err)
		}
	}
}

// handleKeycardConfirmation handles keycard tap during confirmation phase
func (hm *HibernationManager) handleKeycardConfirmation() {
	hm.mu.Lock()
	defer hm.mu.Unlock()

	if hm.state == HibernationConfirmation && hm.brakesReleased {
		hm.logger.Infof("Keycard confirmation received")
		hm.confirmHibernation(false) // Keycard confirmation - require seatbox closed
	}
}


// triggerShutdownTimeout handles the shutdown timer expiration and transitions to standby
func (v *VehicleSystem) triggerShutdownTimeout() {
	v.logger.Infof("Shutdown timer expired, transitioning to standby...")

	v.mu.RLock()
	hibernationRequested := v.hibernationRequest
	v.mu.RUnlock()

	// Transition to standby
	if err := v.transitionTo(types.StateStandby); err != nil {
		v.logger.Infof("Failed to transition to standby after shutdown timeout: %v", err)
		return
	}

	// If hibernation was requested, execute it after the state transition
	if hibernationRequested {
		v.logger.Infof("Hibernation was requested, executing hibernation...")
		if err := v.redis.SendCommand("scooter:power", "hibernate-manual"); err != nil {
			v.logger.Infof("Failed to send hibernate command after shutdown: %v", err)
		}
	}

	// Reset the timer field after it has fired
	v.mu.Lock()
	v.shutdownTimer = nil
	v.hibernationRequest = false
	v.mu.Unlock()
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
	currentState := v.state
	v.mu.Unlock()

	v.logger.Infof("Current state: %s, Dashboard ready: %v", currentState, ready)

	if !ready && currentState == types.StateReadyToDrive {
		v.logger.Infof("Dashboard not ready, transitioning to PARKED")
		return v.transitionTo(types.StateParked)
	}

	// Only try to transition to READY_TO_DRIVE if dashboard is ready
	if ready {
		// Don't process kickstand state in STANDBY state
		if currentState == types.StateStandby {
			v.logger.Infof("Skipping kickstand check in %s state", currentState)
			return nil
		}

		kickstandValue, err := v.io.ReadDigitalInput("kickstand")
		if err != nil {
			v.logger.Infof("Failed to read kickstand: %v", err)
			return err
		}

		v.logger.Infof("Kickstand state: %v", kickstandValue)
		if !kickstandValue && v.isReadyToDrive() {
			v.logger.Infof("Dashboard ready and kickstand up, transitioning to READY_TO_DRIVE")
			return v.transitionTo(types.StateReadyToDrive)
		} else if kickstandValue {
			v.logger.Infof("Kickstand down, staying in/transitioning to PARKED")
			return v.transitionTo(types.StateParked)
		}
	}

	return nil
}

func (v *VehicleSystem) handleInputChange(channel string, value bool) error {
	v.logger.Infof("Input %s => %v", channel, value)

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
				v.logger.Infof("Failed to read kickstand: %v", err)
				return err
			}

			if !kickstandValue {
				// Check if both brakes are held
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

				if brakeLeft && brakeRight {
					v.logger.Infof("Manual ready-to-drive activation: kickstand up, both brakes held, seatbox button pressed")

					// Blink the main light once for confirmation
					if err := v.io.PlayPwmCue(3); err != nil { // LED_PARKED_TO_DRIVE
						v.logger.Infof("Failed to play LED cue: %v", err)
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
		v.logger.Infof("Kickstand changed: %v, current state: %s", value, currentState)

		// Cancel hibernation if kickstand is raised
		if !value {
			v.hibernationManager.cancelHibernation()
		}

		if currentState == types.StateStandby {
			v.logger.Infof("Ignoring kickstand change in %s state", currentState)
			return nil
		}
		if err := v.redis.SetKickstandState(value); err != nil {
			return err
		}
		if value {
			// Kickstand down - check debounce if coming from ready-to-drive
			if currentState == types.StateReadyToDrive {
				v.mu.RLock()
				entryTime := v.readyToDriveEntryTime
				v.mu.RUnlock()

				timeSinceEntry := time.Since(entryTime)
				if timeSinceEntry < parkDebounceTime {
					v.logger.Infof("Kickstand down ignored - debounce protection (%.2fs since entering ready-to-drive)", timeSinceEntry.Seconds())
					return nil
				}
			}
			// Kickstand down - go to PARKED
			v.logger.Infof("Kickstand down, transitioning to PARKED")
			return v.transitionTo(types.StateParked)
		} else if v.isReadyToDrive() {
			// Kickstand up and dashboard ready - go to READY_TO_DRIVE
			v.logger.Infof("Kickstand up and dashboard ready, transitioning to READY_TO_DRIVE")
			return v.transitionTo(types.StateReadyToDrive)
		}

	case "seatbox_button":
		// Cancel hibernation on seatbox button press
		if value {
			v.hibernationManager.cancelHibernation()
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

	// --- Force Standby Check ---
	brakeLeft, errL := v.io.ReadDigitalInput("brake_left")
	if errL != nil {
		v.logger.Infof("Warning: Failed to read brake_left for keycard auth, assuming not pressed: %v", errL)
		brakeLeft = false
	}
	brakeRight, errR := v.io.ReadDigitalInput("brake_right")
	if errR != nil {
		v.logger.Infof("Warning: Failed to read brake_right for keycard auth, assuming not pressed: %v", errR)
		brakeRight = false
	}
	brakePressed := brakeLeft || brakeRight

	currentTime := time.Now()
	performForcedStandby := false

	v.mu.Lock()
	if v.lastKeycardTapTime.IsZero() || currentTime.Sub(v.lastKeycardTapTime) > keycardTapMaxInterval {
		v.keycardTapCount = 1
		v.logger.Infof("Keycard tap sequence: Start/Reset. Count: 1")
	} else {
		v.keycardTapCount++
		v.logger.Infof("Keycard tap sequence: Incremented. Count: %d", v.keycardTapCount)
	}
	v.lastKeycardTapTime = currentTime

	if v.keycardTapCount >= keycardForceStandbyTaps {
		if brakePressed {
			v.logger.Infof("Force standby condition met: %d taps, brake pressed.", v.keycardTapCount)
			v.forceStandbyNoLock = true
			performForcedStandby = true
		} else {
			v.logger.Infof("Force standby condition NOT met: %d taps, but brake not pressed. Resetting count.", v.keycardTapCount)
		}
		v.keycardTapCount = 0 // Reset after 3 taps, regardless of brake, for the next sequence
	}
	v.mu.Unlock()

	if performForcedStandby {
		v.logger.Infof("Transitioning to STANDBY (forced, no lock).")
		// The forceStandbyNoLock flag will be read and reset by transitionTo
		return v.transitionTo(types.StateStandby)
	}
	// --- End Force Standby Check ---

	// --- Hibernation Confirmation Check ---
	// Check if this keycard tap should confirm hibernation
	v.hibernationManager.handleKeycardConfirmation()

	// ----- Original keycardAuthPassed logic continues if not forced standby -----
	currentState := v.getCurrentState()

	v.logger.Infof("Current state during keycard auth (normal flow): %s", currentState)

	if currentState == types.StateStandby {
		v.logger.Infof("Reading kickstand state")
		kickstandValue, err := v.io.ReadDigitalInput("kickstand")
		if err != nil {
			v.logger.Infof("Failed to read kickstand: %v", err)
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
		v.logger.Infof("Failed to read kickstand: %v", err)
		return fmt.Errorf("failed to read kickstand: %w", err)
	}
	if !kickstandValue {
		v.logger.Infof("Cannot transition to STANDBY: kickstand not down")
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

