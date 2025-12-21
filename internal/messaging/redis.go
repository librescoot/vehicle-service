package messaging

import (
	"context"
	"fmt"
	"sync"
	"time"

	"vehicle-service/internal/logger"
	"vehicle-service/internal/types"

	ipc "github.com/librescoot/redis-ipc"
)

type Callbacks struct {
	DashboardCallback func(bool) error
	KeycardCallback   func() error
	SeatboxCallback   func(bool) error   // true for "on", false for "off"
	HornCallback      func(bool) error   // true for "on", false for "off"
	BlinkerCallback   func(string) error // "off", "left", "right", "both"
	StateCallback     func(string) error // "unlock", "lock", "lock-hibernate"
	ForceLockCallback func() error       // New callback for force-lock
	LedCueCallback    func(int) error
	LedFadeCallback   func(int, int) error
	UpdateCallback    func(string) error // "start", "complete"
	HardwareCallback  func(string) error // "dashboard:on", "dashboard:off", "engine:on", "engine:off", "handlebar:lock", "handlebar:unlock"
	SettingsCallback  func(string) error // setting key that was updated (e.g., "scooter.brake-hibernation")
}

type RedisClient struct {
	client    *ipc.Client
	callbacks Callbacks
	logger    *logger.Logger
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup

	// Publishers
	vehiclePub   *ipc.HashPublisher
	systemPub    *ipc.HashPublisher
	otaPub       *ipc.HashPublisher
	dashboardPub *ipc.HashPublisher
	buttonsPub   *ipc.HashPublisher

	// Watchers
	vehicleWatcher   *ipc.HashWatcher
	dashboardWatcher *ipc.HashWatcher
	keycardWatcher   *ipc.HashWatcher
	settingsWatcher  *ipc.HashWatcher

	// Fault handling
	faultSet    *ipc.FaultSet
	faultStream *ipc.StreamPublisher
}

func NewRedisClient(host string, port int, l *logger.Logger, callbacks Callbacks) (*RedisClient, error) {
	ctx, cancel := context.WithCancel(context.Background())

	client, err := ipc.New(
		ipc.WithAddress(host),
		ipc.WithPort(port),
		ipc.WithCodec(ipc.StringCodec{}),
		ipc.WithOnConnect(func() { l.Infof("Redis connected") }),
		ipc.WithOnDisconnect(func(err error) { l.Warnf("Redis disconnected: %v", err) }),
	)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create Redis client: %w", err)
	}

	r := &RedisClient{
		client:    client,
		callbacks: callbacks,
		logger:    l,
		ctx:       ctx,
		cancel:    cancel,
	}

	// Initialize publishers
	r.vehiclePub = client.NewHashPublisher("vehicle")
	r.systemPub = client.NewHashPublisher("system")
	r.otaPub = client.NewHashPublisher("ota")
	r.dashboardPub = client.NewHashPublisher("dashboard")
	r.buttonsPub = client.NewHashPublisher("buttons")

	// Initialize fault handling
	r.faultSet = client.NewFaultSet("vehicle:fault", "vehicle", "fault")
	r.faultStream = client.NewStreamPublisher("events:faults", ipc.WithMaxLen(1000))

	return r, nil
}

func (r *RedisClient) SetCallbacks(callbacks Callbacks) {
	r.callbacks = callbacks
}

func (r *RedisClient) Connect() error {
	r.logger.Infof("Attempting to connect to Redis")

	// Check initial dashboard state
	ready, err := r.client.HGet("dashboard", "ready")
	if err != nil {
		r.logger.Infof("Failed to get initial dashboard state: %v", err)
	} else if ready != "" {
		r.logger.Infof("Initial dashboard ready state: %v", ready == "true")
		if ready == "true" {
			if err := r.callbacks.DashboardCallback(true); err != nil {
				r.logger.Infof("Failed to handle initial dashboard state: %v", err)
			}
		}
	}

	return nil
}

