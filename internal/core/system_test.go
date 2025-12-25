package core

import (
	"context"
	"testing"
	"time"

	"github.com/librescoot/librefsm"

	"vehicle-service/internal/fsm"
	"vehicle-service/internal/hardware"
	"vehicle-service/internal/logger"
	"vehicle-service/internal/messaging"
	"vehicle-service/internal/types"
)

// Mock MessagingClient
type mockMessagingClient struct {
	callbacks messaging.Callbacks

	// Track method calls
	publishedStates        []types.SystemState
	setHandlebarPositions  []bool
	setBrakeStates         []struct{ side string; pressed bool }
	setBlinkerSwitches     []string
	setBlinkerStates       []string
	publishedButtonEvents  []string
	publishedSeatboxOpened int
	sendCommands           []struct{ channel, command string }

	// Return values
	vehicleState    types.SystemState
	vehicleStateErr error
	dashboardPower  bool
	dbcUpdating     bool
	otaStatus       string
	hashFieldValue  string
}

func newMockMessagingClient() *mockMessagingClient {
	return &mockMessagingClient{
		vehicleState: types.StateParked,
	}
}

func (m *mockMessagingClient) SetCallbacks(callbacks messaging.Callbacks)     { m.callbacks = callbacks }
func (m *mockMessagingClient) Connect() error                                 { return nil }
func (m *mockMessagingClient) StartListening() error                          { return nil }
func (m *mockMessagingClient) Close() error                                   { return nil }
func (m *mockMessagingClient) GetVehicleState() (types.SystemState, error)    { return m.vehicleState, m.vehicleStateErr }
func (m *mockMessagingClient) GetDashboardPower() (bool, error)               { return m.dashboardPower, nil }
func (m *mockMessagingClient) SetDashboardPower(enabled bool) error           { m.dashboardPower = enabled; return nil }
func (m *mockMessagingClient) DeleteDashboardReadyFlag() error                { return nil }
func (m *mockMessagingClient) GetDbcUpdating() (bool, error)                  { return m.dbcUpdating, nil }
func (m *mockMessagingClient) SetDbcUpdating(updating bool) error             { m.dbcUpdating = updating; return nil }
func (m *mockMessagingClient) GetOtaStatus(component string) (string, error)  { return m.otaStatus, nil }
func (m *mockMessagingClient) GetHashField(hash, field string) (string, error) { return m.hashFieldValue, nil }
func (m *mockMessagingClient) PublishAutoStandbyDeadline(deadline time.Time) error { return nil }
func (m *mockMessagingClient) ClearAutoStandbyDeadline() error                { return nil }
func (m *mockMessagingClient) PublishStandbyTimerStart() error                { return nil }
func (m *mockMessagingClient) SetKickstandState(deployed bool) error          { return nil }
func (m *mockMessagingClient) SetHandlebarLockState(locked bool) error        { return nil }
func (m *mockMessagingClient) SetSeatboxLockState(locked bool) error          { return nil }
func (m *mockMessagingClient) SetHornButton(pressed bool) error               { return nil }
func (m *mockMessagingClient) SetSeatboxButton(pressed bool) error            { return nil }
func (m *mockMessagingClient) SendCommand(channel, command string) error {
	m.sendCommands = append(m.sendCommands, struct{ channel, command string }{channel, command})
	return nil
}

func (m *mockMessagingClient) PublishVehicleState(state types.SystemState) error {
	m.publishedStates = append(m.publishedStates, state)
	return nil
}

func (m *mockMessagingClient) SetBrakeState(side string, pressed bool) error {
	m.setBrakeStates = append(m.setBrakeStates, struct{ side string; pressed bool }{side, pressed})
	return nil
}

func (m *mockMessagingClient) SetHandlebarPosition(isOnPlace bool) error {
	m.setHandlebarPositions = append(m.setHandlebarPositions, isOnPlace)
	return nil
}

func (m *mockMessagingClient) SetBlinkerSwitch(state string) error {
	m.setBlinkerSwitches = append(m.setBlinkerSwitches, state)
	return nil
}

func (m *mockMessagingClient) SetBlinkerState(state string) error {
	m.setBlinkerStates = append(m.setBlinkerStates, state)
	return nil
}

