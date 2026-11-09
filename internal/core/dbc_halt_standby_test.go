package core

import (
	"testing"
	"time"

	"vehicle-service/internal/fsm"
	"vehicle-service/internal/types"
)

func standbyHaltFixture(t *testing.T) (*VehicleSystem, *mockHardwareIO, *mockMessagingClient, *[]func()) {
	t.Helper()
	system, mockIO, mockRedis := newTestVehicleSystem()
	initTestFSM(t, system)
	system.initialized = true
	system.dbcUpdating = true
	mockRedis.dbcUpdating = true
	var scheduled []func()
	system.afterFunc = func(d time.Duration, f func()) {
		if d != fsm.ShutdownTimeout {
			t.Errorf("power cut scheduled after %v, want %v", d, fsm.ShutdownTimeout)
		}
		scheduled = append(scheduled, f)
	}
	return system, mockIO, mockRedis, &scheduled
}

// The DBC gets its clean shutdown without the vehicle leaving standby.
func TestCompleteDbcInStandbyHaltsWithoutChangingState(t *testing.T) {
	system, mockIO, mockRedis, scheduled := standbyHaltFixture(t)

	if err := system.handleUpdateRequest("complete-dbc"); err != nil {
		t.Fatalf("complete-dbc: %v", err)
	}
	if got := system.getCurrentState(); got != types.StateStandby {
		t.Fatalf("state = %v, want standby", got)
	}
	if n := countPublishedMessages(mockRedis, "dbc:command", "poweroff"); n != 1 {
		t.Fatalf("poweroff published %d times, want 1", n)
	}
	if !system.dbcPoweroffSent.Load() {
		t.Fatal("halt not recorded")
	}
	if len(*scheduled) != 1 {
		t.Fatalf("power cut scheduled %d times, want 1", len(*scheduled))
	}

	mockIO.setDigitalOutput("dashboard_power", true)
	(*scheduled)[0]()
	if mockIO.getDigitalOutput("dashboard_power") {
		t.Fatal("dashboard power still on after the halt window")
	}
	if system.dbcPoweroffSent.Load() {
		t.Fatal("halt flag not cleared")
	}
	if got := system.getCurrentState(); got != types.StateStandby {
		t.Fatalf("state = %v after the cut, want standby", got)
	}
}

// An unlock in the halt window waits for the power cut, then runs.
func TestUnlockDuringStandbyHaltIsReplayedAfterThePowerCut(t *testing.T) {
	system, mockIO, _, scheduled := standbyHaltFixture(t)
	mockIO.setDigitalInput("kickstand", false)
	mockIO.setDigitalInput("seatbox_lock_sensor", true)

	if err := system.handleUpdateRequest("complete-dbc"); err != nil {
		t.Fatal(err)
	}
	if err := system.handleStateRequest("unlock"); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if !system.pendingUnlock.Load() {
		t.Fatal("unlock not queued")
	}
	if got := system.getCurrentState(); got != types.StateStandby {
		t.Fatalf("state = %v before the cut, want standby", got)
	}

	(*scheduled)[0]()
	deadline := time.Now().Add(time.Second)
	for system.getCurrentState() == types.StateStandby && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := system.getCurrentState(); got != types.StateParked {
		t.Fatalf("state = %v after replay, want parked", got)
	}
	if system.pendingUnlock.Load() {
		t.Fatal("queued unlock not consumed")
	}
}

// A new update holding the dashboard keeps the cut from happening.
func TestStandbyHaltKeepsPowerWhileAnotherUpdateHoldsIt(t *testing.T) {
	system, mockIO, _, scheduled := standbyHaltFixture(t)
	if err := system.handleUpdateRequest("complete-dbc"); err != nil {
		t.Fatal(err)
	}
	if err := system.handleUpdateRequest("start-dbc"); err != nil {
		t.Fatal(err)
	}
	mockIO.setDigitalOutput("dashboard_power", true)
	(*scheduled)[0]()
	if !mockIO.getDigitalOutput("dashboard_power") {
		t.Fatal("power cut while a DBC update was running")
	}
}