// StartListening starts all Redis listeners after system initialization is complete
func (r *RedisClient) StartListening() error {
	r.logger.Infof("Starting Redis listeners")

	// Initialize hash watchers
	r.dashboardWatcher = r.client.NewHashWatcher("dashboard")
	r.dashboardWatcher.OnField("ready", func(value string) error {
		r.logger.Infof("Processing dashboard ready state: %v", value == "true")
		return r.callbacks.DashboardCallback(value == "true")
	})
	r.dashboardWatcher.Start()

	r.keycardWatcher = r.client.NewHashWatcher("keycard")
	r.keycardWatcher.OnField("authentication", func(value string) error {
		r.logger.Infof("Processing keycard authentication")
		return r.callbacks.KeycardCallback()
	})
	r.keycardWatcher.Start()

	r.settingsWatcher = r.client.NewHashWatcher("settings")
	r.settingsWatcher.OnAny(func(field, value string) error {
		if r.callbacks.SettingsCallback != nil {
			r.logger.Infof("Processing settings update: %s", field)
			return r.callbacks.SettingsCallback(field)
		}
		return nil
	})
	r.settingsWatcher.Start()

	// Start queue command listeners
	ipc.HandleRequests(r.client, "scooter:seatbox", r.handleSeatboxCommand)
	ipc.HandleRequests(r.client, "scooter:horn", r.handleHornCommand)
	ipc.HandleRequests(r.client, "scooter:blinker", r.handleBlinkerCommand)
	ipc.HandleRequests(r.client, "scooter:state", r.handleStateCommand)
	ipc.HandleRequests(r.client, "scooter:led:cue", r.handleLedCueCommand)
	ipc.HandleRequests(r.client, "scooter:led:fade", r.handleLedFadeCommand)
	ipc.HandleRequests(r.client, "scooter:update", r.handleUpdateCommand)
	ipc.HandleRequests(r.client, "scooter:hardware", r.handleHardwareCommand)

	r.logger.Infof("Successfully started all Redis listeners")
	return nil
}

func (r *RedisClient) handleSeatboxCommand(value string) error {
	if r.callbacks.SeatboxCallback == nil {
		return nil
	}
	switch value {
	case "open":
		return r.callbacks.SeatboxCallback(value == "open")
	default:
		r.logger.Infof("Invalid seatbox command value: %s", value)
		return fmt.Errorf("invalid seatbox command: %s", value)
	}
}

func (r *RedisClient) handleHornCommand(value string) error {
	if r.callbacks.HornCallback == nil {
		return nil
	}
	switch value {
	case "on", "off":
		return r.callbacks.HornCallback(value == "on")
	default:
		r.logger.Infof("Invalid horn command value: %s", value)
		return fmt.Errorf("invalid horn command: %s", value)
	}
}

func (r *RedisClient) handleBlinkerCommand(value string) error {
	if r.callbacks.BlinkerCallback == nil {
		return nil
	}
	switch value {
	case "off", "left", "right", "both":
		return r.callbacks.BlinkerCallback(value)
	default:
		r.logger.Infof("Invalid blinker command value: %s", value)
		return fmt.Errorf("invalid blinker command: %s", value)
	}
}

func (r *RedisClient) handleStateCommand(value string) error {
	if r.callbacks.StateCallback == nil && r.callbacks.ForceLockCallback == nil {
		return nil
	}
	switch value {
	case "unlock", "lock", "lock-hibernate":
		if r.callbacks.StateCallback != nil {
			return r.callbacks.StateCallback(value)
		}
	case "force-lock":
		if r.callbacks.ForceLockCallback != nil {
			return r.callbacks.ForceLockCallback()
		}
	default:
		r.logger.Infof("Invalid state command value: %s", value)
		return fmt.Errorf("invalid state command: %s", value)
	}
	return nil // Should not be reached if callbacks are properly assigned or error is returned
}

func (r *RedisClient) handleLedCueCommand(value string) error {
	if r.callbacks.LedCueCallback == nil {
		return nil
	}
	var cueIndex int
	_, err := fmt.Sscanf(value, "%d", &cueIndex)
	if err != nil {
		r.logger.Infof("Invalid LED cue command value: %s, expected integer: %v", value, err)
		return fmt.Errorf("invalid LED cue command: %s", value)
	}
	return r.callbacks.LedCueCallback(cueIndex)
}

