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
	publishedStates       []types.SystemState
	setHandlebarPositions []bool
	setBrakeStates        []struct {
		side    string
		pressed bool
	}
	setBlinkerSwitches     []string
	setBlinkerStates       []string
	publishedButtonEvents  []string
	publishedSeatboxOpened int
	sendCommands           []struct{ channel, command string }
	publishedMessages      []struct{ channel, message string }
	mainPowerSets          []bool
	enginePowerSets        []bool

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

func (m *mockMessagingClient) SetCallbacks(callbacks messaging.Callbacks) { m.callbacks = callbacks }
func (m *mockMessagingClient) Connect() error                             { return nil }
func (m *mockMessagingClient) StartListening() error                      { return nil }
func (m *mockMessagingClient) Close() error                               { return nil }
func (m *mockMessagingClient) GetVehicleState() (types.SystemState, error) {
	return m.vehicleState, m.vehicleStateErr
}
func (m *mockMessagingClient) GetDashboardPower() (bool, error) { return m.dashboardPower, nil }
func (m *mockMessagingClient) SetDashboardPower(enabled bool) error {
	m.dashboardPower = enabled
	return nil
}
func (m *mockMessagingClient) SetBacklightEnabled(enabled bool) error { return nil }
func (m *mockMessagingClient) DeleteDashboardReadyFlag() error        { return nil }
func (m *mockMessagingClient) GetDbcUpdating() (bool, error)          { return m.dbcUpdating, nil }
func (m *mockMessagingClient) SetDbcUpdating(updating bool) error {
	m.dbcUpdating = updating
	return nil
}
func (m *mockMessagingClient) GetOtaStatus(component string) (string, error)  { return m.otaStatus, nil }
func (m *mockMessagingClient) SetInhibitor(id, inhibitType, why string) error { return nil }
func (m *mockMessagingClient) RemoveInhibitor(id string) error                { return nil }
func (m *mockMessagingClient) GetHashField(hash, field string) (string, error) {
	return m.hashFieldValue, nil
}
func (m *mockMessagingClient) PublishAutoStandbyDeadline(deadline time.Time) error { return nil }
func (m *mockMessagingClient) ClearAutoStandbyDeadline() error                     { return nil }
func (m *mockMessagingClient) SetHopOnActive(active bool) error                    { return nil }
func (m *mockMessagingClient) SetKickstandState(deployed bool) error               { return nil }
func (m *mockMessagingClient) SetHandlebarLockState(locked bool) error             { return nil }
func (m *mockMessagingClient) SetSeatboxLockState(locked bool) error               { return nil }
func (m *mockMessagingClient) SetHornButton(pressed bool) error                    { return nil }
func (m *mockMessagingClient) SetSeatboxButton(pressed bool) error                 { return nil }
func (m *mockMessagingClient) SetMainPower(on bool) error {
	m.mainPowerSets = append(m.mainPowerSets, on)
	return nil
}
func (m *mockMessagingClient) SetEnginePower(on bool) error {
	m.enginePowerSets = append(m.enginePowerSets, on)
	return nil
}
func (m *mockMessagingClient) SendCommand(channel, command string) error {
	m.sendCommands = append(m.sendCommands, struct{ channel, command string }{channel, command})
	return nil
}
func (m *mockMessagingClient) PublishMessage(channel, message string) error {
	m.publishedMessages = append(m.publishedMessages, struct{ channel, message string }{channel, message})
	return nil
}

func (m *mockMessagingClient) PublishVehicleState(state types.SystemState) error {
	m.publishedStates = append(m.publishedStates, state)
	return nil
}

