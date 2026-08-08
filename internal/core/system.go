package core

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/librescoot/librefsm"

	"vehicle-service/internal/fsm"
	"vehicle-service/internal/hardware"
	"vehicle-service/internal/led"
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
	blinkerInterval           = 800 * time.Millisecond
	dbcLedSampleInterval      = 33 * time.Millisecond // ~30 Hz DBC LED brightness updates
	handlebarPositionDebounce = 250 * time.Millisecond
	brakeDebounce             = 20 * time.Millisecond
	// Short debounce on keys affected by spurious resume edges. gpio-keys
	// re-reads every line in gpio_keys_resume() and can emit a
	// release-then-press pair within milliseconds when the pad reads
	// differently across the sleep boundary. The debounce collapses the
	// pair; hardware IO then dedupes against the last-delivered value.
	resumeSettleDebounce      = 50 * time.Millisecond
	handlebarLockDuration     = 1100 * time.Millisecond
	handlebarLockWindow       = 60 * time.Second
	handlebarLockRetries      = 3
	handlebarLockRetryDelay   = 500 * time.Millisecond
	handlebarUnlockRetries    = 3
	handlebarUnlockRetryDelay = 500 * time.Millisecond
	seatboxLockDuration       = 200 * time.Millisecond
	parkDebounceTime          = 1 * time.Second

	// DBC update watchdog — reset on every OTA status write from DBC
	dbcUpdateWatchdogTimeout = 15 * time.Minute

	// Lifetime of the boot restore marker. Comfortably longer than a systemd
	// restart (RestartSec defaults to 100ms) and short enough that a marker
	// left behind by something unrelated cannot cost a legitimate restore
	// hours later.
	restoreAttemptTTL = 60 * time.Second
)

// minLockOnDisconnectSeconds is the floor for a non-zero
// scooter.lock-on-bluetooth-disconnect-seconds value. Values between 1 and
// this floor are clamped up so the countdown overlay stays meaningful.
const minLockOnDisconnectSeconds = 5

type VehicleSystem struct {
	state                  types.SystemState
	dashboardReady         bool
	logger                 *logger.Logger
	io                     HardwareIO
	redis                  MessagingClient
	mu                     sync.RWMutex
	blinkerState           atomic.Int32 // BlinkerState; written from IO callbacks and the Redis blinker handler
	blinkerStopChan        chan struct{}
	blinkerExited          chan struct{}     // closed by runBlinker when it returns; stopBlinker waits on it
	blinkerStartNanos      atomic.Int64      // UnixNano when blinker goroutine started (0 if inactive)
	blinkerCueIndex        atomic.Int32      // Currently playing blinker cue index (-1 if none)
	menuOpen               atomic.Bool       // True while scootui-qt reports its menu is open; suppresses brake-light LED cues
	dbcPoweroffSent        atomic.Bool       // True once EnterShuttingDown published dbc:command poweroff; cleared on EnterShuttingDown entry and EnterStandby
	pendingUnlock          atomic.Bool       // True when an unlock arrived during a committed shutdown; replayed from EnterStandby
	ledCurves              *led.CurveLibrary // LED fade/cue metadata for timing
	initialized            bool
	handlebarUnlocked      bool          // Track if handlebar has been unlocked in this power cycle
	handlebarLatchedLocked bool          // Last commanded/confirmed lock state, immune to spurious sensor edges
	handlebarLatchInit     bool          // True once latch has been seeded from sensor or actuation
	handlebarTimer         *time.Timer   // Timer for handlebar position window
	handlebarDone          chan struct{} // Done channel for handlebar lock goroutine
	handlebarUnlockDone    chan struct{} // Done channel for handlebar unlock goroutine
	readyToDriveEntryTime  time.Time     // Track when we entered ready-to-drive state for park debounce
	kickstandDebounceTimer *time.Timer   // Deferred kickstand-down check after debounce window
	keycardTapCount        int
	lastKeycardTapTime     time.Time
	forceStandbyNoLock     bool
	// handlebarUnlockedOverride is set by the scooter.handlebar-unlocked
	// setting (service mode). While true, auto re-lock on standby/shutdown is
	// suppressed and the latch is held released. Guarded by mu.
	handlebarUnlockedOverride  bool
	hibernationRequest         bool              // Track if hibernation was requested during shutdown
	shutdownFromParked         bool              // Track if shutdown was initiated from parked state
	dbcUpdating                bool              // Track if DBC update is in progress
	dbcWatchdogTimer           *time.Timer       // Watchdog timer for DBC updates, reset on OTA activity
	dbcWatchdogGeneration      uint64            // Generation counter to invalidate stale callbacks
	deferredDashboardPower     *bool             // Deferred dashboard power state (nil = no change needed)
	brakeHibernationEnabled    bool              // Track if brake lever hibernation is enabled (default: true)
	autoStandbySeconds         int               // Auto-standby timeout in seconds (0 = disabled)
	lockOnBleDisconnectSeconds int               // Grace seconds before locking after BLE disconnect while parked (0 = disabled)
	keylessCountdownActive     bool              // A lock-on-disconnect countdown is currently armed
	lastBleStatus              string            // Last observed ble/status ("connected"/"disconnected")
	hornEnableMode             string            // Horn enable mode: "true", "false", or "in-drive" (default: "true")
	hornWhenSeatboxOpen        bool              // Allow manual horn button while seatbox open + unlocked (default: false = muted, guards against the seat lid honking)
	seatboxClosed              bool              // Cached seatbox lock sensor state (true = closed)
	dbcBlinkerLed              bool              // Blink DBC boot LED in sync with blinkers (default: false)
	usb0Policy                 string            // "auto" (default, tracks dashboard_power) or "always-on"
	hibernationForceTimer      *time.Timer       // Timer for forcing hibernation after 15s of brake hold
	machine                    *librefsm.Machine // librefsm state machine
	gestures                   *gestureDetector

	// Hop-on bookkeeping: only the steering-lock latch survives across
	// the EnterHopOn / ExitHopOn pair so we know whether to release on exit.
	hopOnLockedHandlebar bool
	autoStandbyDeadline  time.Time // Live auto-standby deadline (set by EnterAtRest, cleared by ExitAtRest)
}