func (m *mockMessagingClient) PublishButtonEvent(event string) error {
	m.publishedButtonEvents = append(m.publishedButtonEvents, event)
	return nil
}

func (m *mockMessagingClient) PublishSeatboxOpened() error {
	m.publishedSeatboxOpened++
	return nil
}

// Mock HardwareIO
type mockHardwareIO struct {
	digitalOutputs map[string]bool
	digitalInputs  map[string]bool
	initialValues  map[string]bool
	inputCallbacks map[string]hardware.InputCallback
	pwmCues        []int
	pwmFades       []struct{ ch, idx int }
}

func newMockHardwareIO() *mockHardwareIO {
	return &mockHardwareIO{
		digitalOutputs: make(map[string]bool),
		digitalInputs:  make(map[string]bool),
		initialValues:  make(map[string]bool),
		inputCallbacks: make(map[string]hardware.InputCallback),
	}
}

func (m *mockHardwareIO) Initialize() error { return nil }
func (m *mockHardwareIO) Cleanup()          {}

func (m *mockHardwareIO) ReadDigitalInput(channel string) (bool, error) {
	return m.digitalInputs[channel], nil
}

func (m *mockHardwareIO) WriteDigitalOutput(channel string, value bool) error {
	m.digitalOutputs[channel] = value
	return nil
}

func (m *mockHardwareIO) SetInitialValue(name string, value bool) {
	m.initialValues[name] = value
}

func (m *mockHardwareIO) RegisterInputCallback(channel string, callback hardware.InputCallback) {
	m.inputCallbacks[channel] = callback
}

func (m *mockHardwareIO) PlayPwmCue(idx int) error {
	m.pwmCues = append(m.pwmCues, idx)
	return nil
}

func (m *mockHardwareIO) PlayPwmFade(ch int, idx int) error {
	m.pwmFades = append(m.pwmFades, struct{ ch, idx int }{ch, idx})
	return nil
}

// SimulateInput triggers an input callback
func (m *mockHardwareIO) SimulateInput(channel string, value bool) error {
	if cb, ok := m.inputCallbacks[channel]; ok {
		return cb(channel, value)
	}
	return nil
}

// Test helper
func newTestVehicleSystem() (*VehicleSystem, *mockHardwareIO, *mockMessagingClient) {
	l := logger.NewLogger(nil, logger.LogLevelError)
	mockIO := newMockHardwareIO()
	mockRedis := newMockMessagingClient()
	system := NewVehicleSystem(mockIO, mockRedis, l)
	return system, mockIO, mockRedis
}

// ===== Basic Construction Tests =====

func TestNewVehicleSystem(t *testing.T) {
	system, mockIO, mockRedis := newTestVehicleSystem()

	if system == nil {
		t.Fatal("NewVehicleSystem returned nil")
	}
	if system.io != mockIO {
		t.Error("io not set correctly")
	}
	if system.redis != mockRedis {
		t.Error("redis not set correctly")
	}
	if system.state != types.StateInit {
		t.Errorf("Expected initial state StateInit, got %v", system.state)
	}
}

// ===== Horn Handler Tests =====

func TestHandleHornRequestOn(t *testing.T) {
	system, mockIO, _ := newTestVehicleSystem()

	err := system.handleHornRequest(true)
	if err != nil {
		t.Fatalf("handleHornRequest failed: %v", err)
	}
	if !mockIO.digitalOutputs["horn"] {
		t.Error("Expected horn to be on")
	}
}

func TestHandleHornRequestOff(t *testing.T) {
	system, mockIO, _ := newTestVehicleSystem()
	mockIO.digitalOutputs["horn"] = true

	err := system.handleHornRequest(false)
	if err != nil {
		t.Fatalf("handleHornRequest failed: %v", err)
	}
	if mockIO.digitalOutputs["horn"] {
		t.Error("Expected horn to be off")
	}
}

// ===== Blinker Handler Tests =====