func (r *RedisClient) handleLedFadeCommand(value string) error {
	if r.callbacks.LedFadeCallback == nil {
		return nil
	}
	var ledChannel, fadeIndex int
	_, err := fmt.Sscanf(value, "%d:%d", &ledChannel, &fadeIndex)
	if err != nil {
		r.logger.Infof("Invalid LED fade command value: %s, expected 'channel:index': %v", value, err)
		return fmt.Errorf("invalid LED fade command: %s", value)
	}
	return r.callbacks.LedFadeCallback(ledChannel, fadeIndex)
}

func (r *RedisClient) handleUpdateCommand(value string) error {
	if r.callbacks.UpdateCallback == nil {
		return nil
	}

	switch value {
	case "start", "complete", "start-dbc", "complete-dbc":
		return r.callbacks.UpdateCallback(value)
	default:
		r.logger.Infof("Invalid update command value: %s", value)
		return fmt.Errorf("invalid update command: %s", value)
	}
}

// handleHardwareCommand processes hardware power control commands
func (r *RedisClient) handleHardwareCommand(value string) error {
	if r.callbacks.HardwareCallback == nil {
		return nil
	}

	r.logger.Infof("Processing hardware command: %s", value)

	return r.callbacks.HardwareCallback(value)
}


func (r *RedisClient) PublishVehicleState(state types.SystemState) error {
	r.logger.Infof("Publishing vehicle state: %s", state)
	stateStr := string(state)

	// Set both state and timestamp fields using SetWithTimestamp
	if err := r.vehiclePub.SetWithTimestamp("state", stateStr); err != nil {
		r.logger.Warnf("Failed to publish vehicle state: %v", err)
		return err
	}
	r.logger.Debugf("Successfully published vehicle state with timestamp")
	return nil
}

func (r *RedisClient) SetBlinkerSwitch(state string) error {
	r.logger.Debugf("Setting blinker switch: %s", state)

	var blinkerStr string
	switch state {
	case "off":
		blinkerStr = "off"
	case "left":
		blinkerStr = "left"
	case "right":
		blinkerStr = "right"
	case "both":
		blinkerStr = "both"
	default:
		blinkerStr = "unknown"
	}

	if err := r.vehiclePub.Set("blinker:switch", blinkerStr); err != nil {
		r.logger.Warnf("Failed to set blinker switch: %v", err)
		return err
	}
	r.logger.Debugf("Successfully set blinker switch")
	return nil
}

func (r *RedisClient) SetBlinkerState(state string) error {
	r.logger.Debugf("Setting blinker state: %s", state)

	var blinkerStr string
	switch state {
	case "off":
		blinkerStr = "off"
	case "left":
		blinkerStr = "left"
	case "right":
		blinkerStr = "right"
	case "both":
		blinkerStr = "both"
	default:
		blinkerStr = "unknown"
	}

	if err := r.vehiclePub.Set("blinker:state", blinkerStr); err != nil {
		r.logger.Warnf("Failed to set blinker state: %v", err)
		return err
	}
	r.logger.Debugf("Successfully set blinker state")
	return nil
}

func (r *RedisClient) GetVehicleState() (types.SystemState, error) {
	r.logger.Infof("Getting vehicle state from Redis")
	stateStr, err := r.client.HGet("vehicle", "state")
	if err != nil {
		r.logger.Infof("Failed to get vehicle state: %v", err)
		return types.StateInit, err
	}
	if stateStr == "" {
		r.logger.Infof("No vehicle state found in Redis")
		return types.StateInit, nil
	}
	r.logger.Infof("Successfully retrieved vehicle state: %s", stateStr)
	return types.SystemState(stateStr), nil
}

