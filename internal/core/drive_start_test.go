package core

import (
	"context"
	"errors"
	"testing"

	"github.com/librescoot/librefsm"
	"vehicle-service/internal/fsm"
	"vehicle-service/internal/types"
)

func supplyDriveStart(t *testing.T, v *VehicleSystem) {
	t.Helper()
	v.io.(*mockHardwareIO).setDigitalInput("48v_detect", true)
	r := v.redis.(*mockMessagingClient)
	r.mu.Lock()
	r.driveInputs = types.DriveStartInputs{ActiveBattery: true}
	r.mu.Unlock()
}

func rideStartSystem(t *testing.T) (*VehicleSystem, *mockHardwareIO, *mockMessagingClient) {
	t.Helper()
	v, io, r := lockSemanticsSystem(t, fsm.StateParked, true)
	io.setDigitalInput("kickstand", false)
	v.mu.Lock()
	v.dashboardReady, v.handlebarUnlocked = true, true
	v.mu.Unlock()
	return v, io, r
}

func driveStartTick(v *VehicleSystem) string {
	status := v.checkDriveStart(context.Background())
	_ = v.machine.SendSync(librefsm.Event{ID: "test-barrier"})
	return status
}

func TestRideStartWaitsForSupply(t *testing.T) {
	for _, event := range []librefsm.EventID{fsm.EvKickstandUp, fsm.EvDashboardReady, fsm.EvUnlock, fsm.EvSeatboxButton, fsm.EvDriveSupplyReady} {
		t.Run(string(event), func(t *testing.T) {
			v, io, _ := rideStartSystem(t)
			io.setDigitalInput("brake_left", true)
			io.setDigitalInput("brake_right", true)
			if v.CanEnterReadyToDrive(nil) {
				t.Fatal("guard admitted driving before battery activation")
			}
			_ = v.machine.SendSync(librefsm.Event{ID: event})
			if got := v.machine.CurrentState(); got != fsm.StateParked {
				t.Fatalf("without active supply, state = %s", got)
			}
			if !io.getDigitalOutput("engine_brake") {
				t.Fatal("motor interlock released without a supply")
			}
			if got := driveStartTick(v); got != "waiting-battery" {
				t.Fatalf("waiting status = %q", got)
			}
		})
	}
}

func TestRideStartContinuesAfterBatteryActivation(t *testing.T) {
	v, io, r := rideStartSystem(t)
	_ = v.machine.SendSync(librefsm.Event{ID: fsm.EvKickstandUp})
	if got := driveStartTick(v); got != "waiting-battery" {
		t.Fatal(got)
	}
	r.mu.Lock()
	r.driveInputs.ActiveBattery = true
	r.mu.Unlock()
	// No 48V input, ECU health, motion frame or power-settling delay is supplied.
	if got := driveStartTick(v); got != "" {
		t.Fatal(got)
	}
	if v.machine.CurrentState() != fsm.StateReadyToDrive || io.getDigitalOutput("engine_brake") {
		t.Fatal("active battery did not finish the pending start")
	}
}

func TestRideStartOnlyAddsBatteryCondition(t *testing.T) {
	for name, mutate := range map[string]func(*mockHardwareIO){
		"48V absent": func(io *mockHardwareIO) { io.setDigitalInput("48v_detect", false) },
		"48V read error": func(io *mockHardwareIO) {
			io.inputErrs = map[string]error{"48v_detect": errors.New("read")}
		},
		"seatbox open": func(io *mockHardwareIO) { io.setDigitalInput("seatbox_lock_sensor", false) },
	} {
		t.Run(name, func(t *testing.T) {
			v, io, _ := rideStartSystem(t)
			supplyDriveStart(t, v)
			mutate(io)
			if !v.CanEnterReadyToDrive(nil) {
				t.Fatal("additional condition blocked an active battery")
			}
			driveStartTick(v)
			if v.machine.CurrentState() != fsm.StateReadyToDrive {
				t.Fatal("did not enter drive")
			}
		})
	}
}

func TestRideStartPreservesNormalConditions(t *testing.T) {
	for _, condition := range []string{"kickstand down", "dashboard not ready", "handlebar locked"} {
		t.Run(condition, func(t *testing.T) {
			v, io, _ := rideStartSystem(t)
			supplyDriveStart(t, v)
			switch condition {
			case "kickstand down":
				io.setDigitalInput("kickstand", true)
			case "dashboard not ready":
				v.dashboardReady = false
			case "handlebar locked":
				v.handlebarUnlocked = false
			}
			if v.CanEnterReadyToDrive(nil) {
				t.Fatal("battery condition bypassed a normal drive condition")
			}
			driveStartTick(v)
			if v.machine.CurrentState() != fsm.StateParked {
				t.Fatal("pending start bypassed a normal drive condition")
			}
		})
	}
}

func TestRideStartReadFailureWaitsForBattery(t *testing.T) {
	v, _, r := rideStartSystem(t)
	supplyDriveStart(t, v)
	r.driveInputsErr = errors.New("Redis unavailable")
	if v.HasDriveSupply(nil) || driveStartTick(v) != "waiting-battery" {
		t.Fatal("failed read admitted driving")
	}
	r.driveInputsErr = nil
	driveStartTick(v)
	if v.machine.CurrentState() != fsm.StateReadyToDrive {
		t.Fatal("recovered battery read did not resume start")
	}
}