func TestHandleBlinkerRequestOff(t *testing.T) {
	system, mockIO, mockRedis := newTestVehicleSystem()

	err := system.handleBlinkerRequest("off")
	if err != nil {
		t.Fatalf("handleBlinkerRequest failed: %v", err)
	}
	if system.blinkerState != BlinkerOff {
		t.Errorf("Expected BlinkerOff, got %v", system.blinkerState)
	}
	if len(mockIO.pwmCues) != 1 || mockIO.pwmCues[0] != 9 {
		t.Errorf("Expected PWM cue 9 (LED_BLINK_NONE), got %v", mockIO.pwmCues)
	}
	if len(mockRedis.setBlinkerStates) != 1 || mockRedis.setBlinkerStates[0] != "off" {
		t.Errorf("Expected blinker state 'off' published, got %v", mockRedis.setBlinkerStates)
	}
}

func TestHandleBlinkerRequestLeft(t *testing.T) {
	system, _, _ := newTestVehicleSystem()

	err := system.handleBlinkerRequest("left")
	if err != nil {
		t.Fatalf("handleBlinkerRequest failed: %v", err)
	}
	if system.blinkerState != BlinkerLeft {
		t.Errorf("Expected BlinkerLeft, got %v", system.blinkerState)
	}
	if system.blinkerStopChan == nil {
		t.Error("Expected blinker goroutine to be started")
	}
}

func TestHandleBlinkerRequestRight(t *testing.T) {
	system, _, _ := newTestVehicleSystem()

	err := system.handleBlinkerRequest("right")
	if err != nil {
		t.Fatalf("handleBlinkerRequest failed: %v", err)
	}
	if system.blinkerState != BlinkerRight {
		t.Errorf("Expected BlinkerRight, got %v", system.blinkerState)
	}
}

func TestHandleBlinkerRequestBoth(t *testing.T) {
	system, _, _ := newTestVehicleSystem()

	err := system.handleBlinkerRequest("both")
	if err != nil {
		t.Fatalf("handleBlinkerRequest failed: %v", err)
	}
	if system.blinkerState != BlinkerBoth {
		t.Errorf("Expected BlinkerBoth, got %v", system.blinkerState)
	}
}

func TestHandleBlinkerRequestInvalid(t *testing.T) {
	system, _, _ := newTestVehicleSystem()

	err := system.handleBlinkerRequest("invalid")
	if err == nil {
		t.Error("Expected error for invalid blinker state")
	}
}

func TestHandleBlinkerRequestStopsPrevious(t *testing.T) {
	system, _, _ := newTestVehicleSystem()

	_ = system.handleBlinkerRequest("left")
	firstChan := system.blinkerStopChan

	_ = system.handleBlinkerRequest("right")
	if system.blinkerStopChan == firstChan {
		t.Error("Expected new stop channel")
	}
}

// ===== LED Handler Tests =====

func TestHandleLedCueRequest(t *testing.T) {
	system, mockIO, _ := newTestVehicleSystem()

	err := system.handleLedCueRequest(5)
	if err != nil {
		t.Fatalf("handleLedCueRequest failed: %v", err)
	}
	if len(mockIO.pwmCues) != 1 || mockIO.pwmCues[0] != 5 {
		t.Errorf("Expected PWM cue 5, got %v", mockIO.pwmCues)
	}
}

func TestHandleLedFadeRequest(t *testing.T) {
	system, mockIO, _ := newTestVehicleSystem()

	err := system.handleLedFadeRequest(2, 7)
	if err != nil {
		t.Fatalf("handleLedFadeRequest failed: %v", err)
	}
	if len(mockIO.pwmFades) != 1 || mockIO.pwmFades[0].ch != 2 || mockIO.pwmFades[0].idx != 7 {
		t.Errorf("Expected PWM fade (ch=2, idx=7), got %v", mockIO.pwmFades)
	}
}

// ===== Settings Handler Tests =====

func TestHandleSettingsUpdateBrakeHibernationEnabled(t *testing.T) {
	system, _, mockRedis := newTestVehicleSystem()
	mockRedis.hashFieldValue = "enabled"

	err := system.handleSettingsUpdate("scooter.brake-hibernation")
	if err != nil {
		t.Fatalf("handleSettingsUpdate failed: %v", err)
	}

	system.mu.RLock()
	enabled := system.brakeHibernationEnabled
	system.mu.RUnlock()

	if !enabled {
		t.Error("Expected brake hibernation enabled")
	}
}