func (m *mockMessagingClient) SetBrakeState(side string, pressed bool) error {
	m.setBrakeStates = append(m.setBrakeStates, struct {
		side    string
		pressed bool
	}{side, pressed})
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

func (m *mockMessagingClient) SetBlinkerState(state string, _ int64) error {
	m.setBlinkerStates = append(m.setBlinkerStates, state)
	return nil
}

func (m *mockMessagingClient) PublishButtonEvent(event string) error {
	m.publishedButtonEvents = append(m.publishedButtonEvents, event)
	return nil
}

func (m *mockMessagingClient) PublishInputEvent(event string) error {
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
	resyncCount    int
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

func (m *mockHardwareIO) ReadDigitalInputDirect(channel string) (bool, error) {
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

func (m *mockHardwareIO) SetDebounce(channel string, duration time.Duration) {}

func (m *mockHardwareIO) ResyncInputs() error {
	m.resyncCount++
	for channel, cb := range m.inputCallbacks {
		if err := cb(channel, m.digitalInputs[channel]); err != nil {
			return err
		}
	}
	return nil
}

func (m *mockHardwareIO) PlayPwmCue(idx int) error {
	m.pwmCues = append(m.pwmCues, idx)
	return nil
}

func (m *mockHardwareIO) PlayPwmFade(ch int, idx int) error {
	m.pwmFades = append(m.pwmFades, struct{ ch, idx int }{ch, idx})
	return nil
}

func (m *mockHardwareIO) SetDbcLed(color string) error      { return nil }
func (m *mockHardwareIO) SetUsb0Enabled(enabled bool) error { return nil }

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
	mockIO.digitalInputs["kickstand"] = false       // up
	mockIO.digitalInputs["brake_left"] = false      // not pressed
	mockIO.digitalInputs["brake_right"] = false     // not pressed
	mockIO.digitalInputs["handlebar_position"] = false
	mockIO.digitalInputs["handlebar_lock_sensor"] = true // unlocked (pressed)
	mockIO.digitalInputs["seatbox_lock_sensor"] = true   // closed

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
	mockIO.digitalInputs["kickstand"] = false   // up
	mockIO.digitalInputs["brake_left"] = true   // pressed
	mockIO.digitalInputs["brake_right"] = false // not pressed
	mockIO.digitalInputs["handlebar_position"] = false
	mockIO.digitalInputs["handlebar_lock_sensor"] = true // unlocked
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
	mockIO.digitalInputs["kickstand"] = false   // up (will change to down)
	mockIO.digitalInputs["brake_left"] = false  // not pressed
	mockIO.digitalInputs["brake_right"] = false // not pressed
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

	mockIO.digitalInputs["kickstand"] = true // down (parked)
	mockIO.digitalInputs["brake_left"] = false
	mockIO.digitalInputs["brake_right"] = false
	mockIO.digitalInputs["handlebar_position"] = false
	mockIO.digitalInputs["handlebar_lock_sensor"] = true // unlocked
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

func TestKickstandDown_DeferredDuringDebounce(t *testing.T) {
	// When kickstand goes down within the debounce window after entering
	// ready-to-drive, the event should be deferred (not dropped). After the
	// debounce window expires, the GPIO state is re-read and if the kickstand
	// is still down the transition to Parked should fire.
	system, mockIO, _ := newTestVehicleSystem()

	mockIO.digitalInputs["kickstand"] = false // up
	mockIO.digitalInputs["brake_left"] = false
	mockIO.digitalInputs["brake_right"] = false
	mockIO.digitalInputs["handlebar_position"] = false
	mockIO.digitalInputs["handlebar_lock_sensor"] = true // unlocked
	mockIO.digitalInputs["seatbox_lock_sensor"] = true

	initTestFSM(t, system)

	if err := system.machine.SetState(fsm.StateParked); err != nil {
		t.Fatalf("Failed to set initial state: %v", err)
	}
	system.initialized = true
	system.dashboardReady = true

	// Transition to ReadyToDrive via kickstand up
	system.machine.Send(librefsm.Event{ID: fsm.EvKickstandUp})
	time.Sleep(50 * time.Millisecond)

	if system.getCurrentState() != types.StateReadyToDrive {
		t.Fatalf("Expected ReadyToDrive, got %v", system.getCurrentState())
	}

	// Immediately put kickstand down (within debounce window)
	mockIO.digitalInputs["kickstand"] = true // down
	if err := system.handleInputChange("kickstand", true); err != nil {
		t.Fatalf("handleInputChange failed: %v", err)
	}

	// Should still be in ReadyToDrive (debounce deferred the event)
	time.Sleep(50 * time.Millisecond)
	if system.getCurrentState() != types.StateReadyToDrive {
		t.Errorf("Expected ReadyToDrive during debounce, got %v", system.getCurrentState())
	}

	// Wait for debounce window to expire (parkDebounceTime = 1s)
	time.Sleep(1100 * time.Millisecond)

	// Now should have transitioned to Parked via the deferred event
	if system.getCurrentState() != types.StateParked {
		t.Errorf("Expected Parked after deferred kickstand-down, got %v", system.getCurrentState())
	}
}

func TestKickstandDown_DeferredCancelledByKickstandUp(t *testing.T) {
	// When kickstand goes down within the debounce window but is then
	// lifted back up before the window expires, the deferred event should
	// be cancelled (GPIO re-read shows kickstand is up).
	system, mockIO, _ := newTestVehicleSystem()

	mockIO.digitalInputs["kickstand"] = false // up
	mockIO.digitalInputs["brake_left"] = false
	mockIO.digitalInputs["brake_right"] = false
	mockIO.digitalInputs["handlebar_position"] = false
	mockIO.digitalInputs["handlebar_lock_sensor"] = true // unlocked
	mockIO.digitalInputs["seatbox_lock_sensor"] = true

	initTestFSM(t, system)

	if err := system.machine.SetState(fsm.StateParked); err != nil {
		t.Fatalf("Failed to set initial state: %v", err)
	}
	system.initialized = true
	system.dashboardReady = true

	// Transition to ReadyToDrive
	system.machine.Send(librefsm.Event{ID: fsm.EvKickstandUp})
	time.Sleep(50 * time.Millisecond)

	if system.getCurrentState() != types.StateReadyToDrive {
		t.Fatalf("Expected ReadyToDrive, got %v", system.getCurrentState())
	}

	// Kickstand down within debounce window
	mockIO.digitalInputs["kickstand"] = true
	if err := system.handleInputChange("kickstand", true); err != nil {
		t.Fatalf("handleInputChange (down) failed: %v", err)
	}

	// Kickstand back up before debounce expires — this cancels the deferred timer
	// and sends EvKickstandUp immediately (no-op since already in RTD)
	mockIO.digitalInputs["kickstand"] = false
	if err := system.handleInputChange("kickstand", false); err != nil {
		t.Fatalf("handleInputChange (up) failed: %v", err)
	}

	// Wait for the original debounce window to pass
	time.Sleep(1200 * time.Millisecond)

	// Should still be in ReadyToDrive — the deferred event was cancelled
	if system.getCurrentState() != types.StateReadyToDrive {
		t.Errorf("Expected ReadyToDrive (deferred cancelled), got %v", system.getCurrentState())
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

// ===== 48v_detect / main-power publish =====

func TestHandleInputChange_48vDetect_PublishesMainPower(t *testing.T) {
	system, mockIO, mockRedis := newTestVehicleSystem()

	mockIO.digitalInputs["kickstand"] = true
	mockIO.digitalInputs["handlebar_position"] = false
	mockIO.digitalInputs["seatbox_lock_sensor"] = true

	initTestFSM(t, system)
	if err := system.machine.SetState(fsm.StateParked); err != nil {
		t.Fatalf("Failed to set initial state: %v", err)
	}
	system.initialized = true

	if err := system.handleInputChange("48v_detect", true); err != nil {
		t.Fatalf("handleInputChange(48v_detect, true): %v", err)
	}
	if err := system.handleInputChange("48v_detect", false); err != nil {
		t.Fatalf("handleInputChange(48v_detect, false): %v", err)
	}

	if got, want := mockRedis.mainPowerSets, []bool{true, false}; len(got) != len(want) {
		t.Fatalf("mainPowerSets = %v, want %v", got, want)
	}
	for i, v := range []bool{true, false} {
		if mockRedis.mainPowerSets[i] != v {
			t.Errorf("mainPowerSets[%d] = %v, want %v", i, mockRedis.mainPowerSets[i], v)
		}
	}
}

// ===== Shutdown abort / deferred unlock tests =====

func countPublishedMessages(mock *mockMessagingClient, channel, message string) int {
	n := 0
	for _, m := range mock.publishedMessages {
		if m.channel == channel && m.message == message {
			n++
		}
	}
	return n
}

// Plain lock sets dbcPoweroffSent; the unlock handler then defers the unlock
// instead of transitioning back to Parked.
func TestUnlockDuringShutdown_PoweroffSent_Defers(t *testing.T) {
	system, mockIO, mockRedis := newTestVehicleSystem()

	mockIO.digitalInputs["kickstand"] = true
	mockIO.digitalInputs["brake_left"] = false
	mockIO.digitalInputs["brake_right"] = false
	mockIO.digitalInputs["handlebar_position"] = false
	mockIO.digitalInputs["seatbox_lock_sensor"] = true

	initTestFSM(t, system)

	if err := system.machine.SetState(fsm.StateParked); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	system.initialized = true

	// Transition into ShuttingDown via lock.
	system.machine.Send(librefsm.Event{ID: fsm.EvLock})
	time.Sleep(50 * time.Millisecond)

	if system.getCurrentState() != types.StateShuttingDown {
		t.Fatalf("expected ShuttingDown, got %v", system.getCurrentState())
	}
	if !system.dbcPoweroffSent.Load() {
		t.Fatalf("expected dbcPoweroffSent=true after normal lock, got false")
	}
	if countPublishedMessages(mockRedis, "dbc:command", "poweroff") != 1 {
		t.Fatalf("expected exactly one dbc:command poweroff, got %d",
			countPublishedMessages(mockRedis, "dbc:command", "poweroff"))
	}

	// User sends unlock during the 5s window. Handler should defer.
	if err := system.handleStateRequest("unlock"); err != nil {
		t.Fatalf("handleStateRequest unlock: %v", err)
	}
	if !system.pendingUnlock.Load() {
		t.Fatalf("expected pendingUnlock=true after deferred unlock, got false")
	}
	if system.getCurrentState() != types.StateShuttingDown {
		t.Errorf("FSM should stay in ShuttingDown after deferred unlock, got %v",
			system.getCurrentState())
	}
}

// Update-in-progress lock skips the poweroff publish; unlock during shutdown
// should transition straight back to Parked without queuing.
func TestUnlockDuringShutdown_DbcUpdating_AbortsCleanly(t *testing.T) {
	system, mockIO, mockRedis := newTestVehicleSystem()

	mockIO.digitalInputs["kickstand"] = true
	mockIO.digitalInputs["brake_left"] = false
	mockIO.digitalInputs["brake_right"] = false
	mockIO.digitalInputs["handlebar_position"] = false
	mockIO.digitalInputs["seatbox_lock_sensor"] = true

	initTestFSM(t, system)

	if err := system.machine.SetState(fsm.StateParked); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	system.initialized = true

	// Simulate an active DBC update so EnterShuttingDown skips the poweroff.
	system.mu.Lock()
	system.dbcUpdating = true
	system.mu.Unlock()

	system.machine.Send(librefsm.Event{ID: fsm.EvLock})
	time.Sleep(50 * time.Millisecond)

	if system.getCurrentState() != types.StateShuttingDown {
		t.Fatalf("expected ShuttingDown, got %v", system.getCurrentState())
	}
	if system.dbcPoweroffSent.Load() {
		t.Fatalf("expected dbcPoweroffSent=false with update in progress, got true")
	}
	if countPublishedMessages(mockRedis, "dbc:command", "poweroff") != 0 {
		t.Fatalf("expected zero dbc:command poweroff with update in progress, got %d",
			countPublishedMessages(mockRedis, "dbc:command", "poweroff"))
	}

	// Unlock during shutdown aborts back to Parked immediately.
	if err := system.handleStateRequest("unlock"); err != nil {
		t.Fatalf("handleStateRequest unlock: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	if system.getCurrentState() != types.StateParked {
		t.Errorf("expected Parked after safe abort, got %v", system.getCurrentState())
	}
	if system.pendingUnlock.Load() {
		t.Errorf("pendingUnlock should be false on safe abort, got true")
	}
}

// Going all the way through to Standby clears dbcPoweroffSent and — when no
// deferred unlock is queued — does not auto-send EvUnlock.
func TestShutdownToStandby_ClearsFlag_NoAutoUnlock(t *testing.T) {
	system, mockIO, _ := newTestVehicleSystem()

	mockIO.digitalInputs["kickstand"] = true
	mockIO.digitalInputs["brake_left"] = false
	mockIO.digitalInputs["brake_right"] = false
	mockIO.digitalInputs["handlebar_position"] = false
	mockIO.digitalInputs["seatbox_lock_sensor"] = true

	initTestFSM(t, system)

	if err := system.machine.SetState(fsm.StateParked); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	system.initialized = true

	system.machine.Send(librefsm.Event{ID: fsm.EvLock})
	time.Sleep(50 * time.Millisecond)
	if !system.dbcPoweroffSent.Load() {
		t.Fatalf("expected dbcPoweroffSent=true after lock")
	}

	// Drive the shutdown timeout manually.
	system.machine.Send(librefsm.Event{ID: fsm.EvShutdownTimeout})
	time.Sleep(50 * time.Millisecond)

	if system.getCurrentState() != types.StateStandby {
		t.Fatalf("expected Standby, got %v", system.getCurrentState())
	}
	if system.dbcPoweroffSent.Load() {
		t.Errorf("EnterStandby should clear dbcPoweroffSent")
	}
	if system.pendingUnlock.Load() {
		t.Errorf("pendingUnlock should be false when no unlock was queued")
	}
}

// Unlock during a committed shutdown is queued, shutdown timer fires to
// Standby, EnterStandby replays the EvUnlock, and we end up in Parked.
func TestDeferredUnlock_ReplayedFromStandby(t *testing.T) {
	system, mockIO, _ := newTestVehicleSystem()

	mockIO.digitalInputs["kickstand"] = true
	mockIO.digitalInputs["brake_left"] = false
	mockIO.digitalInputs["brake_right"] = false
	mockIO.digitalInputs["handlebar_position"] = false
	mockIO.digitalInputs["seatbox_lock_sensor"] = true

	initTestFSM(t, system)

	if err := system.machine.SetState(fsm.StateParked); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	system.initialized = true
	system.dashboardReady = true

	// Lock -> ShuttingDown -> poweroff sent.
	system.machine.Send(librefsm.Event{ID: fsm.EvLock})
	time.Sleep(50 * time.Millisecond)
	if !system.dbcPoweroffSent.Load() {
		t.Fatalf("setup: expected dbcPoweroffSent=true")
	}

	// Unlock is queued, not forwarded.
	if err := system.handleStateRequest("unlock"); err != nil {
		t.Fatalf("handleStateRequest unlock: %v", err)
	}
	if !system.pendingUnlock.Load() {
		t.Fatalf("unlock should have been queued")
	}

	// Shutdown timer fires; we reach Standby and the queued unlock replays.
	system.machine.Send(librefsm.Event{ID: fsm.EvShutdownTimeout})
	// EnterStandby runs synchronously, but the replay is a goroutine that
	// hits the machine's event channel. Give it time to be processed.
	time.Sleep(100 * time.Millisecond)

	if system.pendingUnlock.Load() {
		t.Errorf("pendingUnlock should be cleared after replay")
	}
	if system.getCurrentState() != types.StateParked {
		t.Errorf("expected Parked after replayed unlock, got %v", system.getCurrentState())
	}
}

// Re-entering ShuttingDown (a second lock during a queued-unlock window)
// drops the queued unlock — user's most recent intent wins.
func TestReEnterShuttingDown_ClearsPendingUnlock(t *testing.T) {
	system, mockIO, _ := newTestVehicleSystem()

	mockIO.digitalInputs["kickstand"] = true
	mockIO.digitalInputs["brake_left"] = false
	mockIO.digitalInputs["brake_right"] = false
	mockIO.digitalInputs["handlebar_position"] = false
	mockIO.digitalInputs["seatbox_lock_sensor"] = true

	initTestFSM(t, system)

	if err := system.machine.SetState(fsm.StateShuttingDown); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	system.initialized = true
	system.pendingUnlock.Store(true) // pretend an unlock was queued

	// Re-enter ShuttingDown (e.g., through a lock command that loops us via
	// a timeout path). Directly re-run EnterShuttingDown by setting state.
	if err := system.machine.SetState(fsm.StateParked); err != nil {
		t.Fatalf("SetState Parked: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if err := system.machine.SetState(fsm.StateShuttingDown); err != nil {
		t.Fatalf("SetState ShuttingDown: %v", err)
	}
	time.Sleep(30 * time.Millisecond)

	if system.pendingUnlock.Load() {
		t.Errorf("EnterShuttingDown should clear pendingUnlock; got true")
	}
}

// EnterParked from ShuttingDown (safe abort path) replays cue 1 or 2 to
// bring the lights back on.
func TestEnterParked_FromShuttingDown_ReplaysLightsCue(t *testing.T) {
	system, mockIO, _ := newTestVehicleSystem()

	mockIO.digitalInputs["kickstand"] = true
	mockIO.digitalInputs["brake_left"] = false
	mockIO.digitalInputs["brake_right"] = false
	mockIO.digitalInputs["handlebar_position"] = false
	mockIO.digitalInputs["seatbox_lock_sensor"] = true

	initTestFSM(t, system)

	if err := system.machine.SetState(fsm.StateShuttingDown); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	system.initialized = true

	// Clear recorded cues accumulated from EnterShuttingDown so we can
	// assert cleanly on what EnterParked emits.
	mockIO.pwmCues = nil

	// Abort path: EvUnlock during ShuttingDown goes back to Parked.
	system.machine.Send(librefsm.Event{ID: fsm.EvUnlock})
	time.Sleep(50 * time.Millisecond)

	if system.getCurrentState() != types.StateParked {
		t.Fatalf("expected Parked, got %v", system.getCurrentState())
	}

	// Brakes off -> cue 1 should be played.
	found := false
	for _, idx := range mockIO.pwmCues {
		if idx == 1 {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected cue 1 (lights-on) to be played on abort, got cues=%v",
			mockIO.pwmCues)
	}
}

// ===== Regression tests for inpin's bug =====
// See bean librescoot-olm0: "no lights, no DBC in parked state, handlebar
// locked" after a lock+unlock sequence via Sunshine/radio-gaga. The root
// cause was the ShuttingDown -> Parked abort path running with a DBC that
// had already been told to halt.

// Exact reproduction of the reported sequence: Parked -> Lock -> Unlock.
// Before the fix, this would land in Parked with dbcPoweroffSent stale
// and no lights-on cue played. After the fix, the unlock is queued and
// replayed from Standby, yielding a clean Parked with cue 1 played.
func TestRegression_LockThenUnlockDuringShutdown_EndsInParkedWithLights(t *testing.T) {
	system, mockIO, mockRedis := newTestVehicleSystem()

	mockIO.digitalInputs["kickstand"] = true
	mockIO.digitalInputs["brake_left"] = false
	mockIO.digitalInputs["brake_right"] = false
	mockIO.digitalInputs["handlebar_position"] = false
	mockIO.digitalInputs["handlebar_lock_sensor"] = true
	mockIO.digitalInputs["seatbox_lock_sensor"] = true

	initTestFSM(t, system)

	if err := system.machine.SetState(fsm.StateParked); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	system.initialized = true
	system.dashboardReady = true

	// Step 1: user sends lock via Sunshine. Vehicle enters ShuttingDown,
	// DBC is told to halt.
	if err := system.handleStateRequest("lock"); err != nil {
		t.Fatalf("handleStateRequest(lock): %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if system.getCurrentState() != types.StateShuttingDown {
		t.Fatalf("after lock, expected ShuttingDown got %v", system.getCurrentState())
	}
	if !system.dbcPoweroffSent.Load() {
		t.Fatalf("after lock, expected dbcPoweroffSent=true")
	}

	// Step 2: user sends unlock via Sunshine, ~2s into the shutdown window.
	// The handler must NOT let the FSM abort back to Parked (DBC is halting).
	if err := system.handleStateRequest("unlock"); err != nil {
		t.Fatalf("handleStateRequest(unlock): %v", err)
	}
	if !system.pendingUnlock.Load() {
		t.Fatalf("unlock during committed shutdown must be queued, not forwarded")
	}
	if system.getCurrentState() != types.StateShuttingDown {
		t.Errorf("after queued unlock, must stay in ShuttingDown, got %v",
			system.getCurrentState())
	}

	// Step 3: shutdown timer fires, we land in Standby, GPIO is cut.
	system.machine.Send(librefsm.Event{ID: fsm.EvShutdownTimeout})
	// Give enough time for EnterStandby to run + the goroutine-dispatched
	// EvUnlock replay to hit the event loop and transition back to Parked.
	time.Sleep(150 * time.Millisecond)

	// Final state: back in Parked with the queued unlock satisfied.
	if system.getCurrentState() != types.StateParked {
		t.Errorf("after replay, expected Parked got %v", system.getCurrentState())
	}
	if system.pendingUnlock.Load() {
		t.Errorf("pendingUnlock must be cleared after replay")
	}
	if system.dbcPoweroffSent.Load() {
		t.Errorf("dbcPoweroffSent must be cleared at Standby entry")
	}

	// Lights must have been turned back on via cue 1 or 2 somewhere in
	// the final EnterParked. (EnterShuttingDown played cue 7/8 off,
	// EnterParked-from-Standby plays cue 1/2 on.)
	lightsOnCue := false
	for _, c := range mockIO.pwmCues {
		if c == 1 || c == 2 {
			lightsOnCue = true
			break
		}
	}
	if !lightsOnCue {
		t.Errorf("expected cue 1 or 2 to be played (lights back on), got cues=%v",
			mockIO.pwmCues)
	}

	// The poweroff publish must have gone out exactly once across the
	// whole sequence — we committed at EnterShuttingDown and didn't
	// duplicate it on the replay path.
	if n := countPublishedMessages(mockRedis, "dbc:command", "poweroff"); n != 1 {
		t.Errorf("expected exactly 1 dbc:command poweroff, got %d", n)
	}
}

// Multiple unlocks during the same shutdown window must queue idempotently —
// EnterStandby replays a single EvUnlock, not a burst.
func TestRegression_MultipleUnlocksDuringShutdown_QueueIdempotently(t *testing.T) {
	system, mockIO, _ := newTestVehicleSystem()

	mockIO.digitalInputs["kickstand"] = true
	mockIO.digitalInputs["handlebar_lock_sensor"] = true
	mockIO.digitalInputs["seatbox_lock_sensor"] = true

	initTestFSM(t, system)

	if err := system.machine.SetState(fsm.StateParked); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	system.initialized = true
	system.dashboardReady = true

	system.machine.Send(librefsm.Event{ID: fsm.EvLock})
	time.Sleep(50 * time.Millisecond)

	// Send unlock three times in a row — all should funnel into pendingUnlock,
	// which is a single-bit latch.
	for i := 0; i < 3; i++ {
		if err := system.handleStateRequest("unlock"); err != nil {
			t.Fatalf("unlock %d: %v", i, err)
		}
	}
	if !system.pendingUnlock.Load() {
		t.Fatalf("pendingUnlock must be true after repeated deferrals")
	}

	// After replay, pendingUnlock is cleared exactly once — the CAS
	// semantics ensure no "spare" queued unlock lingers.
	system.machine.Send(librefsm.Event{ID: fsm.EvShutdownTimeout})
	time.Sleep(150 * time.Millisecond)

	if system.pendingUnlock.Load() {
		t.Errorf("pendingUnlock must be cleared by CAS on replay")
	}
	if system.getCurrentState() != types.StateParked {
		t.Errorf("expected Parked after replay, got %v", system.getCurrentState())
	}
}

// Lock -> queued unlock -> second lock overrides the queue. The user's most
// recent intent wins and we shouldn't wake back up after the second lock.
func TestRegression_SecondLockClearsQueuedUnlock(t *testing.T) {
	system, mockIO, _ := newTestVehicleSystem()

	mockIO.digitalInputs["kickstand"] = true
	mockIO.digitalInputs["handlebar_lock_sensor"] = true
	mockIO.digitalInputs["seatbox_lock_sensor"] = true

	initTestFSM(t, system)

	if err := system.machine.SetState(fsm.StateParked); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	system.initialized = true

	// First lock.
	system.machine.Send(librefsm.Event{ID: fsm.EvLock})
	time.Sleep(50 * time.Millisecond)

	// Queue an unlock.
	if err := system.handleStateRequest("unlock"); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if !system.pendingUnlock.Load() {
		t.Fatalf("setup: expected pendingUnlock=true")
	}

	// Timeout back to Parked (via EvShutdownTimeout -> Standby -> replayed
	// EvUnlock -> Parked). Then the user locks again before timer fires.
	// Simpler repro: re-enter ShuttingDown directly, verifying that fresh
	// entry clears the queue.
	if err := system.machine.SetState(fsm.StateParked); err != nil {
		t.Fatalf("SetState Parked: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if err := system.machine.SetState(fsm.StateShuttingDown); err != nil {
		t.Fatalf("SetState ShuttingDown: %v", err)
	}
	time.Sleep(30 * time.Millisecond)

	if system.pendingUnlock.Load() {
		t.Errorf("second lock intent must clear queued unlock")
	}
}

// An unlock arriving while the vehicle is in Parked (not ShuttingDown)
// must not be queued — it should forward normally. Guards against the
// handler accidentally broadening the queue condition.
func TestRegression_UnlockInParkedIsNotQueued(t *testing.T) {
	system, mockIO, _ := newTestVehicleSystem()

	mockIO.digitalInputs["kickstand"] = true
	mockIO.digitalInputs["handlebar_lock_sensor"] = true
	mockIO.digitalInputs["seatbox_lock_sensor"] = true

	initTestFSM(t, system)

	if err := system.machine.SetState(fsm.StateParked); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	system.initialized = true

	// Poisoning: pretend poweroff was sent (shouldn't matter — we're in
	// Parked, not ShuttingDown).
	system.dbcPoweroffSent.Store(true)

	if err := system.handleStateRequest("unlock"); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if system.pendingUnlock.Load() {
		t.Errorf("unlock in Parked must not be queued")
	}
}

// An unlock arriving in Standby is forwarded as a normal wake — not queued,
// even if dbcPoweroffSent happened to be stale.
func TestRegression_UnlockInStandbyIsNotQueued(t *testing.T) {
	system, mockIO, _ := newTestVehicleSystem()

	mockIO.digitalInputs["kickstand"] = true
	mockIO.digitalInputs["handlebar_lock_sensor"] = true
	mockIO.digitalInputs["seatbox_lock_sensor"] = true

	initTestFSM(t, system)

	if err := system.machine.SetState(fsm.StateStandby); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	system.initialized = true

	// Should never be true in Standby (EnterStandby clears it), but assert
	// defensively that the handler's condition requires BOTH state ==
	// ShuttingDown AND the flag.
	system.dbcPoweroffSent.Store(true)

	if err := system.handleStateRequest("unlock"); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if system.pendingUnlock.Load() {
		t.Errorf("unlock in Standby must not be queued")
	}
	time.Sleep(50 * time.Millisecond)
	if system.getCurrentState() != types.StateParked {
		t.Errorf("unlock from Standby should transition to Parked, got %v",
			system.getCurrentState())
	}
}