func TestRideStartDoesNotReactivateDuringRide(t *testing.T) {
	v, io, r := rideStartSystem(t)
	supplyDriveStart(t, v)
	driveStartTick(v)
	io.setDigitalInput("48v_detect", false)
	r.driveInputs = types.DriveStartInputs{}
	if got := driveStartTick(v); got != "" {
		t.Fatal(got)
	}
	if v.machine.CurrentState() != fsm.StateReadyToDrive {
		t.Fatal("start gate changed an ongoing ride")
	}
}

type changingDriveStartIO struct {
	*mockHardwareIO
	change   func()
	released bool
}

func (io *changingDriveStartIO) WriteDigitalOutput(channel string, value bool) error {
	if channel == "engine_power" && value && io.change != nil {
		change := io.change
		io.change = nil
		change()
	}
	io.mu.Lock()
	if channel == "engine_brake" && !value {
		io.released = true
	}
	io.mu.Unlock()
	return io.mockHardwareIO.WriteDigitalOutput(channel, value)
}

func TestRideStartRechecksBatteryBeforeReleasingMotor(t *testing.T) {
	for _, brakeFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "battery became inactive", true: "interlock write failure"}[brakeFails], func(t *testing.T) {
			v, io, r := rideStartSystem(t)
			supplyDriveStart(t, v)
			changing := &changingDriveStartIO{mockHardwareIO: io}
			changing.change = func() {
				r.mu.Lock()
				r.driveInputs.ActiveBattery = false
				r.mu.Unlock()
				if brakeFails {
					io.mu.Lock()
					io.outputErrs = map[string]error{"engine_brake": errors.New("write")}
					io.mu.Unlock()
				}
			}
			v.io = changing
			_ = v.machine.SendSync(librefsm.Event{ID: fsm.EvKickstandUp})
			_ = v.machine.SendSync(librefsm.Event{ID: "test-barrier"})
			if v.machine.CurrentState() != fsm.StateParked {
				t.Fatal("did not cancel rejected entry")
			}
			io.mu.Lock()
			released := changing.released
			io.mu.Unlock()
			if released {
				t.Fatal("released motor without an active battery")
			}
			if brakeFails && io.getDigitalOutput("engine_power") {
				t.Fatal("failed interlock left engine powered")
			}
			if got := driveStartTick(v); got != "waiting-battery" {
				t.Fatal(got)
			}
			if !brakeFails {
				r.mu.Lock()
				r.driveInputs.ActiveBattery = true
				r.mu.Unlock()
				driveStartTick(v)
				if v.machine.CurrentState() != fsm.StateReadyToDrive {
					t.Fatal("battery recovery required an extra rider request")
				}
			}
		})
	}
}

func TestRideStartHibernationCancellationWaitsForSupply(t *testing.T) {
	v, _, _ := rideStartSystem(t)
	if err := v.machine.SetState(fsm.StateHibernationInitialHold); err != nil {
		t.Fatal(err)
	}
	sendLockEvent(t, v, fsm.EvKickstandUp)
	if v.machine.CurrentState() != fsm.StateParked {
		t.Fatal("hibernation cancellation bypassed battery gate")
	}
}

func TestRideStartPendingRequestCannotOverrideLock(t *testing.T) {
	v, _, _ := rideStartSystem(t)
	driveStartTick(v)
	sendLockEvent(t, v, fsm.EvForceLock)
	supplyDriveStart(t, v)
	if got := driveStartTick(v); got != "" {
		t.Fatal(got)
	}
	if v.machine.CurrentState() != fsm.StateShuttingDown {
		t.Fatal("pending start overrode lock")
	}
}

func TestRideStartManualOverrideKeepsPowerGate(t *testing.T) {
	v, io, _ := rideStartSystem(t)
	v.dashboardReady = false
	io.setDigitalInput("brake_left", true)
	io.setDigitalInput("brake_right", true)
	_ = v.machine.SendSync(librefsm.Event{ID: fsm.EvSeatboxButton})
	if v.machine.CurrentState() != fsm.StateParked {
		t.Fatal("override bypassed missing battery")
	}
	supplyDriveStart(t, v)
	sendLockEvent(t, v, fsm.EvSeatboxButton)
	if v.machine.CurrentState() != fsm.StateReadyToDrive || !io.getDigitalOutput("engine_brake") {
		t.Fatal("manual override did not preserve held brakes")
	}
}

func TestRideStartUnpoweredRestoreRequiresNewIntent(t *testing.T) {
	v, io, _ := rideStartSystem(t)
	v.restoreFSMState(types.StateReadyToDrive)
	supplyDriveStart(t, v)
	if got := driveStartTick(v); got != "timeout" {
		t.Fatalf("restored start status = %q", got)
	}
	if v.machine.CurrentState() != fsm.StateParked || !io.getDigitalOutput("engine_brake") {
		t.Fatal("power returning after restart automatically enabled driving")
	}
	io.setDigitalInput("kickstand", true)
	driveStartTick(v)
	io.setDigitalInput("kickstand", false)
	driveStartTick(v)
	if v.machine.CurrentState() != fsm.StateReadyToDrive {
		t.Fatal("fresh rider request did not enable driving")
	}
}

func TestRideStartRestoreWithoutSupplyReturnsToParked(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "inactive", true: "active"}[active], func(t *testing.T) {
			v, io, r := rideStartSystem(t)
			io.setDigitalInput("48v_detect", active)
			r.driveInputs.ActiveBattery = active
			v.restoreFSMState(types.StateReadyToDrive)
			want := fsm.StateParked
			if active {
				want = fsm.StateReadyToDrive
			}
			if got := v.machine.CurrentState(); got != want {
				t.Fatalf("state=%s want=%s", got, want)
			}
		})
	}
}
