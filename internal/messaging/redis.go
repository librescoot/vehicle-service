package messaging

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"vehicle-service/internal/types"

	"github.com/redis/go-redis/v9"
)

type Callbacks struct {
	DashboardCallback func(bool) error
	KeycardCallback   func() error
	SeatboxCallback   func(bool) error   // true for "on", false for "off"
	HornCallback      func(bool) error   // true for "on", false for "off"
	BlinkerCallback   func(string) error // "off", "left", "right", "both"
	PowerCallback     func(string) error // "hibernate-manual", "reboot"
	StateCallback     func(string) error // "unlock", "lock", "lock-hibernate"
	ForceLockCallback func() error       // New callback for force-lock
	LedCueCallback    func(int) error
	LedFadeCallback   func(int, int) error
	UpdateCallback    func(string) error // "start", "complete"
}

type RedisClient struct {
	client    *redis.Client
	callbacks Callbacks
	logger    *log.Logger
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

func NewRedisClient(host string, port int, callbacks Callbacks) *RedisClient {
	ctx, cancel := context.WithCancel(context.Background())
	return &RedisClient{
		client: redis.NewClient(&redis.Options{
			Addr: fmt.Sprintf("%s:%d", host, port),
			DB:   0,
		}),
		callbacks: callbacks,
		logger:    log.New(log.Writer(), "Redis: ", log.LstdFlags),
		ctx:       ctx,
		cancel:    cancel,
	}
}

func (r *RedisClient) Connect() error {
	r.logger.Printf("Attempting to connect to Redis at %s", r.client.Options().Addr)

	if err := r.client.Ping(r.ctx).Err(); err != nil {
		r.logger.Printf("Redis connection failed: %v", err)
		return fmt.Errorf("Redis connection failed: %w", err)
	}
	r.logger.Printf("Successfully connected to Redis")

	ready, err := r.client.HGet(r.ctx, "dashboard", "ready").Result()
	if err != nil && err != redis.Nil {
		r.logger.Printf("Failed to get initial dashboard state: %v", err)
	} else {
		r.logger.Printf("Initial dashboard ready state: %v", ready == "true")
		if ready == "true" {
			if err := r.callbacks.DashboardCallback(true); err != nil {
				r.logger.Printf("Failed to handle initial dashboard state: %v", err)
			}
		}
	}

	return nil
}

// StartListening starts all Redis listeners after system initialization is complete
func (r *RedisClient) StartListening() error {
	r.logger.Printf("Starting Redis listeners")

	// Subscribe to pub/sub channels for system events
	pubsub := r.client.Subscribe(r.ctx, "dashboard", "keycard", "ota", "power-manager", "vehicle")
	r.logger.Printf("Subscribed to Redis channels: dashboard, keycard, ota, power-manager, vehicle")

	// Start pub/sub listener
	r.wg.Add(1)
	go r.redisListener(pubsub)

	// Start list command listeners for LPUSH commands
	r.wg.Add(8) // Added 1 for update commands
	go r.listCommandListener("scooter:seatbox", r.handleSeatboxCommand)
	go r.listCommandListener("scooter:horn", r.handleHornCommand)
	go r.listCommandListener("scooter:blinker", r.handleBlinkerCommand)
	go r.listCommandListener("scooter:power", r.handlePowerCommand)
	go r.listCommandListener("scooter:state", r.handleStateCommand)
	go r.listCommandListener("scooter:led:cue", r.handleLedCueCommand)
	go r.listCommandListener("scooter:led:fade", r.handleLedFadeCommand)
	go r.listCommandListener("scooter:update", r.handleUpdateCommand)

	// Hash field monitor replaced with pub/sub trigger on "vehicle" channel (see redisListener)

	return nil
}

func (r *RedisClient) listCommandListener(key string, handler func(string) error) {
	defer r.wg.Done()
	r.logger.Printf("Starting list command listener for %s", key)

	for {
		select {
		case <-r.ctx.Done():
			r.logger.Printf("Context cancelled, exiting %s listener", key)
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
					r.logger.Printf("Context cancelled, exiting %s listener", key)
					return
				}
				r.logger.Printf("Error reading from %s list: %v", key, err)
				continue
			}

			if len(result) >= 2 { // BRPOP returns [key, value]
				value := result[1]
				r.logger.Printf("Received command from %s: %s", key, value)
				if err := handler(value); err != nil {
					r.logger.Printf("Error handling %s command: %v", key, err)
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
		r.logger.Printf("Invalid seatbox command value: %s", value)
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
		r.logger.Printf("Invalid horn command value: %s", value)
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
		r.logger.Printf("Invalid blinker command value: %s", value)
		return fmt.Errorf("invalid blinker command: %s", value)
	}
}

func (r *RedisClient) handlePowerCommand(value string) error {
	if r.callbacks.PowerCallback == nil {
		return nil
	}
	switch value {
	case "hibernate-manual", "reboot":
		return r.callbacks.PowerCallback(value)
	default:
		r.logger.Printf("Invalid power command value: %s", value)
		return fmt.Errorf("invalid power command: %s", value)
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
		r.logger.Printf("Invalid state command value: %s", value)
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
		r.logger.Printf("Invalid LED cue command value: %s, expected integer: %v", value, err)
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
		r.logger.Printf("Invalid LED fade command value: %s, expected 'channel:index': %v", value, err)
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
		r.logger.Printf("Invalid update command value: %s", value)
		return fmt.Errorf("invalid update command: %s", value)
	}
}

func (r *RedisClient) redisListener(pubsub *redis.PubSub) {
	defer r.wg.Done()
	defer pubsub.Close()

	r.logger.Printf("Starting Redis message listener")
	channel := pubsub.Channel()

	for {
		select {
		case msg := <-channel:
			if msg == nil {
				r.logger.Printf("Received nil message, exiting listener")
				return
			}

			r.logger.Printf("Received Redis message: channel=%s payload=%s", msg.Channel, msg.Payload)

			switch msg.Channel {
			case "dashboard":
				ready, err := r.client.HGet(r.ctx, "dashboard", "ready").Result()
				if err != nil {
					r.logger.Printf("Failed to get dashboard state: %v", err)
				} else {
					r.logger.Printf("Processing dashboard ready state: %v", ready == "true")
					if err := r.callbacks.DashboardCallback(ready == "true"); err != nil {
						r.logger.Printf("Failed to handle dashboard state: %v", err)
					}
				}

			case "keycard":
				if msg.Payload == "authentication" {
					r.logger.Printf("Processing keycard authentication")
					err := r.callbacks.KeycardCallback()
					r.logger.Printf("Keycard authentication callback completed with error: %v", err)
				}

			case "scooter:seatbox":
				if r.callbacks.SeatboxCallback != nil {
					switch msg.Payload {
					case "on", "off":
						if err := r.callbacks.SeatboxCallback(msg.Payload == "on"); err != nil {
							r.logger.Printf("Failed to handle seatbox request: %v", err)
						}
					default:
						r.logger.Printf("Invalid seatbox request value: %s", msg.Payload)
					}
				}

			case "scooter:horn":
				if r.callbacks.HornCallback != nil {
					switch msg.Payload {
					case "on", "off":
						if err := r.callbacks.HornCallback(msg.Payload == "on"); err != nil {
							r.logger.Printf("Failed to handle horn request: %v", err)
						}
					default:
						r.logger.Printf("Invalid horn request value: %s", msg.Payload)
					}
				}

			case "scooter:blinker":
				if r.callbacks.BlinkerCallback != nil {
					switch msg.Payload {
					case "off", "left", "right", "both":
						if err := r.callbacks.BlinkerCallback(msg.Payload); err != nil {
							r.logger.Printf("Failed to handle blinker request: %v", err)
						}
					default:
						r.logger.Printf("Invalid blinker request value: %s", msg.Payload)
					}
				}

			case "scooter:power":
				if r.callbacks.PowerCallback != nil {
					switch msg.Payload {
					case "hibernate-manual", "reboot":
						if err := r.callbacks.PowerCallback(msg.Payload); err != nil {
							r.logger.Printf("Failed to handle power request: %v", err)
						}
					default:
						r.logger.Printf("Invalid power request value: %s", msg.Payload)
					}
				}

			case "scooter:update":
				if r.callbacks.UpdateCallback != nil {
					switch msg.Payload {
					case "start", "complete":
						if err := r.callbacks.UpdateCallback(msg.Payload); err != nil {
							r.logger.Printf("Failed to handle update request: %v", err)
						}
					default:
						r.logger.Printf("Invalid update request value: %s", msg.Payload)
					}
				}

			case "vehicle":
				// msg.Payload contains the hash field that changed
				r.processVehicleMessage(msg.Payload)
			}

		case <-r.ctx.Done():
			r.logger.Printf("Context cancelled, exiting listener")
			return
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
	case "scooter:power":
		handler = r.handlePowerCommand
	case "scooter:update":
		handler = r.handleUpdateCommand
	default:
		// Ignore unknown payloads to keep behaviour predictable
		r.logger.Printf("Unhandled vehicle payload: %s", payload)
		return
	}

	// Fetch the current value for the field
	value, err := r.client.HGet(r.ctx, "vehicle", payload).Result()
	if err == redis.Nil {
		// Field not set – nothing to do.
		return
	}
	if err != nil {
		r.logger.Printf("Error reading hash field %s: %v", payload, err)
		return
	}

	if handler != nil {
		if err := handler(value); err != nil {
			r.logger.Printf("Error handling %s command: %v", payload, err)
		}
	}

	// Clear the field to acknowledge processing, mirroring previous behaviour
	if err := r.client.HDel(r.ctx, "vehicle", payload).Err(); err != nil {
		r.logger.Printf("Error clearing hash field %s: %v", payload, err)
	}
}

func (r *RedisClient) PublishVehicleState(state types.SystemState) error {
	r.logger.Printf("Publishing vehicle state: %s", state)
	pipe := r.client.Pipeline()

	stateStr := string(state)

	pipe.HSet(r.ctx, "vehicle", "state", stateStr)
	pipe.Publish(r.ctx, "vehicle", "state")

	_, err := pipe.Exec(r.ctx)
	if err != nil {
		r.logger.Printf("Failed to publish vehicle state: %v", err)
		return err
	}
	r.logger.Printf("Successfully published vehicle state")
	return nil
}

func (r *RedisClient) SetBlinkerSwitch(state string) error {
	r.logger.Printf("Setting blinker switch: %s", state)

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
		r.logger.Printf("Failed to set blinker switch: %v", err)
		return err
	}
	r.logger.Printf("Successfully set blinker switch")
	return nil
}

func (r *RedisClient) SetBlinkerState(state string) error {
	r.logger.Printf("Setting blinker state: %s", state)

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
		r.logger.Printf("Failed to set blinker state: %v", err)
		return err
	}
	r.logger.Printf("Successfully set blinker state")
	return nil
}

func (r *RedisClient) GetVehicleState() (types.SystemState, error) {
	r.logger.Printf("Getting vehicle state from Redis")
	stateStr, err := r.client.HGet(r.ctx, "vehicle", "state").Result()
	if err == redis.Nil {
		r.logger.Printf("No vehicle state found in Redis")
		return types.StateInit, nil
	}
	if err != nil {
		r.logger.Printf("Failed to get vehicle state: %v", err)
		return types.StateInit, err
	}
	r.logger.Printf("Successfully retrieved vehicle state: %s", stateStr)
	return types.SystemState(stateStr), nil
}

func (r *RedisClient) SetBrakeState(side string, isPressed bool) error {
	r.logger.Printf("Setting brake state: %s=%v", side, isPressed)
	state := "off"
	if isPressed {
		state = "on"
	}

	pipe := r.client.Pipeline()
	pipe.HSet(r.ctx, "vehicle", fmt.Sprintf("brake:%s", side), state)
	pipe.Publish(r.ctx, "vehicle", fmt.Sprintf("brake:%s", side))

	_, err := pipe.Exec(r.ctx)
	if err != nil {
		r.logger.Printf("Failed to set brake state: %v", err)
		return err
	}
	r.logger.Printf("Successfully set brake state")
	return nil
}

func (r *RedisClient) SetHornButton(isPressed bool) error {
	r.logger.Printf("Setting horn button state: %v", isPressed)
	state := "off"
	if isPressed {
		state = "on"
	}

	_, err := r.client.HSet(r.ctx, "vehicle", "horn:button", state).Result()
	if err != nil {
		r.logger.Printf("Failed to set horn button state: %v", err)
		return err
	}
	r.logger.Printf("Successfully set horn button state")
	return nil
}

func (r *RedisClient) SetSeatboxButton(isPressed bool) error {
	r.logger.Printf("Setting seatbox button state: %v", isPressed)
	state := "off"
	if isPressed {
		state = "on"
	}

	_, err := r.client.HSet(r.ctx, "vehicle", "seatbox:button", state).Result()
	if err != nil {
		r.logger.Printf("Failed to set seatbox button state: %v", err)
		return err
	}
	r.logger.Printf("Successfully set seatbox button state")
	return nil
}

func (r *RedisClient) SetSeatboxLockState(isLocked bool) error {
	r.logger.Printf("Setting seatbox lock state: %v", isLocked)
	state := "open"
	if isLocked {
		state = "closed"
	}

	pipe := r.client.Pipeline()
	pipe.HSet(r.ctx, "vehicle", "seatbox:lock", state)
	pipe.Publish(r.ctx, "vehicle", "seatbox:lock")

	_, err := pipe.Exec(r.ctx)
	if err != nil {
		r.logger.Printf("Failed to set seatbox lock state: %v", err)
		return err
	}
	r.logger.Printf("Successfully set seatbox lock state")
	return nil
}

func (r *RedisClient) SetKickstandState(isDown bool) error {
	r.logger.Printf("Setting kickstand state: %v", isDown)
	state := "up"
	if isDown {
		state = "down"
	}

	pipe := r.client.Pipeline()
	pipe.HSet(r.ctx, "vehicle", "kickstand", state)
	pipe.Publish(r.ctx, "vehicle", "kickstand")

	_, err := pipe.Exec(r.ctx)
	if err != nil {
		r.logger.Printf("Failed to set kickstand state: %v", err)
		return err
	}
	r.logger.Printf("Successfully set kickstand state")
	return nil
}

func (r *RedisClient) SetHandlebarPosition(isOnPlace bool) error {
	r.logger.Printf("Setting handlebar position: %v", isOnPlace)
	state := "off-place"
	if isOnPlace {
		state = "on-place"
	}

	_, err := r.client.HSet(r.ctx, "vehicle", "handlebar:position", state).Result()
	if err != nil {
		r.logger.Printf("Failed to set handlebar position: %v", err)
		return err
	}
	r.logger.Printf("Successfully set handlebar position")
	return nil
}

func (r *RedisClient) SetHandlebarLockState(isLocked bool) error {
	r.logger.Printf("Setting handlebar lock state: %v", isLocked)
	state := "unlocked"
	if isLocked {
		state = "locked"
	}

	pipe := r.client.Pipeline()
	pipe.HSet(r.ctx, "vehicle", "handlebar:lock-sensor", state)
	pipe.Publish(r.ctx, "vehicle", "handlebar:lock-sensor")

	_, err := pipe.Exec(r.ctx)
	if err != nil {
		r.logger.Printf("Failed to set handlebar lock state: %v", err)
		return err
	}
	r.logger.Printf("Successfully set handlebar lock state")
	return nil
}

// PublishUpdateStatus publishes the update status to Redis
func (r *RedisClient) PublishUpdateStatus(status string) error {
	r.logger.Printf("Publishing update status: %s", status)
	pipe := r.client.Pipeline()

	pipe.HSet(r.ctx, "vehicle", "update:status", status)
	pipe.Publish(r.ctx, "vehicle", "update:status")

	_, err := pipe.Exec(r.ctx)
	if err != nil {
		r.logger.Printf("Failed to publish update status: %v", err)
		return err
	}
	r.logger.Printf("Successfully published update status")
	return nil
}

func (r *RedisClient) Close() error {
	r.cancel()
	r.wg.Wait()
	return r.client.Close()
}