func TestHandleSettingsUpdateBrakeHibernationDisabled(t *testing.T) {
	system, _, mockRedis := newTestVehicleSystem()
	system.brakeHibernationEnabled = true
	mockRedis.hashFieldValue = "disabled"

	err := system.handleSettingsUpdate("scooter.brake-hibernation")
	if err != nil {
		t.Fatalf("handleSettingsUpdate failed: %v", err)
	}

	system.mu.RLock()
	enabled := system.brakeHibernationEnabled
	system.mu.RUnlock()

	if enabled {
		t.Error("Expected brake hibernation disabled")
	}
}

func TestHandleSettingsUpdateAutoStandby(t *testing.T) {
	system, _, mockRedis := newTestVehicleSystem()
	mockRedis.hashFieldValue = "300"

	err := system.handleSettingsUpdate("scooter.auto-standby-seconds")
	if err != nil {
		t.Fatalf("handleSettingsUpdate failed: %v", err)
	}

	system.mu.RLock()
	seconds := system.autoStandbySeconds
	system.mu.RUnlock()

	if seconds != 300 {
		t.Errorf("Expected 300 seconds, got %d", seconds)
	}
}

func TestHandleSettingsUpdateAutoStandbyInvalid(t *testing.T) {
	system, _, mockRedis := newTestVehicleSystem()
	mockRedis.hashFieldValue = "not-a-number"

	err := system.handleSettingsUpdate("scooter.auto-standby-seconds")
	if err == nil {
		t.Error("Expected error for invalid value")
	}
}

func TestHandleSettingsUpdateUnknown(t *testing.T) {
	system, _, _ := newTestVehicleSystem()

	err := system.handleSettingsUpdate("scooter.unknown")
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
}

// ===== Hardware Request Handler Tests =====

func TestHandleHardwareRequestInvalidFormat(t *testing.T) {
	system, _, _ := newTestVehicleSystem()

	err := system.handleHardwareRequest("invalid")
	if err == nil {
		t.Error("Expected error for invalid format")
	}
}

func TestHandleHardwareRequestInvalidComponent(t *testing.T) {
	system, _, _ := newTestVehicleSystem()

	err := system.handleHardwareRequest("unknown:on")
	if err == nil {
		t.Error("Expected error for invalid component")
	}
}

func TestHandleHardwareRequestDashboardOffDuringUpdate(t *testing.T) {
	system, _, _ := newTestVehicleSystem()
	system.dbcUpdating = true

	err := system.handleHardwareRequest("dashboard:off")
	if err == nil {
		t.Error("Expected error when turning off dashboard during DBC update")
	}
}

func TestHandleHardwareRequestDashboardOffForce(t *testing.T) {
	system, _, _ := newTestVehicleSystem()
	system.dbcUpdating = true

	err := system.handleHardwareRequest("dashboard:off:force")
	if err != nil {
		t.Fatalf("Force flag should override DBC update check: %v", err)
	}
}

// ===== Update Request Handler Tests =====

func TestHandleUpdateRequestStart(t *testing.T) {
	system, _, _ := newTestVehicleSystem()

	err := system.handleUpdateRequest("start")
	if err != nil {
		t.Fatalf("handleUpdateRequest failed: %v", err)
	}
}

func TestHandleUpdateRequestStartDbc(t *testing.T) {
	system, _, mockRedis := newTestVehicleSystem()

	err := system.handleUpdateRequest("start-dbc")
	if err != nil {
		t.Fatalf("handleUpdateRequest failed: %v", err)
	}

	system.mu.RLock()
	updating := system.dbcUpdating
	system.mu.RUnlock()

	if !updating {
		t.Error("Expected dbcUpdating to be true")
	}
	if !mockRedis.dbcUpdating {
		t.Error("Expected DBC updating persisted to Redis")
	}
}

func TestHandleUpdateRequestCompleteDbc(t *testing.T) {
	system, _, mockRedis := newTestVehicleSystem()
	system.dbcUpdating = true
	mockRedis.dbcUpdating = true

	err := system.handleUpdateRequest("complete-dbc")
	if err != nil {
		t.Fatalf("handleUpdateRequest failed: %v", err)
	}

	system.mu.RLock()
	updating := system.dbcUpdating
	system.mu.RUnlock()

	if updating {
		t.Error("Expected dbcUpdating to be false")
	}
}

