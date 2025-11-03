package hardware

import (
	"encoding/binary"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"vehicle-service/internal/logger"

	"github.com/warthog618/go-gpiocdev"
)

const (
	EV_SYN = 0x00
	EV_KEY = 0x01

	KEY_A = 30 // brake_right
	KEY_B = 48 // brake_left
	KEY_C = 46 // horn_button
	KEY_D = 32 // seatbox_button
	KEY_E = 18 // kickstand
	KEY_F = 33 // blinker_right
	KEY_G = 34 // blinker_left
	KEY_H = 35 // ecu_power
	KEY_K = 37 // handlebar_lock_sensor
	KEY_I = 38 // handlebar_position
	KEY_J = 36 // seatbox_lock_sensor
	KEY_Q = 16 // 48v_detect
)

type InputEvent struct {
	Sec   int32  // 4 bytes
	Usec  int32  // 4 bytes
	Type  uint16 // 2 bytes
	Code  uint16 // 2 bytes
	Value int32  // 4 bytes
}

type InputCallback func(channel string, value bool) error

// Single source of truth for keycode to channel mappings
var keycodeToChannel = map[uint16]string{
	KEY_A: "brake_right",
	KEY_B: "brake_left",
	KEY_C: "horn_button",
	KEY_D: "seatbox_button",
	KEY_E: "kickstand",
	KEY_F: "blinker_right",
	KEY_G: "blinker_left",
	KEY_H: "ecu_power",
	KEY_K: "handlebar_lock_sensor",
	KEY_I: "handlebar_position",
	KEY_J: "seatbox_lock_sensor",
	KEY_Q: "48v_detect",
}

// Reverse map built at initialization
var channelToKeycode map[string]uint16

func init() {
	// Build reverse map from forward map
	channelToKeycode = make(map[string]uint16, len(keycodeToChannel))
	for code, channel := range keycodeToChannel {
		channelToKeycode[channel] = code
	}
}

type LinuxHardwareIO struct {
	logger          *logger.Logger
	inputDevicePath string
	inputFile       *os.File
	chips           map[int]*gpiocdev.Chip
	lines           map[string]*gpiocdev.Line
	inputCallbacks  map[string]InputCallback
	pwmLed          *ImxPwmLed
	mu              sync.RWMutex
	stopChan        chan struct{}
	activeKeys      map[uint16]bool // Track key states
	initialValues   map[string]bool // Initial values for outputs
}

func NewLinuxHardwareIO(l *logger.Logger) *LinuxHardwareIO {
	return &LinuxHardwareIO{
		logger:          l,
		inputDevicePath: GpioKeysInput,
		chips:           make(map[int]*gpiocdev.Chip),
		lines:           make(map[string]*gpiocdev.Line),
		inputCallbacks:  make(map[string]InputCallback),
		stopChan:        make(chan struct{}),
		activeKeys:      make(map[uint16]bool),
		initialValues:   make(map[string]bool),
	}
}

func (io *LinuxHardwareIO) SetInitialValue(name string, value bool) {
	io.mu.Lock()
	defer io.mu.Unlock()
	io.initialValues[name] = value
}

