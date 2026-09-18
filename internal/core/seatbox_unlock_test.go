package core

import (
	"testing"

	"github.com/librescoot/librefsm"
	"vehicle-service/internal/fsm"
)

// seatboxUnlockSystem builds a started system positioned in state with the
// advanced open-seatbox-on-unlock setting at the requested value. Kickstand is
// down and the handlebar is unlocked unless a test overrides the inputs.
func seatboxUnlockSystem(t *testing.T, state librefsm.StateID, enabled, closed bool) (*VehicleSystem, *mockHardwareIO, *mockMessagingClient) {
	t.Helper()
	v, io, redis := newTestVehicleSystem()
	io.setDigitalInput("kickstand", true)
	io.setDigitalInput("handlebar_lock_sensor", true)
	io.setDigitalInput("seatbox_lock_sensor", closed)
	v.mu.Lock()
	v.openSeatboxOnUnlock = enabled
	v.mu.Unlock()
	initTestFSM(t, v)
	t.Cleanup(func() {
		v.machine.Stop()
		v.cancelHandlebarLock()
		v.cancelHandlebarUnlock()
		v.mu.Lock()
		v.stopMapHoldTimer()
		v.mu.Unlock()
	})
	if err := v.machine.SetState(state); err != nil {
		t.Fatal(err)
	}
	return v, io, redis
}

func TestOpenSeatboxOnUnlock_EnabledOpensFromStandby(t *testing.T) {
	v, _, redis := seatboxUnlockSystem(t, fsm.StateStandby, true, true)

	if err := v.handleStateRequest("unlock"); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if got := v.machine.CurrentState(); got != fsm.StateParked {
		t.Fatalf("state = %s, want parked", got)
	}
	if redis.publishedSeatboxOpened != 1 {
		t.Fatalf("seatbox opens = %d, want 1", redis.publishedSeatboxOpened)
	}
}

func TestOpenSeatboxOnUnlock_DisabledDoesNotOpen(t *testing.T) {
	v, _, redis := seatboxUnlockSystem(t, fsm.StateStandby, false, true)

	if err := v.handleStateRequest("unlock"); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if got := v.machine.CurrentState(); got != fsm.StateParked {
		t.Fatalf("state = %s, want parked", got)
	}
	if redis.publishedSeatboxOpened != 0 {
		t.Fatalf("seatbox opens = %d, want 0", redis.publishedSeatboxOpened)
	}
}

func TestOpenSeatboxOnUnlock_KeycardUnlockOpens(t *testing.T) {
	v, _, redis := seatboxUnlockSystem(t, fsm.StateStandby, true, true)

	sendLockEvent(t, v, fsm.EvKeycardAuth)
	if got := v.machine.CurrentState(); got != fsm.StateParked {
		t.Fatalf("state = %s, want parked", got)
	}
	if redis.publishedSeatboxOpened != 1 {
		t.Fatalf("seatbox opens = %d, want 1", redis.publishedSeatboxOpened)
	}
}

// A keycard tap in parked with the kickstand down locks; it must not pop the
// seatbox on the way into shutdown.
func TestOpenSeatboxOnUnlock_KeycardLockDoesNotOpen(t *testing.T) {
	v, _, redis := seatboxUnlockSystem(t, fsm.StateParked, true, true)

	sendLockEvent(t, v, fsm.EvKeycardAuth)
	if got := v.machine.CurrentState(); got != fsm.StateShuttingDown {
		t.Fatalf("state = %s, want shutting-down", got)
	}
	if redis.publishedSeatboxOpened != 0 {
		t.Fatalf("seatbox opens = %d, want 0", redis.publishedSeatboxOpened)
	}
}

// Raising the kickstand also enters ready-to-drive, but it is not an unlock.
func TestOpenSeatboxOnUnlock_KickstandUpDoesNotOpen(t *testing.T) {
	v, io, redis := seatboxUnlockSystem(t, fsm.StateParked, true, true)
	v.mu.Lock()
	v.dashboardReady = true
	v.handlebarUnlocked = true
	v.mu.Unlock()
	io.setDigitalInput("kickstand", false) // up

	sendLockEvent(t, v, fsm.EvKickstandUp)
	if got := v.machine.CurrentState(); got != fsm.StateReadyToDrive {
		t.Fatalf("state = %s, want ready-to-drive", got)
	}
	if redis.publishedSeatboxOpened != 0 {
		t.Fatalf("seatbox opens = %d, want 0", redis.publishedSeatboxOpened)
	}
}

// An app or cloud unlock while parked and ready enters ready-to-drive, which is
// still an unlock and should open.
func TestOpenSeatboxOnUnlock_EnabledOpensIntoReadyToDrive(t *testing.T) {
	v, io, redis := seatboxUnlockSystem(t, fsm.StateParked, true, true)
	v.mu.Lock()
	v.dashboardReady = true
	v.handlebarUnlocked = true
	v.mu.Unlock()
	io.setDigitalInput("kickstand", false) // up

	if err := v.handleStateRequest("unlock"); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if got := v.machine.CurrentState(); got != fsm.StateReadyToDrive {
		t.Fatalf("state = %s, want ready-to-drive", got)
	}
	if redis.publishedSeatboxOpened != 1 {
		t.Fatalf("seatbox opens = %d, want 1", redis.publishedSeatboxOpened)
	}
}

// The setting takes effect without a restart.
func TestOpenSeatboxOnUnlock_SettingUpdateAppliesImmediately(t *testing.T) {
	v, _, redis := seatboxUnlockSystem(t, fsm.StateStandby, false, true)
	redis.hashFields = map[string]string{
		"settings/scooter.open-seatbox-on-unlock": "true",
	}

	if err := v.handleSettingsUpdate("scooter.open-seatbox-on-unlock"); err != nil {
		t.Fatalf("handleSettingsUpdate: %v", err)
	}
	if err := v.handleStateRequest("unlock"); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if redis.publishedSeatboxOpened != 1 {
		t.Fatalf("seatbox opens = %d, want 1", redis.publishedSeatboxOpened)
	}
}