func TestHandleUpdateRequestComplete(t *testing.T) {
	system, _, _ := newTestVehicleSystem()

	err := system.handleUpdateRequest("complete")
	if err != nil {
		t.Fatalf("handleUpdateRequest failed: %v", err)
	}
}

func TestHandleUpdateRequestInvalid(t *testing.T) {
	system, _, _ := newTestVehicleSystem()

	err := system.handleUpdateRequest("invalid")
	if err == nil {
		t.Error("Expected error for invalid action")
	}
}

// ===== Hardware IO Mock Tests =====

func TestHardwareIODigitalOutput(t *testing.T) {
	_, mockIO, _ := newTestVehicleSystem()

	err := mockIO.WriteDigitalOutput("test_pin", true)
	if err != nil {
		t.Fatalf("WriteDigitalOutput failed: %v", err)
	}
	if !mockIO.digitalOutputs["test_pin"] {
		t.Error("Digital output not set")
	}
}

func TestHardwareIOInputCallback(t *testing.T) {
	_, mockIO, _ := newTestVehicleSystem()

	called := false
	var receivedValue bool

	mockIO.RegisterInputCallback("test_input", func(channel string, value bool) error {
		called = true
		receivedValue = value
		return nil
	})

	err := mockIO.SimulateInput("test_input", true)
	if err != nil {
		t.Fatalf("SimulateInput failed: %v", err)
	}
	if !called {
		t.Error("Callback not called")
	}
	if !receivedValue {
		t.Error("Wrong value received")
	}
}

// ===== State Transition Tests =====
// These tests verify the engine brake behavior during state transitions.
// This was a regression where updateEngineBrake() was called at the end of
// state entry actions, but it read v.state which hadn't been updated yet,
// causing inverted engine brake behavior.

// initTestFSM initializes the FSM for a test system
func initTestFSM(t *testing.T, system *VehicleSystem) {
	t.Helper()
	if err := system.initFSM(context.Background()); err != nil {
		t.Fatalf("Failed to initialize FSM: %v", err)
	}
}

func TestEnterReadyToDrive_EngineBrakeDisabled(t *testing.T) {
	system, mockIO, _ := newTestVehicleSystem()

	// Set up initial conditions: kickstand up, no brakes pressed
	mockIO.digitalInputs["kickstand"] = false        // up
	mockIO.digitalInputs["brake_left"] = false       // not pressed
	mockIO.digitalInputs["brake_right"] = false      // not pressed
	mockIO.digitalInputs["handlebar_position"] = false
	mockIO.digitalInputs["seatbox_lock_sensor"] = true // closed

	initTestFSM(t, system)

	// Start from Parked state
	if err := system.machine.SetState(fsm.StateParked); err != nil {
		t.Fatalf("Failed to set initial state: %v", err)
	}
	system.initialized = true
	system.dashboardReady = true

	// Clear any previous outputs
	mockIO.digitalOutputs = make(map[string]bool)

	// Trigger transition to ReadyToDrive via kickstand up event
	system.machine.Send(librefsm.Event{ID: fsm.EvKickstandUp})

	// Allow state transition to complete
	time.Sleep(50 * time.Millisecond)

	// Verify we're in ready-to-drive
	if system.getCurrentState() != types.StateReadyToDrive {
		t.Errorf("Expected state ReadyToDrive, got %v", system.getCurrentState())
	}

	// CRITICAL: engine_brake should be FALSE in ready-to-drive (throttle enabled)
	if mockIO.digitalOutputs["engine_brake"] != false {
		t.Errorf("Engine brake should be FALSE (disabled) in ReadyToDrive with no brakes pressed, got %v",
			mockIO.digitalOutputs["engine_brake"])
	}
}