func (r *RedisClient) SetBrakeState(side string, isPressed bool) error {
	r.logger.Debugf("Setting brake state: %s=%v", side, isPressed)
	state := "off"
	if isPressed {
		state = "on"
	}

	field := fmt.Sprintf("brake:%s", side)
	if err := r.vehiclePub.Set(field, state); err != nil {
		r.logger.Warnf("Failed to set brake state: %v", err)
		return err
	}
	r.logger.Debugf("Successfully set brake state")
	return nil
}

func (r *RedisClient) SetHornButton(isPressed bool) error {
	r.logger.Debugf("Setting horn button state: %v", isPressed)
	state := "off"
	if isPressed {
		state = "on"
	}

	// Note: Using vehiclePub here but it publishes to "buttons" channel for these button events
	// We need a separate publisher for the buttons hash
	if err := r.buttonsPub.Set(fmt.Sprintf("horn:%s", state), "1"); err != nil {
		r.logger.Warnf("Failed to set horn button state: %v", err)
		return err
	}
	r.logger.Debugf("Successfully set horn button state")
	return nil
}

func (r *RedisClient) SetSeatboxButton(isPressed bool) error {
	r.logger.Debugf("Setting seatbox button state: %v", isPressed)
	state := "off"
	if isPressed {
		state = "on"
	}

	// Note: Using buttonsPub here for button event publishing
	if err := r.buttonsPub.Set(fmt.Sprintf("seatbox:%s", state), "1"); err != nil {
		r.logger.Warnf("Failed to set seatbox button state: %v", err)
		return err
	}
	r.logger.Debugf("Successfully set seatbox button state")
	return nil
}

func (r *RedisClient) SetSeatboxLockState(isLocked bool) error {
	r.logger.Debugf("Setting seatbox lock state: %v", isLocked)
	state := "open"
	if isLocked {
		state = "closed"
	}

	if err := r.vehiclePub.Set("seatbox:lock", state); err != nil {
		r.logger.Warnf("Failed to set seatbox lock state: %v", err)
		return err
	}
	r.logger.Debugf("Successfully set seatbox lock state")
	return nil
}

func (r *RedisClient) PublishSeatboxOpened() error {
	r.logger.Infof("Publishing seatbox opened event")
	_, err := r.client.Publish("vehicle", "seatbox:opened")
	if err != nil {
		r.logger.Infof("Failed to publish seatbox opened: %v", err)
		return err
	}
	return nil
}

func (r *RedisClient) SetKickstandState(isDown bool) error {
	r.logger.Debugf("Setting kickstand state: %v", isDown)
	state := "up"
	if isDown {
		state = "down"
	}

	if err := r.vehiclePub.Set("kickstand", state); err != nil {
		r.logger.Warnf("Failed to set kickstand state: %v", err)
		return err
	}
	r.logger.Debugf("Successfully set kickstand state")
	return nil
}

func (r *RedisClient) SetHandlebarPosition(isOnPlace bool) error {
	r.logger.Debugf("Setting handlebar position: %v", isOnPlace)
	state := "off-place"
	if isOnPlace {
		state = "on-place"
	}

	err := r.client.HSet("vehicle", "handlebar:position", state)
	if err != nil {
		r.logger.Warnf("Failed to set handlebar position: %v", err)
		return err
	}
	r.logger.Debugf("Successfully set handlebar position")
	return nil
}

func (r *RedisClient) SetHandlebarLockState(isLocked bool) error {
	r.logger.Debugf("Setting handlebar lock state: %v", isLocked)
	state := "unlocked"
	if isLocked {
		state = "locked"
	}

	if err := r.vehiclePub.Set("handlebar:lock-sensor", state); err != nil {
		r.logger.Warnf("Failed to set handlebar lock state: %v", err)
		return err
	}
	r.logger.Debugf("Successfully set handlebar lock state")
	return nil
}

// SetDbcUpdating sets the DBC updating state in Redis
func (r *RedisClient) SetDbcUpdating(updating bool) error {
	r.logger.Debugf("Setting DBC updating state: %v", updating)
	value := "false"
	if updating {
		value = "true"
	}

	if err := r.vehiclePub.Set("dbc-updating", value); err != nil {
		r.logger.Warnf("Failed to set DBC updating state: %v", err)
		return err
	}
	r.logger.Debugf("Successfully set DBC updating state")
	return nil
}