func (io *LinuxHardwareIO) Initialize() error {
	io.logger.Infof("Initializing hardware IO")

	// Initialize PWM LED
	io.pwmLed = NewImxPwmLed()
	if err := io.pwmLed.Init(); err != nil {
		return fmt.Errorf("failed to initialize PWM LED: %w", err)
	}

	// Initialize GPIO outputs
	for name, mapping := range DoMappings {
		chip, err := gpiocdev.NewChip(fmt.Sprintf("gpiochip%d", mapping.Chip))
		if err != nil {
			return fmt.Errorf("failed to open GPIO chip %d: %w", mapping.Chip, err)
		}

		io.chips[mapping.Chip] = chip

		// Get initial value for this output
		io.mu.RLock()
		val := 0
		if value, exists := io.initialValues[name]; exists && value {
			val = 1
		}
		io.mu.RUnlock()

		// Request line as output with initial value
		line, err := chip.RequestLine(mapping.Line,
			gpiocdev.AsOutput(val),
			gpiocdev.WithConsumer("vehicle-service"))
		if err != nil {
			return fmt.Errorf("failed to request GPIO line %d: %w", mapping.Line, err)
		}

		io.lines[name] = line
		io.logger.Infof("Configured DO %s: chip=%d, line=%d", name, mapping.Chip, mapping.Line)
	}

	// Open input device
	io.logger.Infof("Opening input device: %s", io.inputDevicePath)
	var err error
	io.inputFile, err = os.OpenFile(io.inputDevicePath, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("failed to open input device %s: %w", io.inputDevicePath, err)
	}
	io.logger.Infof("Successfully opened input device")

	// Get initial state of all inputs
	if err := io.readInitialState(); err != nil {
		io.logger.Infof("Warning: Failed to read initial input states: %v", err)
	}

	// Start input monitoring
	go io.monitorInputs()

	return nil
}

func (io *LinuxHardwareIO) readInitialState() error {
	// Use EVIOCGKEY ioctl to get key states
	// The key state is returned as a bit array
	buffer := make([]byte, 128)
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(io.inputFile.Fd()),
		uintptr(0x80804518), // EVIOCGKEY(len)
		uintptr(unsafe.Pointer(&buffer[0])),
	)
	if errno != 0 {
		return fmt.Errorf("EVIOCGKEY ioctl failed: %v", errno)
	}

	io.mu.Lock()
	defer io.mu.Unlock()

	// Check each key we care about
	keycodes := []uint16{
		KEY_A, KEY_B, KEY_C, KEY_D, KEY_E, KEY_F, KEY_G, KEY_H,
		KEY_K, KEY_I, KEY_J, KEY_Q,
	}

	for _, code := range keycodes {
		byteOffset := int(code / 8)
		bitOffset := code % 8
		if byteOffset < len(buffer) {
			isPressed := (buffer[byteOffset] & (1 << bitOffset)) != 0
			io.activeKeys[code] = isPressed
			if isPressed {
				channel := io.mapKeycode(code)
				io.logger.Infof("Initial state: %s (code %d) is pressed", channel, code)
			}
		}
	}

	return nil
}

func (io *LinuxHardwareIO) monitorInputs() {
	defer io.inputFile.Close()

	buffer := make([]byte, 16)
	io.logger.Infof("Starting input event monitoring with buffer size: %d", len(buffer))

	for {
		select {
		case <-io.stopChan:
			io.logger.Infof("Stopping input monitoring")
			return
		default:
			n, err := io.inputFile.Read(buffer)
			if err != nil {
				io.logger.Infof("Error reading input: %v", err)
				time.Sleep(100 * time.Millisecond)
				continue
			}
			if n != len(buffer) {
				io.logger.Infof("Incomplete read: got %d bytes, expected %d", n, len(buffer))
				continue
			}

			sec := int32(binary.LittleEndian.Uint32(buffer[0:4]))
			usec := int32(binary.LittleEndian.Uint32(buffer[4:8]))
			typ := binary.LittleEndian.Uint16(buffer[8:10])
			code := binary.LittleEndian.Uint16(buffer[10:12])
			val := int32(binary.LittleEndian.Uint32(buffer[12:16]))

			io.logger.Infof("Event: type=%d code=%d value=%d time=%d.%d",
				typ, code, val, sec, usec)

			if typ == EV_KEY {
				io.handleKeyEvent(&InputEvent{
					Sec:   sec,
					Usec:  usec,
					Type:  typ,
					Code:  code,
					Value: val,
				})
			}
		}
	}
}

