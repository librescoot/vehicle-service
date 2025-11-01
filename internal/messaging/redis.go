package messaging

import (
	"context"
	"fmt"
	"sync"
	"time"

	"vehicle-service/internal/logger"
	"vehicle-service/internal/types"

	"github.com/redis/go-redis/v9"
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
}

type RedisClient struct {
	client    *redis.Client
	callbacks Callbacks
	logger    *logger.Logger
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

func NewRedisClient(host string, port int, l *logger.Logger, callbacks Callbacks) *RedisClient {
	ctx, cancel := context.WithCancel(context.Background())
	return &RedisClient{
		client: redis.NewClient(&redis.Options{
			Addr: fmt.Sprintf("%s:%d", host, port),
			DB:   0,
		}),
		callbacks: callbacks,
		logger:    l,
		ctx:       ctx,
		cancel:    cancel,
	}
}

func (r *RedisClient) Connect() error {
	r.logger.Infof("Attempting to connect to Redis at %s", r.client.Options().Addr)

	if err := r.client.Ping(r.ctx).Err(); err != nil {
		r.logger.Infof("Redis connection failed: %v", err)
		return fmt.Errorf("Redis connection failed: %w", err)
	}
	r.logger.Infof("Successfully connected to Redis")

	ready, err := r.client.HGet(r.ctx, "dashboard", "ready").Result()
	if err != nil && err != redis.Nil {
		r.logger.Infof("Failed to get initial dashboard state: %v", err)
	} else {
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

	// Subscribe to pub/sub channels for system events
	pubsub := r.client.Subscribe(r.ctx, "dashboard", "keycard", "ota", "power-manager", "vehicle")
	r.logger.Infof("Subscribed to Redis channels: dashboard, keycard, ota, power-manager, vehicle")

	// Start pub/sub listener
	r.wg.Add(1)
	go r.redisListener(pubsub)

	// Start list command listeners for LPUSH commands
	r.wg.Add(8)
	go r.listCommandListener("scooter:seatbox", r.handleSeatboxCommand)
	go r.listCommandListener("scooter:horn", r.handleHornCommand)
	go r.listCommandListener("scooter:blinker", r.handleBlinkerCommand)
	go r.listCommandListener("scooter:state", r.handleStateCommand)
	go r.listCommandListener("scooter:led:cue", r.handleLedCueCommand)
	go r.listCommandListener("scooter:led:fade", r.handleLedFadeCommand)
	go r.listCommandListener("scooter:update", r.handleUpdateCommand)
	go r.listCommandListener("scooter:hardware", r.handleHardwareCommand)

	return nil
}

func (r *RedisClient) listCommandListener(key string, handler func(string) error) {
	defer r.wg.Done()
	r.logger.Infof("Starting list command listener for %s", key)

	for {
		select {
		case <-r.ctx.Done():
			r.logger.Infof("Context cancelled, exiting %s listener", key)
			return
		default:
			// Use BRPOP with a short timeout to allow periodic context cancellation checks
			result, err := r.client.BRPop(r.ctx, 5*time.Second, key).Result()
			if err != nil {
				if err == redis.Nil {
					// Timeout elapsed, loop back to check context
					continue
				}
				if err == context.Canceled {
					r.logger.Infof("Context cancelled, exiting %s listener", key)
					return
				}
				r.logger.Infof("Error reading from %s list: %v", key, err)
				continue
			}

			select {
			case <-r.ctx.Done():
				r.logger.Infof("Context cancelled, exiting %s listener", key)
				return
			default:
				if len(result) >= 2 { // BRPOP returns [key, value]
					value := result[1]
					r.logger.Infof("Received command from %s: %s", key, value)
					if err := handler(value); err != nil {
						r.logger.Infof("Error handling %s command: %v", key, err)
					}
				}
			}
		}
	}
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

	// Validate command format (component:action)
	switch value {
	case "dashboard:on", "dashboard:off", "engine:on", "engine:off", "handlebar:lock", "handlebar:unlock":
		return r.callbacks.HardwareCallback(value)
	default:
		r.logger.Infof("Invalid hardware command value: %s", value)
		return fmt.Errorf("invalid hardware command: %s", value)
	}
}


func (r *RedisClient) redisListener(pubsub *redis.PubSub) {
	defer r.wg.Done()
	defer pubsub.Close()

	r.logger.Infof("Starting Redis message listener")
	channel := pubsub.Channel()

	for {
		select {
		case <-r.ctx.Done():
			r.logger.Infof("Context cancelled, exiting listener")
			return
		case msg, ok := <-channel:
			if !ok {
				r.logger.Infof("Redis channel closed unexpectedly")
				r.logger.Fatalf("Redis connection lost, exiting to allow systemd restart")
			}
			if msg == nil {
				r.logger.Infof("Received nil Redis message")
				r.logger.Fatalf("Redis connection lost, exiting to allow systemd restart")
			}

			r.logger.Infof("Received Redis message: channel=%s payload=%s", msg.Channel, msg.Payload)

			switch msg.Channel {
			case "dashboard":
				if msg.Payload == "ready" {
					ready, err := r.client.HGet(r.ctx, "dashboard", "ready").Result()
					if err != nil {
						r.logger.Infof("Failed to get dashboard state: %v", err)
					} else {
						r.logger.Infof("Processing dashboard ready state: %v", ready == "true")
						if err := r.callbacks.DashboardCallback(ready == "true"); err != nil {
							r.logger.Infof("Failed to handle dashboard state: %v", err)
						}
					}
				}

			case "keycard":
				if msg.Payload == "authentication" {
					r.logger.Infof("Processing keycard authentication")
					err := r.callbacks.KeycardCallback()
					r.logger.Infof("Keycard authentication callback completed with error: %v", err)
				}

			case "scooter:seatbox":
				if r.callbacks.SeatboxCallback != nil {
					switch msg.Payload {
					case "on", "off":
						if err := r.callbacks.SeatboxCallback(msg.Payload == "on"); err != nil {
							r.logger.Infof("Failed to handle seatbox request: %v", err)
						}
					default:
						r.logger.Infof("Invalid seatbox request value: %s", msg.Payload)
					}
				}

			case "scooter:horn":
				if r.callbacks.HornCallback != nil {
					switch msg.Payload {
					case "on", "off":
						if err := r.callbacks.HornCallback(msg.Payload == "on"); err != nil {
							r.logger.Infof("Failed to handle horn request: %v", err)
						}
					default:
						r.logger.Infof("Invalid horn request value: %s", msg.Payload)
					}
				}

			case "scooter:blinker":
				if r.callbacks.BlinkerCallback != nil {
					switch msg.Payload {
					case "off", "left", "right", "both":
						if err := r.callbacks.BlinkerCallback(msg.Payload); err != nil {
							r.logger.Infof("Failed to handle blinker request: %v", err)
						}
					default:
						r.logger.Infof("Invalid blinker request value: %s", msg.Payload)
					}
				}

			case "scooter:update":
				if r.callbacks.UpdateCallback != nil {
					switch msg.Payload {
					case "start", "complete":
						if err := r.callbacks.UpdateCallback(msg.Payload); err != nil {
							r.logger.Infof("Failed to handle update request: %v", err)
						}
					default:
						r.logger.Infof("Invalid update request value: %s", msg.Payload)
					}
				}

			case "vehicle":
				// msg.Payload contains the hash field that changed
				r.processVehicleMessage(msg.Payload)
			}
		}
	}
}

func (r *RedisClient) processVehicleMessage(payload string) {
	// Handles hash-based scooter commands signalled via the "vehicle" channel.
	// The payload is expected to be the hash field that was modified (e.g. "scooter:seatbox").

	// Only react to the set of commands we currently support.
	var handler func(string) error
	switch payload {
	case "scooter:seatbox":
		handler = r.handleSeatboxCommand
	case "scooter:horn":
		handler = r.handleHornCommand
	case "scooter:blinker":
		handler = r.handleBlinkerCommand
	case "scooter:update":
		handler = r.handleUpdateCommand
	case "seatbox:lock", "brake:left", "brake:right", "blinker:switch", "blinker:state",
	     "kickstand", "handlebar:lock-sensor", "state", "update:status", "fault":
		// These are state updates published by vehicle-service itself, ignore silently
		return
	default:
		// Log truly unknown payloads for debugging
		r.logger.Infof("Unhandled vehicle payload: %s", payload)
		return
	}

	// Fetch the current value for the field
	value, err := r.client.HGet(r.ctx, "vehicle", payload).Result()
	if err == redis.Nil {
		// Field not set – nothing to do.
		return
	}
	if err != nil {
		r.logger.Infof("Error reading hash field %s: %v", payload, err)
		return
	}

	if handler != nil {
		if err := handler(value); err != nil {
			r.logger.Infof("Error handling %s command: %v", payload, err)
		}
	}

	// Clear the field to acknowledge processing, mirroring previous behaviour
	if err := r.client.HDel(r.ctx, "vehicle", payload).Err(); err != nil {
		r.logger.Infof("Error clearing hash field %s: %v", payload, err)
	}
}

func (r *RedisClient) PublishVehicleState(state types.SystemState) error {
	r.logger.Infof("Publishing vehicle state: %s", state)
	pipe := r.client.Pipeline()

	stateStr := string(state)

	pipe.HSet(r.ctx, "vehicle", "state", stateStr)
	pipe.Publish(r.ctx, "vehicle", "state")

	_, err := pipe.Exec(r.ctx)
	if err != nil {
		r.logger.Infof("Failed to publish vehicle state: %v", err)
		return err
	}
	r.logger.Infof("Successfully published vehicle state")
	return nil
}

func (r *RedisClient) SetBlinkerSwitch(state string) error {
	r.logger.Infof("Setting blinker switch: %s", state)

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

	pipe := r.client.Pipeline()
	pipe.HSet(r.ctx, "vehicle", "blinker:switch", blinkerStr)
	pipe.Publish(r.ctx, "vehicle", "blinker:switch")

	_, err := pipe.Exec(r.ctx)
	if err != nil {
		r.logger.Infof("Failed to set blinker switch: %v", err)
		return err
	}
	r.logger.Infof("Successfully set blinker switch")
	return nil
}

func (r *RedisClient) SetBlinkerState(state string) error {
	r.logger.Infof("Setting blinker state: %s", state)

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

	pipe := r.client.Pipeline()
	pipe.HSet(r.ctx, "vehicle", "blinker:state", blinkerStr)
	pipe.Publish(r.ctx, "vehicle", "blinker:state")

	_, err := pipe.Exec(r.ctx)
	if err != nil {
		r.logger.Infof("Failed to set blinker state: %v", err)
		return err
	}
	r.logger.Infof("Successfully set blinker state")
	return nil
}

func (r *RedisClient) GetVehicleState() (types.SystemState, error) {
	r.logger.Infof("Getting vehicle state from Redis")
	stateStr, err := r.client.HGet(r.ctx, "vehicle", "state").Result()
	if err == redis.Nil {
		r.logger.Infof("No vehicle state found in Redis")
		return types.StateInit, nil
	}
	if err != nil {
		r.logger.Infof("Failed to get vehicle state: %v", err)
		return types.StateInit, err
	}
	r.logger.Infof("Successfully retrieved vehicle state: %s", stateStr)
	return types.SystemState(stateStr), nil
}

func (r *RedisClient) SetBrakeState(side string, isPressed bool) error {
	r.logger.Infof("Setting brake state: %s=%v", side, isPressed)
	state := "off"
	if isPressed {
		state = "on"
	}

	pipe := r.client.Pipeline()
	pipe.HSet(r.ctx, "vehicle", fmt.Sprintf("brake:%s", side), state)
	pipe.Publish(r.ctx, "vehicle", fmt.Sprintf("brake:%s", side))

	_, err := pipe.Exec(r.ctx)
	if err != nil {
		r.logger.Infof("Failed to set brake state: %v", err)
		return err
	}
	r.logger.Infof("Successfully set brake state")
	return nil
}

func (r *RedisClient) SetHornButton(isPressed bool) error {
	r.logger.Infof("Setting horn button state: %v", isPressed)
	state := "off"
	if isPressed {
		state = "on"
	}

	pipe := r.client.Pipeline()
	pipe.HSet(r.ctx, "vehicle", "horn:button", state)
	// Also publish an immediate event via pubsub for button press
	pipe.Publish(r.ctx, "buttons", fmt.Sprintf("horn:%s", state))

	_, err := pipe.Exec(r.ctx)
	if err != nil {
		r.logger.Infof("Failed to set horn button state: %v", err)
		return err
	}
	r.logger.Infof("Successfully set horn button state")
	return nil
}

func (r *RedisClient) SetSeatboxButton(isPressed bool) error {
	r.logger.Infof("Setting seatbox button state: %v", isPressed)
	state := "off"
	if isPressed {
		state = "on"
	}

	pipe := r.client.Pipeline()
	pipe.HSet(r.ctx, "vehicle", "seatbox:button", state)
	// Also publish an immediate event via pubsub for button press
	pipe.Publish(r.ctx, "buttons", fmt.Sprintf("seatbox:%s", state))

	_, err := pipe.Exec(r.ctx)
	if err != nil {
		r.logger.Infof("Failed to set seatbox button state: %v", err)
		return err
	}
	r.logger.Infof("Successfully set seatbox button state")
	return nil
}

func (r *RedisClient) SetSeatboxLockState(isLocked bool) error {
	r.logger.Infof("Setting seatbox lock state: %v", isLocked)
	state := "open"
	if isLocked {
		state = "closed"
	}

	pipe := r.client.Pipeline()
	pipe.HSet(r.ctx, "vehicle", "seatbox:lock", state)
	pipe.Publish(r.ctx, "vehicle", "seatbox:lock")

	_, err := pipe.Exec(r.ctx)
	if err != nil {
		r.logger.Infof("Failed to set seatbox lock state: %v", err)
		return err
	}
	r.logger.Infof("Successfully set seatbox lock state")
	return nil
}

func (r *RedisClient) PublishSeatboxOpened() error {
	r.logger.Infof("Publishing seatbox opened event")
	if err := r.client.Publish(r.ctx, "vehicle", "seatbox:opened").Err(); err != nil {
		r.logger.Infof("Failed to publish seatbox opened: %v", err)
		return err
	}
	return nil
}

func (r *RedisClient) SetKickstandState(isDown bool) error {
	r.logger.Infof("Setting kickstand state: %v", isDown)
	state := "up"
	if isDown {
		state = "down"
	}

	pipe := r.client.Pipeline()
	pipe.HSet(r.ctx, "vehicle", "kickstand", state)
	pipe.Publish(r.ctx, "vehicle", "kickstand")

	_, err := pipe.Exec(r.ctx)
	if err != nil {
		r.logger.Infof("Failed to set kickstand state: %v", err)
		return err
	}
	r.logger.Infof("Successfully set kickstand state")
	return nil
}

func (r *RedisClient) SetHandlebarPosition(isOnPlace bool) error {
	r.logger.Infof("Setting handlebar position: %v", isOnPlace)
	state := "off-place"
	if isOnPlace {
		state = "on-place"
	}

	_, err := r.client.HSet(r.ctx, "vehicle", "handlebar:position", state).Result()
	if err != nil {
		r.logger.Infof("Failed to set handlebar position: %v", err)
		return err
	}
	r.logger.Infof("Successfully set handlebar position")
	return nil
}

func (r *RedisClient) SetHandlebarLockState(isLocked bool) error {
	r.logger.Infof("Setting handlebar lock state: %v", isLocked)
	state := "unlocked"
	if isLocked {
		state = "locked"
	}

	pipe := r.client.Pipeline()
	pipe.HSet(r.ctx, "vehicle", "handlebar:lock-sensor", state)
	pipe.Publish(r.ctx, "vehicle", "handlebar:lock-sensor")

	_, err := pipe.Exec(r.ctx)
	if err != nil {
		r.logger.Infof("Failed to set handlebar lock state: %v", err)
		return err
	}
	r.logger.Infof("Successfully set handlebar lock state")
	return nil
}

// PublishUpdateStatus publishes the update status to Redis
func (r *RedisClient) PublishUpdateStatus(status string) error {
	r.logger.Infof("Publishing update status: %s", status)
	pipe := r.client.Pipeline()

	pipe.HSet(r.ctx, "vehicle", "update:status", status)
	pipe.Publish(r.ctx, "vehicle", "update:status")

	_, err := pipe.Exec(r.ctx)
	if err != nil {
		r.logger.Infof("Failed to publish update status: %v", err)
		return err
	}
	r.logger.Infof("Successfully published update status")
	return nil
}

// PublishButtonEvent publishes a button event to the "buttons" channel
func (r *RedisClient) PublishButtonEvent(event string) error {
	r.logger.Infof("Publishing button event: %s", event)
	if err := r.client.Publish(r.ctx, "buttons", event).Err(); err != nil {
		r.logger.Infof("Failed to publish button event: %v", err)
		return err
	}
	r.logger.Infof("Successfully published button event")
	return nil
}

// PublishGovernorChange publishes a governor change event to Redis
func (r *RedisClient) PublishGovernorChange(governor string) error {
	r.logger.Infof("Publishing governor change: %s", governor)
	pipe := r.client.Pipeline()

	pipe.HSet(r.ctx, "system", "cpu:governor", governor)
	pipe.Publish(r.ctx, "system", "cpu:governor")

	_, err := pipe.Exec(r.ctx)
	if err != nil {
		r.logger.Infof("Failed to publish governor change: %v", err)
		return err
	}
	r.logger.Infof("Successfully published governor change")
	return nil
}

// DeleteDashboardReadyFlag deletes the dashboard ready flag from Redis
func (r *RedisClient) DeleteDashboardReadyFlag() error {
	r.logger.Infof("Deleting dashboard ready flag from Redis")
	if err := r.client.HDel(r.ctx, "dashboard", "ready").Err(); err != nil {
		r.logger.Infof("Failed to delete dashboard ready flag: %v", err)
		return err
	}
	r.logger.Infof("Successfully deleted dashboard ready flag")
	return nil
}

// PublishStandbyTimerStart sets the standby timer start timestamp for MDB reboot coordination
func (r *RedisClient) PublishStandbyTimerStart() error {
	r.logger.Infof("Setting standby timer start timestamp")
	timestamp := fmt.Sprintf("%d", time.Now().Unix())

	pipe := r.client.Pipeline()
	pipe.HSet(r.ctx, "ota", "standby-timer-start", timestamp)
	pipe.Publish(r.ctx, "ota", "standby-timer-start")

	_, err := pipe.Exec(r.ctx)
	if err != nil {
		r.logger.Infof("Failed to set standby timer start: %v", err)
		return err
	}
	r.logger.Infof("Successfully set standby timer start: %s", timestamp)
	return nil
}

// GetOtaStatus gets the OTA status for a specific component from Redis
func (r *RedisClient) GetOtaStatus(component string) (string, error) {
	statusKey := fmt.Sprintf("status:%s", component)
	status, err := r.client.HGet(r.ctx, "ota", statusKey).Result()
	if err == redis.Nil {
		// Status field doesn't exist, return empty string (idle)
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to get OTA status for component %s: %w", component, err)
	}
	return status, nil
}

// SendCommand sends a command to a Redis list (for communication with other services)
func (r *RedisClient) SendCommand(channel, command string) error {
	err := r.client.LPush(r.ctx, channel, command).Err()
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

	pipe := r.client.Pipeline()

	// Add fault code to active faults set
	pipe.SAdd(r.ctx, "vehicle:fault", code)

	// Add fault event to global event stream with metadata
	eventData := map[string]interface{}{
		"group":       "vehicle",
		"code":        code,
		"description": description,
		"ts":          timestamp,
	}
	if info != "" {
		eventData["info"] = info
	}
	pipe.XAdd(r.ctx, &redis.XAddArgs{
		Stream: "events:faults",
		MaxLen: 1000,
		Values: eventData,
	})

	// Publish notification
	pipe.Publish(r.ctx, "vehicle", "fault")

	_, err := pipe.Exec(r.ctx)
	if err != nil {
		r.logger.Infof("Failed to report fault present: %v", err)
		return err
	}

	r.logger.Infof("Successfully reported fault %d as present", code)
	return nil
}

// ReportFaultAbsent reports a fault as absent (cleared) to Redis
func (r *RedisClient) ReportFaultAbsent(code int) error {
	r.logger.Infof("Reporting fault absent: code=%d", code)

	pipe := r.client.Pipeline()

	// Remove fault code from active faults set
	pipe.SRem(r.ctx, "vehicle:fault", code)

	// Add clear event to global event stream (negative code indicates cleared)
	pipe.XAdd(r.ctx, &redis.XAddArgs{
		Stream: "events:faults",
		MaxLen: 1000,
		Values: map[string]interface{}{
			"group": "vehicle",
			"code":  -code, // Negative code indicates fault cleared
		},
	})

	// Publish notification
	pipe.Publish(r.ctx, "vehicle", "fault")

	_, err := pipe.Exec(r.ctx)
	if err != nil {
		r.logger.Infof("Failed to report fault absent: %v", err)
		return err
	}

	r.logger.Infof("Successfully reported fault %d as absent", code)
	return nil
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