func NewVehicleSystem(io HardwareIO, redis MessagingClient, l *logger.Logger) *VehicleSystem {
	vs := &VehicleSystem{
		state:                   types.StateStandby,
		logger:                  l.WithTag("Vehicle"),
		io:                      io,
		redis:                   redis,
		initialized:             false,
		keycardTapCount:         0,
		forceStandbyNoLock:      false,
		brakeHibernationEnabled: true,   // Default to enabled for backward compatibility
		hornEnableMode:          "true", // Default to always enabled for backward compatibility
		hornWhenSeatboxOpen:     false,  // Default: mute manual horn while seatbox open in unlocked states
		seatboxClosed:           true,   // Safe default until first sensor read (closed = no suppression)
		usb0Policy:              "auto", // Default: bring usb0 down in standby; setPower tracks dashboard_power
	}
	vs.blinkerCueIndex.Store(-1)
	vs.gestures = newGestureDetector(func(event string) {
		if err := vs.redis.PublishInputEvent(event); err != nil {
			vs.logger.Debugf("Failed to publish input event: %v", err)
		}
	}, gestureLongTapThreshold, gestureHoldThreshold, gestureDoubleTapThreshold)

	// Set up Redis callbacks so they are available as soon as the system object
	// exists, even before Start() is called (enables unit tests to verify wiring).
	vs.redis.SetCallbacks(messaging.Callbacks{
		DashboardCallback:      vs.handleDashboardReady,
		KeycardCallback:        vs.keycardAuthPassed,
		SeatboxCallback:        vs.handleSeatboxRequest,
		HornCallback:           vs.handleHornRequest,
		BlinkerCallback:        vs.handleBlinkerRequest,
		StateCallback:          vs.handleStateRequest,
		ForceLockCallback:      vs.handleForceLockRequest,
		LedCueCallback:         vs.handleLedCueRequest,
		LedFadeCallback:        vs.handleLedFadeRequest,
		UpdateCallback:         vs.handleUpdateRequest,
		HardwareCallback:       vs.handleHardwareRequest,
		SettingsCallback:       vs.handleSettingsUpdate,
		OtaDbcActivityCallback: vs.resetDbcWatchdog,
		HopOnCallback:          vs.handleHopOnRequest,
		PowerStateCallback:     vs.handlePowerStateChange,
		MenuOpenCallback: func(open bool) error {
			vs.menuOpen.Store(open)
			return nil
		},
		BleCallback: vs.handleBleStatusChange,
	})
	return vs
}