func (io *LinuxHardwareIO) handleKeyEvent(event *InputEvent) {
	channel := io.mapKeycode(event.Code)
	io.logger.Infof("Key event: code=%d channel=%q value=%d",
		event.Code, channel, event.Value)

	// Update active keys map
	io.mu.Lock()
	if event.Value == 0 {
		delete(io.activeKeys, event.Code)
		io.logger.Infof("Key released: code=%d", event.Code)
	} else {
		io.activeKeys[event.Code] = true
		io.logger.Infof("Key pressed: code=%d", event.Code)
	}
	io.mu.Unlock()

	// Only process key press (1) and release (0)
	if event.Value > 1 {
		io.logger.Infof("Ignoring repeat event")
		return
	}

	if channel == "" {
		io.logger.Infof("Unknown key code: %d", event.Code)
		return
	}

	io.mu.RLock()
	callback, exists := io.inputCallbacks[channel]
	io.mu.RUnlock()

	if exists {
		io.logger.Infof("Executing callback for channel %s (value=%v)",
			channel, event.Value == 1)
		if err := callback(channel, event.Value == 1); err != nil {
			io.logger.Infof("Error in callback for %s: %v", channel, err)
		}
	} else {
		io.logger.Infof("No callback registered for channel: %s", channel)
	}
}

func (io *LinuxHardwareIO) mapKeycode(code uint16) string {
	return keycodeToChannel[code]
}

func (io *LinuxHardwareIO) ReadDigitalInput(channel string) (bool, error) {
	// After initialization, activeKeys is kept in sync by readInitialState and
	// handleKeyEvent.  It is therefore the single source of truth for input
	// state queries originating from application logic.  Rely exclusively on
	// it here to avoid unnecessary and potentially expensive ioctl calls.

	io.mu.RLock()
	keycode := io.getKeycodeForChannel(channel)
	if keycode == 0 {
		io.mu.RUnlock()
		return false, fmt.Errorf("unknown input channel: %s", channel)
	}

	state := io.activeKeys[keycode]
	io.mu.RUnlock()

	io.logger.Infof("Reading digital input for channel %s (keycode %d) -> %v (cached)", channel, keycode, state)
	return state, nil
}

func (io *LinuxHardwareIO) getKeycodeForChannel(channel string) uint16 {
	return channelToKeycode[channel]
}

func (io *LinuxHardwareIO) RegisterInputCallback(channel string, callback InputCallback) {
	io.mu.Lock()
	defer io.mu.Unlock()
	io.inputCallbacks[channel] = callback
	io.logger.Infof("Registered callback for channel: %s", channel)
}

func (io *LinuxHardwareIO) WriteDigitalOutput(channel string, value bool) error {
	io.mu.RLock()
	line, ok := io.lines[channel]
	io.mu.RUnlock()

	if !ok {
		return fmt.Errorf("unknown digital output channel: %s", channel)
	}

	val := 0
	if value {
		val = 1
	}

	if err := line.SetValue(val); err != nil {
		return fmt.Errorf("failed to set DO %s=%v: %w", channel, value, err)
	}

	io.logger.Infof("Set DO %s=%v", channel, value)
	return nil
}

func (io *LinuxHardwareIO) PlayPwmCue(idx int) error {
	return io.pwmLed.PlayCue(idx)
}

func (io *LinuxHardwareIO) PlayPwmFade(ch int, idx int) error {
	return io.pwmLed.PlayFade(ch, idx)
}

func (io *LinuxHardwareIO) Cleanup() {
	close(io.stopChan)

	// First close the input file descriptor to interrupt any blocked Read() calls
	if io.inputFile != nil {
		io.inputFile.Close()
		io.logger.Infof("Closed input device file descriptor")
	}

	// Short delay to allow goroutine to process the error and exit
	time.Sleep(100 * time.Millisecond)

	io.mu.Lock()
	defer io.mu.Unlock()

	io.logger.Infof("Cleaning up hardware resources")

	for name, line := range io.lines {
		line.Close()
		io.logger.Infof("Closed GPIO line for %s", name)
	}

	for id, chip := range io.chips {
		chip.Close()
		io.logger.Infof("Closed GPIO chip %d", id)
	}

	if io.pwmLed != nil {
		io.pwmLed.Cleanup()
		io.logger.Infof("Cleaned up PWM LED")
	}

	io.logger.Infof("Hardware cleanup complete")
}