func TestEnterReadyToDrive_EngineBrakeFollowsBrakes(t *testing.T) {
	system, mockIO, _ := newTestVehicleSystem()

	// Set up initial conditions: kickstand up, LEFT brake pressed
	mockIO.digitalInputs["kickstand"] = false        // up
	mockIO.digitalInputs["brake_left"] = true        // pressed
	mockIO.digitalInputs["brake_right"] = false      // not pressed
	mockIO.digitalInputs["handlebar_position"] = false
	mockIO.digitalInputs["seatbox_lock_sensor"] = true

	initTestFSM(t, system)

	// Start from Parked state
	if err := system.machine.SetState(fsm.StateParked); err != nil {
		t.Fatalf("Failed to set initial state: %v", err)
	}
	system.initialized = true
	system.dashboardReady = true

	// Clear outputs
	mockIO.digitalOutputs = make(map[string]bool)

	// Trigger transition to ReadyToDrive
	system.machine.Send(librefsm.Event{ID: fsm.EvKickstandUp})
	time.Sleep(50 * time.Millisecond)

	// engine_brake should be TRUE when brake lever is pressed
	if mockIO.digitalOutputs["engine_brake"] != true {
		t.Errorf("Engine brake should be TRUE when brake lever is pressed in ReadyToDrive, got %v",
			mockIO.digitalOutputs["engine_brake"])
	}
}

func TestEnterParked_EngineBrakeEnabled(t *testing.T) {
	system, mockIO, _ := newTestVehicleSystem()

	// Set up initial conditions: start with kickstand up, then will put it down
	mockIO.digitalInputs["kickstand"] = false        // up (will change to down)
	mockIO.digitalInputs["brake_left"] = false       // not pressed
	mockIO.digitalInputs["brake_right"] = false      // not pressed
	mockIO.digitalInputs["handlebar_position"] = false
	mockIO.digitalInputs["seatbox_lock_sensor"] = true

	initTestFSM(t, system)

	// Start from ReadyToDrive state
	if err := system.machine.SetState(fsm.StateReadyToDrive); err != nil {
		t.Fatalf("Failed to set initial state: %v", err)
	}
	system.initialized = true
	system.dashboardReady = true
	system.readyToDriveEntryTime = time.Now().Add(-2 * time.Second) // bypass debounce

	// Clear outputs
	mockIO.digitalOutputs = make(map[string]bool)

	// Simulate kickstand going down
	mockIO.digitalInputs["kickstand"] = true // down

	// Trigger transition to Parked
	system.machine.Send(librefsm.Event{ID: fsm.EvKickstandDown})
	time.Sleep(50 * time.Millisecond)

	// Verify we're in parked
	if system.getCurrentState() != types.StateParked {
		t.Errorf("Expected state Parked, got %v", system.getCurrentState())
	}

	// CRITICAL: engine_brake should be TRUE in parked (throttle disabled)
	if mockIO.digitalOutputs["engine_brake"] != true {
		t.Errorf("Engine brake should be TRUE (enabled) in Parked state, got %v",
			mockIO.digitalOutputs["engine_brake"])
	}
}