func (v *VehicleSystem) Start() error {
	v.logger.Infof("Starting vehicle system")

	// Load LED curve library for blinker timing
	v.ledCurves = led.NewCurveLibrary(v.logger)
	if err := v.ledCurves.Load(); err != nil {
		v.logger.Warnf("Failed to load LED curves (blinker timing may be imprecise): %v", err)
	}

	// Connect to Redis first so we can retrieve saved state
	if err := v.redis.Connect(); err != nil {
		return fmt.Errorf("failed to connect to Redis: %w", err)
	}

	// Load initial state from Redis for restoration after FSM starts
	savedState, err := v.redis.GetVehicleState()
	if err != nil {
		v.logger.Infof("Failed to get saved state from Redis: %v", err)
		savedState = "" // No saved state available; FSM stays in default Standby
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

	// Read initial auto-standby setting from Redis. Default 900 s (15 minutes)
	// when unset; the last 60 s are shown as a cancellable countdown on the
	// dashboard. Set to 0 to disable.
	const defaultAutoStandbySeconds = 900
	v.mu.Lock()
	v.autoStandbySeconds = defaultAutoStandbySeconds
	v.mu.Unlock()
	autoStandbySetting, err := v.redis.GetHashField("settings", "scooter.auto-standby-seconds")
	if err != nil {
		v.logger.Warnf("Failed to read auto-standby setting on startup: %v (using default %d s)", err, defaultAutoStandbySeconds)
	} else if autoStandbySetting != "" {
		seconds, parseErr := strconv.Atoi(autoStandbySetting)
		if parseErr != nil {
			v.logger.Warnf("Invalid auto-standby setting value on startup: '%s', using default (%d)", autoStandbySetting, defaultAutoStandbySeconds)
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
		v.logger.Infof("No auto-standby setting found on startup, using default (%d seconds)", defaultAutoStandbySeconds)
	}

	v.mu.Lock()
	v.lockOnBleDisconnectSeconds = 0
	v.mu.Unlock()
	if lockSetting, err := v.redis.GetHashField("settings", "scooter.lock-on-bluetooth-disconnect-seconds"); err != nil {
		v.logger.Infof("No lock-on-bluetooth-disconnect setting on startup (using disabled)")
	} else if lockSetting != "" {
		if secs, perr := strconv.Atoi(lockSetting); perr != nil {
			v.logger.Warnf("Invalid lock-on-bluetooth-disconnect setting on startup: '%s' (using disabled)", lockSetting)
		} else {
			clamped := v.clampLockOnDisconnect(secs)
			v.mu.Lock()
			v.lockOnBleDisconnectSeconds = clamped
			v.mu.Unlock()
		}
	}

	// Read initial horn enable mode setting from Redis
	hornEnableModeSetting, err := v.redis.GetHashField("settings", "scooter.enable-horn")
	if err != nil {
		v.logger.Warnf("Failed to read horn enable mode setting on startup: %v", err)
		// Continue with default (true = always enabled)
	} else if hornEnableModeSetting != "" {
		v.mu.Lock()
		if hornEnableModeSetting == "true" || hornEnableModeSetting == "false" || hornEnableModeSetting == "in-drive" {
			v.hornEnableMode = hornEnableModeSetting
			v.logger.Infof("Horn enable mode setting on startup: %s", hornEnableModeSetting)
		} else {
			v.logger.Warnf("Invalid horn enable mode setting value on startup: '%s', using default (true)", hornEnableModeSetting)
			v.hornEnableMode = "true"
		}
		v.mu.Unlock()
	} else {
		v.logger.Infof("No horn enable mode setting found on startup, using default (true)")
	}

	// Read initial "horn while seatbox open" setting from Redis
	hornWhenSeatboxOpenSetting, err := v.redis.GetHashField("settings", "scooter.horn-when-seatbox-open")
	if err != nil {
		v.logger.Warnf("Failed to read horn-when-seatbox-open setting on startup: %v", err)
		// Continue with default (false = mute manual horn while seatbox open)
	} else if hornWhenSeatboxOpenSetting != "" {
		v.mu.Lock()
		v.hornWhenSeatboxOpen = hornWhenSeatboxOpenSetting == "true"
		v.mu.Unlock()
		v.logger.Infof("Horn-when-seatbox-open setting on startup: %s", hornWhenSeatboxOpenSetting)
	} else {
		v.logger.Infof("No horn-when-seatbox-open setting found on startup, using default (false)")
	}

	// Read initial DBC blinker LED setting from Redis
	dbcBlinkerLedSetting, err := v.redis.GetHashField("settings", "scooter.dbc-blinker-led")
	if err != nil {
		v.logger.Warnf("Failed to read DBC blinker LED setting on startup: %v", err)
	} else if dbcBlinkerLedSetting != "" {
		v.mu.Lock()
		v.dbcBlinkerLed = dbcBlinkerLedSetting == "enabled"
		v.mu.Unlock()
		v.logger.Infof("DBC blinker LED setting on startup: %s", dbcBlinkerLedSetting)
	}

	// Check if DBC update is in progress and restore dbcUpdating flag
	// First check the explicit vehicle:dbc-updating flag
	restoreDbcUpdate := false
	dbcUpdating, err := v.redis.GetDbcUpdating()
	if err != nil {
		v.logger.Warnf("Failed to get DBC updating flag on startup: %v", err)
	} else if dbcUpdating {
		v.logger.Infof("DBC updating flag set on startup, restoring dbcUpdating flag")
		restoreDbcUpdate = true
	}

	// Also check OTA status as a fallback/secondary check
	dbcStatus, err := v.redis.GetOtaStatus("dbc")
	if err != nil {
		v.logger.Warnf("Failed to get DBC OTA status on startup: %v", err)
	} else if dbcStatus == "downloading" || dbcStatus == "preparing" || dbcStatus == "installing" || dbcStatus == "pending-reboot" {
		v.logger.Infof("DBC update in progress on startup (status=%s), restoring dbcUpdating flag", dbcStatus)
		if !restoreDbcUpdate {
			// Sync the Redis flag if it wasn't already set
			if err := v.redis.SetDbcUpdating(true); err != nil {
				v.logger.Warnf("Failed to sync DBC updating flag to Redis: %v", err)
			}
		}
		restoreDbcUpdate = true
	}

	if restoreDbcUpdate {
		v.mu.Lock()
		v.dbcUpdating = true
		v.startDbcWatchdog()
		v.mu.Unlock()
		v.logger.Infof("Restored DBC updating state, watchdog started (timeout: %v)", dbcUpdateWatchdogTimeout)

		// Re-set suspend-only inhibitor so pm-service keeps MDB awake during DBC update
		if err := v.redis.SetInhibitor("dbc-update", "suspend-only", "DBC update in progress (restored on startup)"); err != nil {
			v.logger.Warnf("Failed to set DBC update inhibitor on startup: %v", err)
		}
	} else {
		// Not restoring DBC update, clean up any stale inhibitor from a previous run
		if err := v.redis.RemoveInhibitor("dbc-update"); err != nil {
			v.logger.Warnf("Failed to remove stale DBC update inhibitor on startup: %v", err)
		}
	}

	// Read dashboard power from Redis BEFORE hardware initialization
	// This ensures GPIO starts with correct value (no power interruption)
	if savedState != "" && savedState != types.StateShuttingDown {
		dashboardPower, err := v.redis.GetDashboardPower()
		if err != nil {
			v.logger.Warnf("Failed to read dashboard power from Redis: %v", err)
			// Fallback to state-based logic. Hop-on family runs the
			// dashboard alive (lock screen / learn overlay) so it
			// belongs in the powered set alongside parked/RTD.
			v.mu.RLock()
			dashboardPower = savedState == types.StateReadyToDrive ||
				savedState == types.StateParked ||
				savedState == types.StateHopOn ||
				savedState == types.StateHopOnLearning ||
				v.dbcUpdating
			v.mu.RUnlock()
		} else {
			// Override if DBC is updating (keep dashboard powered during updates)
			v.mu.RLock()
			if v.dbcUpdating {
				dashboardPower = true
			}
			v.mu.RUnlock()
		}
		v.logger.Infof("Setting initial dashboard power: %v", dashboardPower)
		v.io.SetInitialValue("dashboard_power", dashboardPower)

		// Sync the UART PPP link to the restored dashboard power state. The
		// unit is not enabled at boot (see the ppp-link recipe), so without
		// this a vehicle-service restart would leave it in a stale state.
		if err := v.io.SetPppLinkEnabled(dashboardPower); err != nil {
			v.logger.Warnf("Failed to sync ppp-link to dashboard power: %v", err)
		}

		// Also set engine power initial value. Hop-on family runs with
		// parked-equivalent rails (the firmware HOP_ON state explicitly
		// returns POWER_MODE_ACTIVE), so engine_power must come up ON
		// when restoring into hop-on or hop-on-learning.
		enginePower := savedState == types.StateReadyToDrive ||
			savedState == types.StateParked ||
			savedState == types.StateHopOn ||
			savedState == types.StateHopOnLearning
		v.io.SetInitialValue("engine_power", enginePower)
		if err := v.redis.SetEnginePower(enginePower); err != nil {
			v.logger.Warnf("Failed to publish initial engine power state to Redis: %v", err)
		}
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

	// Every GPIO line has just been re-requested successfully, so nothing this
	// process knows of is standing. Reconcile before the FSM starts, so a fault
	// raised by the state restore below survives its own boot.
	v.clearOwnedFaults()

	// Configure per-channel debounce before registering callbacks
	v.io.SetDebounce("handlebar_position", handlebarPositionDebounce)
	v.io.SetDebounce("brake_left", brakeDebounce)
	v.io.SetDebounce("brake_right", brakeDebounce)
	v.io.SetDebounce("kickstand", resumeSettleDebounce)
	v.io.SetDebounce("seatbox_lock_sensor", resumeSettleDebounce)

	// Apply usb0 policy up front so an installer reboot has a reachable
	// interface before the FSM restore runs setPower. Default ("auto")
	// hands the wheel to setPower so usb0 follows dashboard_power;
	// "always-on" keeps usb0 up regardless. The policy is a user setting
	// in the `settings` hash.
	usb0PolicySetting, err := v.redis.GetHashField("settings", "scooter.usb0-policy")
	if err != nil {
		v.logger.Warnf("Failed to read usb0 policy setting on startup: %v (using default auto)", err)
	} else if usb0PolicySetting == "always-on" {
		v.mu.Lock()
		v.usb0Policy = "always-on"
		v.mu.Unlock()
	} else if usb0PolicySetting != "" && usb0PolicySetting != "auto" {
		v.logger.Warnf("Unknown usb0 policy value on startup: %q, using default auto", usb0PolicySetting)
	}
	v.mu.RLock()
	policy := v.usb0Policy
	v.mu.RUnlock()
	effective := v.usb0AutoEffective()
	v.logger.Infof("usb0 policy=%s, auto-effective=%v", policy, effective)
	if !effective {
		if err := v.io.SetUsb0Enabled(true); err != nil {
			v.logger.Warnf("Failed to bring usb0 up at startup: %v", err)
		}
	}

	// Initialize and start librefsm state machine now that hardware is up.
	// EnterStandby (the default initial state) drives PWM/GPIO immediately,
	// so the FSM cannot run before v.io.Initialize() has wired things up.
	if err := v.initFSM(context.Background()); err != nil {
		return fmt.Errorf("failed to initialize FSM: %w", err)
	}

	// Restore saved FSM state now that hardware is initialized. A restore that
	// cannot happen is not a startup failure, see restoreFSMState.
	v.restoreFSMState(savedState)

	// The state the machine actually ended up in. Everything below that used to
	// branch on savedState (the value Redis asked for) has to branch on this
	// instead, or a declined restore gets silently undone a few lines later by
	// hardware asserted from stale intent.
	restoredState := stateIDToSystemState(v.getCurrentStateID())

	// Check initial handlebar lock sensor state
	handlebarLockSensorRaw, err := v.io.ReadDigitalInputDirect("handlebar_lock_sensor")
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
				// Cache sensor state (value true = closed) for the horn gate
				v.mu.Lock()
				v.seatboxClosed = value
				v.mu.Unlock()
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
		"48v_detect":            v.redis.SetMainPower,
	}

	for sensor, publisher := range sensorPublishers {
		value, err := v.io.ReadDigitalInput(sensor)
		if err != nil {
			v.logger.Infof("Warning: Failed to read initial state for %s: %v", sensor, err)
			continue
		}
		v.logger.Infof("Initial state %s: %s", sensor, hardware.LabelValue(sensor, value))

		if err := publisher(value); err != nil {
			v.logger.Infof("Warning: Failed to publish initial state for %s to Redis: %v", sensor, err)
		}

		// Seed the cached seatbox state for the horn gate (value true = closed)
		if sensor == "seatbox_lock_sensor" {
			v.mu.Lock()
			v.seatboxClosed = value
			v.mu.Unlock()
		}
	}

	// Seed the latched lock state from the raw sensor. Subsequent updates
	// only happen on confirmed actuations and on Standby entry.
	v.resyncHandlebarLatchFromSensor()

	// Now that hardware is initialized, set engine brake for the state the
	// machine is in. Only drive mode lets the brake follow the physical levers;
	// every other state keeps it engaged so the motor stays disabled.
	engineBrake := true
	if restoredState == types.StateReadyToDrive {
		brakeLeft, err := v.io.ReadDigitalInput("brake_left")
		if err != nil {
			v.logger.Errorf("Failed to read brake_left: %v", err)
		}
		brakeRight, err := v.io.ReadDigitalInput("brake_right")
		if err != nil {
			v.logger.Errorf("Failed to read brake_right: %v", err)
		}
		engineBrake = brakeLeft || brakeRight
	}
	if err := v.writeOutput("engine_brake", engineBrake); err != nil {
		v.logger.Errorf("Failed to set engine brake: %v", err)
	}

	// Play LED cues based on restored state
	if savedState == types.StateReadyToDrive || savedState == types.StateParked {
		// Read brake states to determine which LED cue to play
		brakeLeft, err := v.io.ReadDigitalInput("brake_left")
		if err != nil {
			v.logger.Errorf("Failed to read brake_left: %v", err)
		}
		brakeRight, err := v.io.ReadDigitalInput("brake_right")
		if err != nil {
			v.logger.Errorf("Failed to read brake_right: %v", err)
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

// startAutoStandbyTimer starts the auto-standby timer using librefsm.
//
// SAFETY: The auto-standby timer must NEVER run outside of StateParked. This
// function refuses to start the timer in any other state — defense in depth,
// in addition to the per-callsite checks in resetAutoStandbyTimer and
// handleSettingsUpdate. The FSM also restricts EvAutoStandbyTimeout to a
// single transition (StateParked -> StateShuttingDown), and ExitParked stops
// the timer on state exit, but this guard exists so a future caller cannot
// accidentally arm the timer outside parked.
func (v *VehicleSystem) startAutoStandbyTimer() {
	currentState := v.getCurrentState()
	if currentState != types.StateParked {
		v.logger.Warnf("Refusing to start auto-standby timer: not in parked state (current: %s)", currentState)
		return
	}

	v.mu.RLock()
	seconds := v.autoStandbySeconds
	v.mu.RUnlock()

	if seconds <= 0 {
		return
	}

	// Debug level: this fires on every resetAutoStandbyTimer call (brake,
	// kickstand, seatbox). The meaningful "timer armed" log is emitted once
	// from EnterParked. See fsm_actions.go `Started auto-standby timer`.
	v.logger.Debugf("Starting auto-standby timer: %d seconds", seconds)
	v.startStandbyCountdown(time.Duration(seconds) * time.Second)
}

// startStandbyCountdown arms TimerAutoStandby for the given duration (firing
// EvAutoStandbyTimeout) and publishes the deadline so the dashboard countdown
// overlay can render it. Callers own all precondition checks; this is reused by
// both the idle auto-standby path and the lock-on-disconnect path.
func (v *VehicleSystem) startStandbyCountdown(duration time.Duration) {
	if v.machine == nil || duration <= 0 {
		return
	}
	v.machine.StartTimer(fsm.TimerAutoStandby, duration, librefsm.Event{ID: fsm.EvAutoStandbyTimeout})
	deadline := time.Now().Add(duration)
	if err := v.redis.PublishAutoStandbyDeadline(deadline); err != nil {
		v.logger.Warnf("Failed to publish auto-standby deadline: %v", err)
	}
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

// resetAutoStandbyTimer resets the auto-standby timer on rider interaction. If a
// lock-on-disconnect countdown is active it is cancelled first (regardless of the
// idle auto-standby setting), then the idle timer is re-armed when applicable.
func (v *VehicleSystem) resetAutoStandbyTimer() {
	v.mu.RLock()
	countingDown := v.keylessCountdownActive
	autoStandbySeconds := v.autoStandbySeconds
	v.mu.RUnlock()

	if countingDown {
		v.logger.Debugf("Interaction during lock-on-disconnect countdown; canceling")
		v.clearKeylessCountdown()
		return
	}

	currentState := v.getCurrentState()
	if currentState != types.StateParked || autoStandbySeconds <= 0 {
		return
	}

	v.logger.Debugf("Resetting auto-standby timer")
	v.cancelAutoStandbyTimer()
	v.startAutoStandbyTimer()
}

// handleBleStatusChange reacts to the bluetooth-service ble/status field. A
// connected->disconnected edge while parked, with the feature enabled, arms a
// short standby countdown; a reconnect during that window cancels it.
func (v *VehicleSystem) handleBleStatusChange(status string) error {
	v.mu.Lock()
	prev := v.lastBleStatus
	v.lastBleStatus = status
	graceSeconds := v.lockOnBleDisconnectSeconds
	countingDown := v.keylessCountdownActive
	v.mu.Unlock()

	switch status {
	case "disconnected":
		if prev != "connected" || graceSeconds <= 0 || countingDown {
			return nil
		}
		if v.getCurrentState() != types.StateParked {
			return nil
		}
		v.mu.Lock()
		v.keylessCountdownActive = true
		v.mu.Unlock()
		v.logger.Infof("BLE disconnected while parked; locking in %d seconds", graceSeconds)
		v.cancelAutoStandbyTimer()
		v.startStandbyCountdown(time.Duration(graceSeconds) * time.Second)
	case "connected":
		if countingDown {
			v.logger.Infof("BLE reconnected during lock countdown; canceling")
			v.clearKeylessCountdown()
		}
	}
	return nil
}

// clearKeylessCountdown cancels an active lock-on-disconnect countdown and, when
// still parked with idle auto-standby enabled, restores the normal idle timer.
func (v *VehicleSystem) clearKeylessCountdown() {
	v.mu.Lock()
	v.keylessCountdownActive = false
	idleSeconds := v.autoStandbySeconds
	v.mu.Unlock()

	v.cancelAutoStandbyTimer()
	if v.getCurrentState() == types.StateParked && idleSeconds > 0 {
		v.startAutoStandbyTimer()
	}
}

// clampLockOnDisconnect normalises a lock-on-disconnect setting value:
// negatives become 0 (off); 1..min become the floor; 0 and >=min pass through.
func (v *VehicleSystem) clampLockOnDisconnect(seconds int) int {
	if seconds < 0 {
		return 0
	}
	if seconds > 0 && seconds < minLockOnDisconnectSeconds {
		v.logger.Warnf("lock-on-bluetooth-disconnect-seconds %d below minimum, clamping to %d", seconds, minLockOnDisconnectSeconds)
		return minLockOnDisconnectSeconds
	}
	return seconds
}

// isInHopOnFamily reports whether the FSM is currently in the locked
// (StateHopOn) or learning (StateHopOnLearning) sub-state. Used to gate
// hardware side-effects (horn output, brake LED, blinker LED, seatbox
// open) so the scooter stays "quiet" while the dashboard drives a
// combo-matching UI on top.
func (v *VehicleSystem) isInHopOnFamily() bool {
	if v.machine == nil {
		return false
	}
	return v.machine.IsInState(fsm.StateHopOn) || v.machine.IsInState(fsm.StateHopOnLearning)
}

// isHornAllowed checks if horn activation is permitted based on current setting and vehicle state
func (v *VehicleSystem) isHornAllowed() bool {
	v.mu.RLock()
	mode := v.hornEnableMode
	v.mu.RUnlock()

	switch mode {
	case "false":
		return false
	case "in-drive":
		return v.getCurrentState() == types.StateReadyToDrive
	default: // "true" or any other value defaults to enabled
		return true
	}
}

// seatboxBlocksHorn reports whether the manual horn button should be muted
// because the open seat lid can rest on it. Only applies while the seatbox is
// open and the scooter is unlocked (any non-standby state). The alarm and
// remote/CLI horn paths (scooter:horn) are unaffected.
func (v *VehicleSystem) seatboxBlocksHorn() bool {
	v.mu.RLock()
	allowHorn := v.hornWhenSeatboxOpen
	seatboxClosed := v.seatboxClosed
	v.mu.RUnlock()

	if allowHorn || seatboxClosed {
		return false
	}
	// getCurrentState takes its own lock; call it after releasing v.mu.
	return v.getCurrentState() != types.StateStandby
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
	shouldPublish := true

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
		}
	}

	// Feed gesture detector for synthesized input events
	switch channel {
	case "brake_right":
		v.gestures.OnChange("brake:right", value)
	case "brake_left":
		v.gestures.OnChange("brake:left", value)
	case "seatbox_button":
		v.gestures.OnChange("seatbox", value)
	case "horn_button":
		v.gestures.OnChange("horn", value)
	}

	// Hop-on family (locked or learning): suppress hardware side-effects
	// (horn, brake LED cue, blinker LED, seatbox open) while still
	// publishing input state to Redis so the dashboard's combo matcher
	// sees every press. FSM-level transitions are handled declaratively
	// by BlockedEvents on StateHopOn/StateHopOnLearning (see
	// fsm/definition.go) — sending the events here is harmless because
	// the FSM drops them.
	if v.isInHopOnFamily() {
		switch channel {
		case "brake_left":
			return v.redis.SetBrakeState("left", value)
		case "brake_right":
			return v.redis.SetBrakeState("right", value)
		case "horn_button":
			return v.redis.SetHornButton(value)
		case "seatbox_button":
			return v.redis.SetSeatboxButton(value)
		case "kickstand":
			return v.redis.SetKickstandState(value)
		case "blinker_left", "blinker_right":
			// handleBlinkerChange would normally publish the button
			// event; emit it manually here since we're skipping that
			// path to avoid driving the LED.
			position := strings.TrimPrefix(channel, "blinker_")
			evt := fmt.Sprintf("blinker:%s:%s", position, state)
			if err := v.redis.PublishButtonEvent(evt); err != nil {
				v.logger.Warnf("hop-on: failed to publish %s: %v", evt, err)
			}
			switchState := "off"
			if value {
				switchState = position
			}
			return v.redis.SetBlinkerSwitch(switchState)
		case "handlebar_lock_sensor":
			isLocked := !value
			v.mu.Lock()
			v.handlebarUnlocked = value
			v.mu.Unlock()
			return v.redis.SetHandlebarLockState(isLocked)
		case "48v_detect":
			return v.redis.SetMainPower(value)
		}
		return nil
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
		// Check if horn is allowed before activating. Also mute it when the
		// open seat lid can rest on the button (seatbox open + unlocked).
		allowed := v.isHornAllowed() && !v.seatboxBlocksHorn()
		hornValue := value && allowed // Only activate if both button pressed AND allowed

		if err := v.writeOutput("horn", hornValue); err != nil {
			return err
		}
		return v.redis.SetHornButton(value) // Still publish button state even if horn disabled

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

		// Cancel any pending deferred kickstand-down check
		v.mu.Lock()
		if v.kickstandDebounceTimer != nil {
			v.kickstandDebounceTimer.Stop()
			v.kickstandDebounceTimer = nil
		}
		v.mu.Unlock()

		// Send FSM event - FSM handles all transitions based on current state
		if value {
			// Debounce protection for kickstand down from ready-to-drive:
			// defer the event instead of dropping it, so quick flips are never lost
			v.mu.RLock()
			entryTime := v.readyToDriveEntryTime
			v.mu.RUnlock()

			remaining := parkDebounceTime - time.Since(entryTime)
			if currentState == types.StateReadyToDrive && remaining > 0 {
				v.logger.Infof("Kickstand down deferred by %v for debounce", remaining)
				v.mu.Lock()
				v.kickstandDebounceTimer = time.AfterFunc(remaining, func() {
					// Re-read the actual GPIO state — if kickstand was flipped
					// back up in the meantime we should not send EvKickstandDown
					kickstandDown, err := v.io.ReadDigitalInput("kickstand")
					if err != nil {
						v.logger.Warnf("Failed to read kickstand after debounce: %v", err)
						return
					}
					if kickstandDown {
						v.logger.Infof("Sending deferred EvKickstandDown")
						v.machine.Send(librefsm.Event{ID: fsm.EvKickstandDown})
					} else {
						v.logger.Infof("Deferred kickstand-down cancelled - kickstand is back up")
					}
				})
				v.mu.Unlock()
			} else {
				v.logger.Infof("Sending EvKickstandDown")
				v.machine.Send(librefsm.Event{ID: fsm.EvKickstandDown})
			}
		} else {
			v.logger.Infof("Sending EvKickstandUp")
			v.machine.Send(librefsm.Event{ID: fsm.EvKickstandUp})
		}

	case "seatbox_button":
		if value {
			// Reset auto-standby timer on seatbox press
			v.resetAutoStandbyTimer()
			// Send physical event - FSM handles seatbox opening via OnSeatboxButton action
			v.logger.Infof("Seatbox button pressed - sending EvSeatboxButton")
			v.machine.Send(librefsm.Event{ID: fsm.EvSeatboxButton})
		}

		// Update button state in Redis hash (happens after FSM event processing)
		// This is slow (~2s) but doesn't block physical response since FSM is async
		if err := v.redis.SetSeatboxButton(value); err != nil {
			return err
		}

	case "brake_right", "brake_left":
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

		eitherBrakePressed := brakeLeft || brakeRight

		if eitherBrakePressed {
			v.resetAutoStandbyTimer()
		}

		// Suppress brake-light LED cues while the scootui-qt menu is open:
		// it's navigated with the brake levers, and each tap would otherwise
		// flash the brake light on a parked scooter.
		if !v.menuOpen.Load() {
			if value {
				// A brake was just pressed — only play on-cue if the other wasn't already held
				otherHeld := brakeLeft
				if channel == "brake_left" {
					otherHeld = brakeRight
				}
				if !otherHeld {
					if err := v.io.PlayPwmCue(4); err != nil { // LED_BRAKE_OFF_TO_BRAKE_ON
						return err
					}
				}
			} else if !eitherBrakePressed {
				// Last brake released
				if err := v.io.PlayPwmCue(5); err != nil { // LED_BRAKE_ON_TO_BRAKE_OFF
					return err
				}
			}
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

		if err := v.writeOutput("engine_brake", engineBrakeEngaged); err != nil {
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

	case "handlebar_lock_sensor":
		// Invert the value: true (pressed) means unlocked, false (released) means locked.
		// Redis stores true for locked, false for unlocked.
		isLocked := !value
		if err := v.redis.SetHandlebarLockState(isLocked); err != nil {
			return err
		}
		// Track unlock state from sensor (source of truth for handlebarUnlocked flag)
		v.mu.Lock()
		v.handlebarUnlocked = value // sensor true = unlocked
		v.mu.Unlock()

	case "48v_detect":
		if err := v.redis.SetMainPower(value); err != nil {
			return fmt.Errorf("failed to set main-power in Redis: %w", err)
		}
	}
	return nil
}

// stopBlinker cancels any running blinker goroutine and waits for it to
// exit before returning. Waiting matters: runBlinker writes blinker:state
// and runs PlayPwmCue at startup, so any subsequent SetBlinkerState from
// the caller must happen after the old goroutine has flushed its writes —
// otherwise a late-scheduled goroutine can clobber the new state. Without
// the wait, rapid switch flicks (left→off→right) can leave Redis with a
// stale direction while the hardware blinks correctly.
func (v *VehicleSystem) stopBlinker() {
	v.mu.Lock()
	stopChan := v.blinkerStopChan
	exited := v.blinkerExited
	v.blinkerStopChan = nil
	v.blinkerExited = nil
	v.mu.Unlock()

	if stopChan != nil {
		close(stopChan)
	}
	if exited != nil {
		<-exited
	}
}

// getBlinkerState / setBlinkerState wrap the atomic field: blinker state is
// written from both the IO callback goroutine (handleBlinkerChange) and the
// Redis handler goroutine (handleBlinkerRequest).
func (v *VehicleSystem) getBlinkerState() BlinkerState {
	return BlinkerState(v.blinkerState.Load())
}

func (v *VehicleSystem) setBlinkerState(s BlinkerState) {
	v.blinkerState.Store(int32(s))
}

func (v *VehicleSystem) handleBlinkerChange(channel string, value bool) error {
	// Capture blinker state before stopping so the peer-read fallback below
	// uses the pre-stop value regardless of what stopBlinker does internally.
	prevBlinkerState := v.getBlinkerState()

	// Stop any existing blinker routine
	v.stopBlinker()

	// Determine the combined state of both blinker switches. io.go updates
	// activeKeys for each event before firing the callback, so by the time
	// event 2's callback runs, event 1's key is already reflected in the
	// cache. ReadDigitalInput (activeKeys) is therefore more reliable than
	// ReadDigitalInputDirect (EVIOCGKEY ioctl), which races against the
	// kernel's second GPIO interrupt handler.
	blinkerRight := false
	blinkerLeft := false
	switch channel {
	case "blinker_right":
		blinkerRight = value
		if leftVal, err := v.io.ReadDigitalInput("blinker_left"); err != nil {
			v.logger.Warnf("Failed to read blinker_left for hazard detection, falling back to last known state: %v", err)
			blinkerLeft = prevBlinkerState == BlinkerLeft || prevBlinkerState == BlinkerBoth
		} else {
			blinkerLeft = leftVal
		}
	case "blinker_left":
		blinkerLeft = value
		if rightVal, err := v.io.ReadDigitalInput("blinker_right"); err != nil {
			v.logger.Warnf("Failed to read blinker_right for hazard detection, falling back to last known state: %v", err)
			blinkerRight = prevBlinkerState == BlinkerRight || prevBlinkerState == BlinkerBoth
		} else {
			blinkerRight = rightVal
		}
	default:
		return fmt.Errorf("unexpected blinker channel: %q", channel)
	}

	// Build PUBSUB event string after channel validation so position is
	// guaranteed to be "left" or "right", never a TrimPrefix residue from an
	// unexpected channel value.
	position := strings.TrimPrefix(channel, "blinker_")
	onOff := "on"
	if !value {
		onOff = "off"
	}
	evt := fmt.Sprintf("blinker:%s:%s", position, onOff)

	var switchState string
	var cue int
	switch {
	case blinkerLeft && blinkerRight:
		switchState = "both"
		cue = 12 // LED_BLINK_BOTH
	case blinkerRight:
		switchState = "right"
		cue = 11 // LED_BLINK_RIGHT
	case blinkerLeft:
		switchState = "left"
		cue = 10 // LED_BLINK_LEFT
	default:
		switchState = "off"
		cue = 9 // LED_BLINK_NONE
	}

	if switchState == "off" {
		if err := v.io.PlayPwmCue(cue); err != nil {
			return err
		}
		if err := v.redis.SetBlinkerSwitch(switchState); err != nil {
			return err
		}
		// Also publish the button event directly via PUBSUB for immediate handling
		if err := v.redis.PublishButtonEvent(evt); err != nil {
			v.logger.Infof("Failed to publish blinker button event %s: %v", evt, err)
			// Continue with normal processing even if PUBSUB fails
		}
		if err := v.redis.SetBlinkerState(switchState, 0); err != nil {
			return err
		}
		// Only update in-memory state after all hardware and Redis writes
		// succeed so the fallback read in future events reflects reality.
		v.setBlinkerState(BlinkerOff)
		return nil
	}

	// Blinker active: write Redis switch state before publishing the PUBSUB
	// event so consumers that read Redis in response to the event always see
	// the updated switch state. Publishing channel+value (not switchState)
	// ensures consumers always receive the correct edge — e.g. blinker:right:off
	// when one side of a hazard pair is released, even though the combined
	// state transitions to "left" rather than "off".
	if err := v.redis.SetBlinkerSwitch(switchState); err != nil {
		return err
	}

	if err := v.redis.PublishButtonEvent(evt); err != nil {
		v.logger.Infof("Failed to publish blinker button event %s: %v", evt, err)
		// Continue with normal processing even if PUBSUB fails
	}

	// Only update in-memory blinker state after all Redis writes succeed so
	// the fallback read in future events reflects actual committed state.
	switch switchState {
	case "both":
		v.setBlinkerState(BlinkerBoth)
	case "right":
		v.setBlinkerState(BlinkerRight)
	case "left":
		v.setBlinkerState(BlinkerLeft)
	}

	// Start blinker routine. runBlinker captures blinker:start_nanos itself
	// immediately before the first PlayPwmCue ioctl, so the published anchor
	// reflects when the hardware fade actually begins rather than when we
	// queued the goroutine. It also publishes blinker:state from inside the
	// goroutine for the same reason.
	v.blinkerCueIndex.Store(int32(cue))
	stopChan := make(chan struct{})
	exited := make(chan struct{})
	v.mu.Lock()
	v.blinkerStopChan = stopChan
	v.blinkerExited = exited
	v.mu.Unlock()
	go v.runBlinker(cue, switchState, stopChan, exited)
	return nil
}

func (v *VehicleSystem) runBlinker(cue int, state string, stopChan, exited chan struct{}) {
	defer close(exited)

	// Bail before any side effects if cancellation already arrived. With
	// rapid switch flicks the cancel can fire before this goroutine first
	// gets CPU; without this gate we'd still write blinker:state and run
	// the initial PlayPwmCue, racing the next handler's writes.
	select {
	case <-stopChan:
		return
	default:
	}

	v.mu.RLock()
	dbcLed := v.dbcBlinkerLed
	v.mu.RUnlock()

	ledColor := "blinker_green"
	if state == "both" {
		ledColor = "blinker_red"
	}

	// Resolve the fade behind this cue once. Cues 10/11/12 (left/right/both)
	// all reference fade10 on at least one channel; we sample whichever fade
	// the cue's first fade-action points at. If anything's missing we fall
	// back to a square envelope (full-bright until cycle midpoint).
	var fade *led.Fade
	if v.ledCurves != nil {
		if c := v.ledCurves.GetCue(cue); c != nil {
			for _, action := range c.Actions {
				if action.ActionType == led.ActionTypeFade {
					fade = v.ledCurves.GetFade(action.FadeIndex)
					if fade != nil {
						break
					}
				}
			}
		}
	}

	setDbcBrightness := func(b uint8) {
		if !dbcLed {
			return
		}
		if err := v.io.SetDbcLed(ledColor, b); err != nil {
			v.logger.Warnf("Error setting DBC LED: %v", err)
		}
	}

	// Anchor the cycle schedule right before the first ioctl so
	// blinker:start_nanos matches when the hardware fade actually begins.
	startNanos := time.Now().UnixNano()
	v.blinkerStartNanos.Store(startNanos)
	if err := v.redis.SetBlinkerState(state, startNanos); err != nil {
		v.logger.Warnf("Failed to publish blinker state: %v", err)
	}

	// Fire the first cue immediately; subsequent cues are scheduled on the
	// absolute grid (startNanos + N*blinkerInterval) so phase doesn't drift
	// regardless of sample-loop jitter.
	if err := v.io.PlayPwmCue(cue); err != nil {
		v.logger.Warnf("Error playing blinker cue: %v", err)
	}
	nextCueAt := startNanos + int64(blinkerInterval)

	ticker := time.NewTicker(dbcLedSampleInterval)
	defer ticker.Stop()

	var lastBrightness uint8
	first := true
	for {
		// Non-blocking stop check before any side-effecting work this tick,
		// so a close that races with the ticker can't trigger one extra
		// PlayPwmCue after stopBlinker has returned.
		select {
		case <-stopChan:
			setDbcBrightness(0)
			return
		default:
		}

		now := time.Now().UnixNano()

		if now >= nextCueAt {
			if err := v.io.PlayPwmCue(cue); err != nil {
				v.logger.Warnf("Error playing blinker cue: %v", err)
			}
			nextCueAt += int64(blinkerInterval)
		}

		cyclePos := time.Duration((now - startNanos) % int64(blinkerInterval))
		var brightness uint8
		if fade != nil {
			// Square the normalised duty: the LP5562 is a lot brighter than
			// the front lamp, so a linear ramp looks pinned-on while the
			// lamp is still mid-fade. Gamma 2 keeps the indicator visibly
			// dark through the early rise / late fall.
			duty := fade.DutyAt(cyclePos)
			brightness = uint8(duty * duty * 255.0)
		} else if cyclePos < blinkerInterval/2 {
			brightness = 255
		}
		if first || brightness != lastBrightness {
			setDbcBrightness(brightness)
			lastBrightness = brightness
			first = false
		}

		select {
		case <-stopChan:
			setDbcBrightness(0)
			return
		case <-ticker.C:
		}
	}
}

func (v *VehicleSystem) keycardAuthPassed() error {
	v.logger.Infof("Processing keycard authentication tap")

	// --- Force Standby Check (3 taps + brake) ---
	// Works from any state, including ready-to-drive. A single tap or a
	// regular `lock` Redis command cannot shut down from drive; only this
	// triple-tap-with-brake gesture (or the explicit force-lock Redis
	// command) routes through EvForceLock.
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

// writeOutput writes a digital output and keeps the fault set in step with the
// result. The write error is returned unchanged, so callers behave exactly as
// they did before; only the fault reporting is new. A failure to report a fault
// is logged and swallowed, because it must never propagate into vehicle logic.
//
// Channels absent from outputFaultCodes pass straight through, which keeps
// "does this channel report faults" a property of the map rather than a
// per-call-site judgement.
//
// There is deliberately no in-process record of which codes are set. The three
// tracked channels are written at human-event rates against a local Redis, the
// fast channels are not in the map, and FaultReporter already collapses repeats
// server side. Set membership stays the only copy of the state; a second copy in
// process memory is the drift this reporting exists to avoid. Revisit only with
// a measurement.
func (v *VehicleSystem) writeOutput(channel string, value bool) error {
	err := v.io.WriteDigitalOutput(channel, value)

	code, tracked := outputFaultCodes[channel]
	if !tracked {
		return err
	}

	if err != nil {
		if reportErr := v.redis.RaiseFault(code, fmt.Sprintf("%s output write failed: %v", channel, err)); reportErr != nil {
			v.logger.Errorf("Failed to raise fault %d: %v", code, reportErr)
		}
		return err
	}

	if reportErr := v.redis.ClearFault(code); reportErr != nil {
		v.logger.Errorf("Failed to clear fault %d: %v", code, reportErr)
	}
	return nil
}

// clearOwnedFaults drops every fault this service owns. Called once at startup,
// right after the GPIO lines have been re-requested successfully: a process that
// just started has no evidence of a standing failure, and anything genuinely
// broken re-raises within one state transition. The alternative, carrying the
// set over from the previous run, keeps stale faults alive with no way to tell
// them from live ones.
func (v *VehicleSystem) clearOwnedFaults() {
	for _, code := range ownedFaultCodes {
		if err := v.redis.ClearFault(code); err != nil {
			v.logger.Warnf("Failed to clear fault %d at startup: %v", code, err)
		}
	}
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

	// Stop the UART PPP link BEFORE cutting DBC power, so pppd never holds
	// ttymxc2 open against a dead line (the unpowered DBC pulls RX into a
	// permanent break, triggering imx-uart RX flood soft resets).
	if component == "dashboard_power" && !enabled {
		if err := v.io.SetPppLinkEnabled(false); err != nil {
			v.logger.Warnf("Failed to stop ppp-link: %v", err)
			// Continue anyway - power change must not be blocked
		}
	}

	if err := v.writeOutput(component, enabled); err != nil {
		action := "enable"
		if !enabled {
			action = "disable"
		}
		return fmt.Errorf("failed to %s %s: %w", action, component, err)
	}

	// Persist dashboard_power state to Redis and mirror the usb0 link to DBC,
	// unless a persistent override is pinning usb0 to a specific state.
	if component == "dashboard_power" {
		if err := v.redis.SetDashboardPower(enabled); err != nil {
			v.logger.Warnf("Failed to persist dashboard power state to Redis: %v", err)
			// Don't return error - hardware state was set successfully
		}
		// Start the UART PPP link once the DBC is powered; pppd sits in
		// 'passive' until the DBC side answers with LCP.
		if enabled {
			if err := v.io.SetPppLinkEnabled(true); err != nil {
				v.logger.Warnf("Failed to start ppp-link: %v", err)
			}
		}
		if v.usb0AutoEffective() {
			if err := v.io.SetUsb0Enabled(enabled); err != nil {
				v.logger.Warnf("Failed to set usb0 link: %v", err)
				// Don't return error - dashboard power state was set successfully
			}
		} else {
			// Keep usb0 up regardless of dashboard_power: either the user
			// picked always-on, or auto is gated by insufficient keycard
			// pairings. Either way installer/diag tools need reachability.
			// Reassert so a transient bounce gets corrected on the next
			// state transition.
			if err := v.io.SetUsb0Enabled(true); err != nil {
				v.logger.Warnf("Failed to reassert usb0 always-on: %v", err)
			}
		}
	}

	// Persist engine_power commanded state to Redis so ecu-service can gate
	// its stale-frame detection on what vehicle-service actually commanded,
	// not on an inferred proxy like vehicle state.
	if component == "engine_power" {
		if err := v.redis.SetEnginePower(enabled); err != nil {
			v.logger.Warnf("Failed to persist engine power state to Redis: %v", err)
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
	if err := v.writeOutput(name, true); err != nil {
		return err
	}
	time.Sleep(duration)
	if err := v.writeOutput(name, false); err != nil {
		return err
	}
	return nil
}

// resyncHandlebarLatchFromSensor re-seeds the latched lock state from the raw
// sensor and publishes it. Called at boot and on Standby entry, where we have
// to trust the sensor as ground truth (no actuation history available).
func (v *VehicleSystem) resyncHandlebarLatchFromSensor() {
	sensorVal, err := v.io.ReadDigitalInputDirect("handlebar_lock_sensor")
	if err != nil {
		v.logger.Warnf("Handlebar latch resync: sensor read failed: %v", err)
		return
	}
	isLocked := !sensorVal
	v.mu.Lock()
	v.handlebarLatchedLocked = isLocked
	v.handlebarLatchInit = true
	v.mu.Unlock()
	if err := v.redis.SetHandlebarLockLatched(isLocked); err != nil {
		v.logger.Warnf("Handlebar latch publish failed: %v", err)
	}
}

// setHandlebarLatch updates the latched lock state from a confirmed actuation
// and publishes it.
func (v *VehicleSystem) setHandlebarLatch(isLocked bool) {
	v.mu.Lock()
	v.handlebarLatchedLocked = isLocked
	v.handlebarLatchInit = true
	v.mu.Unlock()
	if err := v.redis.SetHandlebarLockLatched(isLocked); err != nil {
		v.logger.Warnf("Handlebar latch publish failed: %v", err)
	}
}

// pulseHandlebarLock drives the handlebar lock solenoid H-bridge.
// Explicitly sets both sides: active side high, complementary side low,
// holds for the lock duration, then sets both low.
func (v *VehicleSystem) pulseHandlebarLock(lock bool) error {
	closeVal := lock
	openVal := !lock
	if err := v.writeOutput("handlebar_lock_close", closeVal); err != nil {
		return err
	}
	if err := v.writeOutput("handlebar_lock_open", openVal); err != nil {
		// Best-effort: deactivate the first output before returning. If this
		// also fails the solenoid coil can stay energized continuously
		// instead of being pulsed, so log it even though the original error
		// is still what gets returned.
		if rbErr := v.writeOutput("handlebar_lock_close", false); rbErr != nil {
			v.logger.Errorf("Failed to deactivate handlebar_lock_close after open write failure: %v", rbErr)
		}
		return err
	}
	time.Sleep(handlebarLockDuration)
	// Both outputs low (coast)
	err1 := v.writeOutput("handlebar_lock_close", false)
	err2 := v.writeOutput("handlebar_lock_open", false)
	if err1 != nil {
		return err1
	}
	return err2
}

// playLedCue plays an LED cue and logs any errors with context
func (v *VehicleSystem) playLedCue(cue int, description string) {
	if err := v.io.PlayPwmCue(cue); err != nil {
		v.logger.Infof("Failed to play LED cue %d (%s): %v", cue, description, err)
	}
}

func (v *VehicleSystem) openSeatboxLock() {
	go func() {
		if err := v.pulseOutput("seatbox_lock", seatboxLockDuration); err != nil {
			v.logger.Errorf("Failed to toggle seatbox lock: %v", err)
			return
		}
		v.logger.Infof("Seatbox lock toggled for 0.2s")
	}()
}

func (v *VehicleSystem) publishState() error {
	state := v.getCurrentState()

	v.logger.Debugf("Publishing state to Redis: %s", state)
	if err := v.redis.PublishVehicleState(state); err != nil {
		return err
	}
	return nil
}

// resetDbcWatchdog resets the DBC update watchdog timer on OTA activity.
// Called from the ota pub/sub watcher whenever any :dbc field changes.
func (v *VehicleSystem) resetDbcWatchdog() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dbcWatchdogTimer != nil {
		v.dbcWatchdogTimer.Reset(dbcUpdateWatchdogTimeout)
	}
	return nil
}

// handlePowerStateChange reacts to pm-service publishing power-manager.state.
// On the transition to "running" (resume from suspend), re-read GPIO inputs
// via EVIOCGKEY so any edges the kernel masked during sleep surface as
// synthetic events. Without this, inputs like 48v_detect can stay stuck at
// the pre-suspend value even after the rail comes back up.
func (v *VehicleSystem) handlePowerStateChange(state string) error {
	if state != "running" {
		return nil
	}
	v.logger.Infof("Power state -> running, resyncing GPIO inputs")
	if err := v.io.ResyncInputs(); err != nil {
		v.logger.Warnf("Failed to resync inputs on resume: %v", err)
		return err
	}
	return nil
}

// startDbcWatchdog starts (or restarts) the DBC update watchdog timer.
// Must be called with v.mu held.
func (v *VehicleSystem) startDbcWatchdog() {
	if v.dbcWatchdogTimer != nil {
		v.dbcWatchdogTimer.Stop()
	}
	v.dbcWatchdogGeneration++
	gen := v.dbcWatchdogGeneration
	v.dbcWatchdogTimer = time.AfterFunc(dbcUpdateWatchdogTimeout, func() {
		v.mu.RLock()
		currentGen := v.dbcWatchdogGeneration
		v.mu.RUnlock()
		if currentGen != gen {
			return
		}
		v.handleDbcWatchdogTimeout()
	})
}

// stopDbcWatchdog stops the DBC update watchdog timer.
// Must be called with v.mu held.
func (v *VehicleSystem) stopDbcWatchdog() {
	if v.dbcWatchdogTimer != nil {
		v.dbcWatchdogTimer.Stop()
		v.dbcWatchdogTimer = nil
	}
}

// usb0AutoEffective reports whether the usb0=auto policy should actually
// take effect right now. Even when the user has opted in to auto, we keep
// usb0 up until enough keycards are paired to recover from a lockout —
// otherwise a bad pairing could leave the scooter unreachable to both the
// owner (no working card) and the installer (no usb0).
//
// Threshold: (master >= 1 AND authorized >= 1) OR authorized >= 2.
// Counts are published to the "system" hash by keycard-service.
func (v *VehicleSystem) usb0AutoEffective() bool {
	v.mu.RLock()
	policy := v.usb0Policy
	v.mu.RUnlock()
	if policy != "auto" {
		return false
	}

	master := v.readKeycardCount("keycard-master-count")
	authorized := v.readKeycardCount("keycard-authorized-count")

	if master >= 1 && authorized >= 1 {
		return true
	}
	if authorized >= 2 {
		return true
	}
	return false
}

func (v *VehicleSystem) readKeycardCount(field string) int {
	raw, err := v.redis.GetHashField("system", field)
	if err != nil || raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		v.logger.Warnf("Bad %s in system hash: %q (%v)", field, raw, err)
		return 0
	}
	return n
}

func (v *VehicleSystem) Shutdown() {
	// Stop blinker if running
	v.stopBlinker()

	// Stop any running handlebar lock/unlock goroutine
	v.cancelHandlebarLock()
	v.cancelHandlebarUnlock()

	v.mu.Lock()
	v.stopDbcWatchdog()
	v.mu.Unlock()

	if v.redis != nil {
		_ = v.redis.Close()
	}
	if v.io != nil {
		v.io.Cleanup()
	}
}