// GetDbcUpdating gets the DBC updating state from Redis
func (r *RedisClient) GetDbcUpdating() (bool, error) {
	value, err := r.client.HGet("vehicle", "dbc-updating")
	if err != nil {
		return false, err
	}
	if value == "" {
		return false, nil // Field doesn't exist, default to false
	}
	return value == "true", nil
}

// SetDashboardPower sets the dashboard power state in Redis
func (r *RedisClient) SetDashboardPower(enabled bool) error {
	r.logger.Debugf("Setting dashboard power state: %v", enabled)
	value := "off"
	if enabled {
		value = "on"
	}

	if err := r.vehiclePub.Set("dashboard:power", value); err != nil {
		r.logger.Warnf("Failed to set dashboard power state: %v", err)
		return err
	}
	r.logger.Debugf("Successfully set dashboard power state")
	return nil
}

// GetDashboardPower gets the dashboard power state from Redis
func (r *RedisClient) GetDashboardPower() (bool, error) {
	value, err := r.client.HGet("vehicle", "dashboard:power")
	if err != nil {
		return false, err
	}
	if value == "" {
		return false, nil // Field doesn't exist, default to false/off
	}
	return value == "on", nil
}

// PublishUpdateStatus publishes the update status to Redis
func (r *RedisClient) PublishUpdateStatus(status string) error {
	r.logger.Debugf("Publishing update status: %s", status)

	if err := r.vehiclePub.Set("update:status", status); err != nil {
		r.logger.Warnf("Failed to publish update status: %v", err)
		return err
	}
	r.logger.Debugf("Successfully published update status")
	return nil
}

// PublishButtonEvent publishes a button event to the "buttons" channel
func (r *RedisClient) PublishButtonEvent(event string) error {
	r.logger.Debugf("Publishing button event: %s", event)
	_, err := r.client.Publish("buttons", event)
	if err != nil {
		r.logger.Warnf("Failed to publish button event: %v", err)
		return err
	}
	r.logger.Debugf("Successfully published button event")
	return nil
}

// PublishAutoStandbyCountdown publishes the auto-standby countdown to Redis
// Deprecated: Use PublishAutoStandbyDeadline instead
func (r *RedisClient) PublishAutoStandbyCountdown(remaining int) error {
	return r.vehiclePub.Set("auto-standby-remaining", remaining)
}

// ClearAutoStandbyCountdown removes the auto-standby countdown from Redis
// Deprecated: Use ClearAutoStandbyDeadline instead
func (r *RedisClient) ClearAutoStandbyCountdown() error {
	return r.vehiclePub.Delete("auto-standby-remaining")
}

// PublishAutoStandbyDeadline publishes when auto-standby will trigger as Unix timestamp
func (r *RedisClient) PublishAutoStandbyDeadline(deadline time.Time) error {
	unixTs := deadline.Unix()
	return r.vehiclePub.Set("auto-standby-deadline", unixTs)
}

// ClearAutoStandbyDeadline removes the auto-standby deadline from Redis
func (r *RedisClient) ClearAutoStandbyDeadline() error {
	return r.vehiclePub.Delete("auto-standby-deadline")
}

// PublishGovernorChange publishes a governor change event to Redis
func (r *RedisClient) PublishGovernorChange(governor string) error {
	r.logger.Debugf("Publishing governor change: %s", governor)

	if err := r.systemPub.Set("cpu:governor", governor); err != nil {
		r.logger.Warnf("Failed to publish governor change: %v", err)
		return err
	}
	r.logger.Debugf("Successfully published governor change")
	return nil
}

// DeleteDashboardReadyFlag deletes the dashboard ready flag from Redis and publishes the change
func (r *RedisClient) DeleteDashboardReadyFlag() error {
	r.logger.Infof("Deleting dashboard ready flag from Redis")

	if err := r.dashboardPub.Delete("ready"); err != nil {
		r.logger.Infof("Failed to delete dashboard ready flag: %v", err)
		return err
	}
	r.logger.Infof("Successfully deleted dashboard ready flag and published change")
	return nil
}