func TestParkedToReadyToDrive_EngineBrakeTransition(t *testing.T) {
	// This test verifies the full transition cycle that was broken before the fix.
	// The bug caused engine_brake to be inverted after transitions.
	system, mockIO, _ := newTestVehicleSystem()

	mockIO.digitalInputs["kickstand"] = true         // down (parked)
	mockIO.digitalInputs["brake_left"] = false
	mockIO.digitalInputs["brake_right"] = false
	mockIO.digitalInputs["handlebar_position"] = false
	mockIO.digitalInputs["seatbox_lock_sensor"] = true

	initTestFSM(t, system)

	// Start from Parked
	if err := system.machine.SetState(fsm.StateParked); err != nil {
		t.Fatalf("Failed to set initial state: %v", err)
	}
	system.initialized = true
	system.dashboardReady = true

	// Clear and verify engine_brake is TRUE in parked by re-entering
	mockIO.digitalOutputs = make(map[string]bool)
	system.machine.SetState(fsm.StateStandby)
	time.Sleep(20 * time.Millisecond)
	system.dashboardReady = true // Restore after standby clears it
	system.machine.SetState(fsm.StateParked)
	time.Sleep(50 * time.Millisecond)

	if mockIO.digitalOutputs["engine_brake"] != true {
		t.Errorf("Step 1: Engine brake should be TRUE in Parked, got %v", mockIO.digitalOutputs["engine_brake"])
	}

	// Now transition to ReadyToDrive
	mockIO.digitalInputs["kickstand"] = false // up
	mockIO.digitalOutputs = make(map[string]bool)
	system.dashboardReady = true // Ensure dashboard ready for guard

	system.machine.Send(librefsm.Event{ID: fsm.EvKickstandUp})
	time.Sleep(50 * time.Millisecond)

	if system.getCurrentState() != types.StateReadyToDrive {
		t.Fatalf("Expected ReadyToDrive, got %v", system.getCurrentState())
	}

	// CRITICAL CHECK: This was the bug - engine_brake ended up TRUE instead of FALSE
	if mockIO.digitalOutputs["engine_brake"] != false {
		t.Errorf("Step 2: Engine brake should be FALSE in ReadyToDrive (no brakes pressed), got %v",
			mockIO.digitalOutputs["engine_brake"])
	}

	// Now go back to Parked
	mockIO.digitalInputs["kickstand"] = true // down
	system.readyToDriveEntryTime = time.Now().Add(-2 * time.Second)
	mockIO.digitalOutputs = make(map[string]bool)

	system.machine.Send(librefsm.Event{ID: fsm.EvKickstandDown})
	time.Sleep(50 * time.Millisecond)

	if system.getCurrentState() != types.StateParked {
		t.Fatalf("Expected Parked, got %v", system.getCurrentState())
	}

	// CRITICAL CHECK: This was the bug - engine_brake ended up FALSE instead of TRUE
	if mockIO.digitalOutputs["engine_brake"] != true {
		t.Errorf("Step 3: Engine brake should be TRUE in Parked, got %v",
			mockIO.digitalOutputs["engine_brake"])
	}
}

func TestEnterStandby_FromShuttingDown(t *testing.T) {
	system, mockIO, _ := newTestVehicleSystem()

	mockIO.digitalInputs["kickstand"] = true
	mockIO.digitalInputs["brake_left"] = false
	mockIO.digitalInputs["brake_right"] = false
	mockIO.digitalInputs["handlebar_position"] = false
	mockIO.digitalInputs["seatbox_lock_sensor"] = true

	initTestFSM(t, system)

	// Start from ShuttingDown
	if err := system.machine.SetState(fsm.StateShuttingDown); err != nil {
		t.Fatalf("Failed to set initial state: %v", err)
	}
	system.initialized = true

	mockIO.digitalOutputs = make(map[string]bool)

	// Transition to Standby (via timeout event)
	system.machine.Send(librefsm.Event{ID: fsm.EvShutdownTimeout})
	time.Sleep(50 * time.Millisecond)

	if system.getCurrentState() != types.StateStandby {
		t.Errorf("Expected Standby, got %v", system.getCurrentState())
	}
}

func TestEnterShuttingDown_EnginePowerOff(t *testing.T) {
	system, mockIO, _ := newTestVehicleSystem()

	mockIO.digitalInputs["kickstand"] = true
	mockIO.digitalInputs["brake_left"] = false
	mockIO.digitalInputs["brake_right"] = false
	mockIO.digitalInputs["handlebar_position"] = false
	mockIO.digitalInputs["seatbox_lock_sensor"] = true

	initTestFSM(t, system)

	// Start from Parked
	if err := system.machine.SetState(fsm.StateParked); err != nil {
		t.Fatalf("Failed to set initial state: %v", err)
	}
	system.initialized = true

	// Set engine power on initially
	mockIO.digitalOutputs["engine_power"] = true

	// Trigger shutdown via lock event
	system.machine.Send(librefsm.Event{ID: fsm.EvLock})
	time.Sleep(50 * time.Millisecond)

	if system.getCurrentState() != types.StateShuttingDown {
		t.Errorf("Expected ShuttingDown, got %v", system.getCurrentState())
	}

	// Engine power should be turned off during shutdown
	if mockIO.digitalOutputs["engine_power"] != false {
		t.Errorf("Engine power should be FALSE during shutdown, got %v",
			mockIO.digitalOutputs["engine_power"])
	}
}