// PublishStandbyTimerStart sets the standby timer start timestamp for MDB reboot coordination
func (r *RedisClient) PublishStandbyTimerStart() error {
	r.logger.Infof("Setting standby timer start timestamp")
	timestamp := fmt.Sprintf("%d", time.Now().Unix())

	if err := r.otaPub.Set("standby-timer-start", timestamp); err != nil {
		r.logger.Infof("Failed to set standby timer start: %v", err)
		return err
	}
	r.logger.Infof("Successfully set standby timer start: %s", timestamp)
	return nil
}

// GetOtaStatus gets the OTA status for a specific component from Redis
func (r *RedisClient) GetOtaStatus(component string) (string, error) {
	statusKey := fmt.Sprintf("status:%s", component)
	status, err := r.client.HGet("ota", statusKey)
	if err != nil {
		return "", fmt.Errorf("failed to get OTA status for component %s: %w", component, err)
	}
	if status == "" {
		// Status field doesn't exist, return empty string (idle)
		return "", nil
	}
	return status, nil
}

// SendCommand sends a command to a Redis list (for communication with other services)
func (r *RedisClient) SendCommand(channel, command string) error {
	_, err := r.client.LPush(channel, command)
	if err != nil {
		r.logger.Infof("Failed to send command '%s' to channel '%s': %v", command, channel, err)
		return err
	}
	r.logger.Infof("Sent command '%s' to channel '%s'", command, channel)
	return nil
}

// ReportFaultPresent reports a fault as present to Redis
func (r *RedisClient) ReportFaultPresent(code int, description string, timestamp int64, info string) error {
	r.logger.Infof("Reporting fault present: code=%d, description=%s", code, description)

	// Add fault code to active faults set
	if err := r.faultSet.Add(code); err != nil {
		r.logger.Infof("Failed to add fault to set: %v", err)
		return err
	}

	// Add fault event to global event stream with metadata
	eventData := map[string]any{
		"group":       "vehicle",
		"code":        fmt.Sprint(code),
		"description": description,
		"ts":          fmt.Sprint(timestamp),
	}
	if info != "" {
		eventData["info"] = info
	}
	if _, err := r.faultStream.Add(eventData); err != nil {
		r.logger.Infof("Failed to add fault to stream: %v", err)
		return err
	}

	r.logger.Infof("Successfully reported fault %d as present", code)
	return nil
}

// ReportFaultAbsent reports a fault as absent (cleared) to Redis
func (r *RedisClient) ReportFaultAbsent(code int) error {
	r.logger.Infof("Reporting fault absent: code=%d", code)

	// Remove fault code from active faults set
	if err := r.faultSet.Remove(code); err != nil {
		r.logger.Infof("Failed to remove fault from set: %v", err)
		return err
	}

	// Add clear event to global event stream (negative code indicates cleared)
	eventData := map[string]any{
		"group": "vehicle",
		"code":  fmt.Sprint(-code), // Negative code indicates fault cleared
	}
	if _, err := r.faultStream.Add(eventData); err != nil {
		r.logger.Infof("Failed to add fault clear to stream: %v", err)
		return err
	}

	r.logger.Infof("Successfully reported fault %d as absent", code)
	return nil
}

// GetHashField reads a field from a Redis hash using HGET
func (r *RedisClient) GetHashField(hash, field string) (string, error) {
	value, err := r.client.HGet(hash, field)
	if err != nil {
		return "", fmt.Errorf("failed to get hash field %s from %s: %w", field, hash, err)
	}
	if value == "" {
		// Field doesn't exist, return empty string
		return "", nil
	}
	return value, nil
}

func (r *RedisClient) Close() error {
	r.logger.Infof("Closing Redis client")
	r.cancel()

	// Wait for all goroutines to finish with a timeout
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		r.logger.Infof("All Redis goroutines finished")
	case <-time.After(5 * time.Second):
		r.logger.Infof("Timeout waiting for Redis goroutines to finish")
	}

	return r.client.Close()
}
