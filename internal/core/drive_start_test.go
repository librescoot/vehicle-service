package core

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/librescoot/librefsm"
	"vehicle-service/internal/fsm"
	"vehicle-service/internal/types"
)

func driveMotionPayload(now time.Time, seq uint64, throttle bool, rpm uint16) string {
	valid, speed := true, uint8(0)
	b, _ := json.Marshal(driveMotion{Version: 1, Source: "00112233445566778899aabbccddeeff", Sequence: seq,
		ObservedAt: &now, Valid: &valid, RPM: &rpm, RawSpeed: &speed, ThrottleOn: &throttle})
	return string(b)
}

func supplyDriveStart(t *testing.T, v *VehicleSystem) {
	t.Helper()
	io := v.io.(*mockHardwareIO)
	io.setDigitalInput("48v_detect", true)
	r := v.redis.(*mockMessagingClient)
	r.mu.Lock()
	r.driveInputs = types.DriveStartInputs{ActiveBattery: true, ECUHealthy: true}
	r.mu.Unlock()
	now := time.Now()
	v.driveStart.mu.Lock()
	v.driveStart.inputs, v.driveStart.inputsAt = r.driveInputs, now
	v.driveStart.engineOnAt, v.driveStart.railOnAt = now.Add(-2*time.Second), now.Add(-2*time.Second)
	v.driveStart.mu.Unlock()
	v.receiveDriveMotionAt(driveMotionPayload(now, 1, false, 0), now)
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
	status := v.checkDriveStart(context.Background(), time.Now())
	// Drain the asynchronous retry event before inspecting state/outputs.
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
	r.driveInputs = types.DriveStartInputs{ActiveBattery: true, ECUHealthy: true}
	if got := driveStartTick(v); got != "waiting-power" {
		t.Fatal(got)
	}
	io.setDigitalInput("48v_detect", true)
	if got := driveStartTick(v); got != "waiting-ecu" {
		t.Fatal(got)
	}
	if v.machine.CurrentState() != fsm.StateParked || !io.getDigitalOutput("engine_brake") {
		t.Fatal("enabled drive before power and live ECU evidence were ready")
	}
	supplyDriveStart(t, v)
	if got := driveStartTick(v); got != "" {
		t.Fatal(got)
	}
	if v.machine.CurrentState() != fsm.StateReadyToDrive || io.getDigitalOutput("engine_brake") {
		t.Fatal("did not finish pending start after activation")
	}
}

func TestRideStartRequiresFreshReleasedThrottle(t *testing.T) {
	v, io, _ := rideStartSystem(t)
	supplyDriveStart(t, v)
	now := time.Now()
	v.receiveDriveMotionAt(driveMotionPayload(now, 2, true, 0), now)
	if got := driveStartTick(v); got != "release-throttle" {
		t.Fatal(got)
	}
	if v.machine.CurrentState() != fsm.StateParked || !io.getDigitalOutput("engine_brake") {
		t.Fatal("held throttle released the motor interlock")
	}
	now = time.Now()
	v.receiveDriveMotionAt(driveMotionPayload(now, 3, false, 0), now)
	driveStartTick(v)
	if v.machine.CurrentState() != fsm.StateReadyToDrive {
		t.Fatal("fresh released throttle did not finish pending start")
	}
}

func TestRideStartRejectsUnusableEvidence(t *testing.T) {
	cases := map[string]func(*VehicleSystem, *mockHardwareIO, *mockMessagingClient){
		"48V absent": func(v *VehicleSystem, io *mockHardwareIO, r *mockMessagingClient) {
			io.setDigitalInput("48v_detect", false)
		},
		"48V read error": func(v *VehicleSystem, io *mockHardwareIO, r *mockMessagingClient) {
			io.inputErrs = map[string]error{"48v_detect": errors.New("read")}
		},
		"seatbox open": func(v *VehicleSystem, io *mockHardwareIO, r *mockMessagingClient) {
			io.setDigitalInput("seatbox_lock_sensor", false)
		},
		"no active pack": func(v *VehicleSystem, io *mockHardwareIO, r *mockMessagingClient) {
			v.driveStart.inputs.ActiveBattery = false
		},
		"ECU fault": func(v *VehicleSystem, io *mockHardwareIO, r *mockMessagingClient) {
			v.driveStart.inputs.ECUHealthy = false
		},
		"expired battery read": func(v *VehicleSystem, io *mockHardwareIO, r *mockMessagingClient) {
			v.driveStart.inputsAt = time.Now().Add(-time.Second)
		},
		"missing motion":   func(v *VehicleSystem, io *mockHardwareIO, r *mockMessagingClient) { v.receiveDriveMotion("") },
		"malformed motion": func(v *VehicleSystem, io *mockHardwareIO, r *mockMessagingClient) { v.receiveDriveMotion("{bad") },
		"omitted zero fields": func(v *VehicleSystem, io *mockHardwareIO, r *mockMessagingClient) {
			v.receiveDriveMotion(`{"version":1,"source":"00112233445566778899aabbccddeeff","sequence":2,"valid":true}`)
		},
		"old motion": func(v *VehicleSystem, io *mockHardwareIO, r *mockMessagingClient) {
			v.receiveDriveMotion(driveMotionPayload(time.Now().Add(-time.Second), 2, false, 0))
		},
		"future motion": func(v *VehicleSystem, io *mockHardwareIO, r *mockMessagingClient) {
			v.receiveDriveMotion(driveMotionPayload(time.Now().Add(time.Second), 2, false, 0))
		},
		"replayed motion": func(v *VehicleSystem, io *mockHardwareIO, r *mockMessagingClient) {
			v.receiveDriveMotion(driveMotionPayload(time.Now(), 1, false, 0))
		},
		"moving wheel": func(v *VehicleSystem, io *mockHardwareIO, r *mockMessagingClient) {
			v.receiveDriveMotion(driveMotionPayload(time.Now(), 2, false, 100))
		},
		"power still settling": func(v *VehicleSystem, io *mockHardwareIO, r *mockMessagingClient) {
			v.driveStart.engineOnAt = time.Now()
		},
		"brief rail loss": func(v *VehicleSystem, io *mockHardwareIO, r *mockMessagingClient) {
			v.noteDriveMainPower(false)
			v.noteDriveMainPower(true)
		},
		"brief engine power loss": func(v *VehicleSystem, io *mockHardwareIO, r *mockMessagingClient) {
			v.noteDriveEnginePower(false)
			v.noteDriveEnginePower(true)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			v, io, r := rideStartSystem(t)
			supplyDriveStart(t, v)
			if !v.HasDriveSupply(nil) {
				t.Fatal("invalid fixture")
			}
			mutate(v, io, r)
			if v.HasDriveSupply(nil) {
				t.Fatal("unsafe evidence admitted drive")
			}
		})
	}
}

func TestRideStartTimeoutRequiresKickstandCycle(t *testing.T) {
	v, io, _ := rideStartSystem(t)
	v.driveStart.requestedAt = time.Now().Add(-driveStartTimeout)
	if got := driveStartTick(v); got != "timeout" {
		t.Fatal(got)
	}
	supplyDriveStart(t, v)
	if got := driveStartTick(v); got != "timeout" {
		t.Fatal(got)
	}
	if v.machine.CurrentState() != fsm.StateParked {
		t.Fatal("late activation bypassed timeout")
	}
	io.setDigitalInput("kickstand", true)
	if err := v.handleInputChange("kickstand", true); err != nil {
		t.Fatal(err)
	}
	driveStartTick(v)
	io.setDigitalInput("kickstand", false)
	driveStartTick(v)
	if v.machine.CurrentState() != fsm.StateReadyToDrive {
		t.Fatal("new start request was not admitted")
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

func TestRideStartRechecksBeforeReleasingMotor(t *testing.T) {
	for _, brakeFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "new throttle input", true: "interlock write failure"}[brakeFails], func(t *testing.T) {
			v, io, _ := rideStartSystem(t)
			supplyDriveStart(t, v)
			changing := &changingDriveStartIO{mockHardwareIO: io}
			changing.change = func() {
				v.receiveDriveMotion(driveMotionPayload(time.Now(), 2, true, 0))
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
				t.Fatal("released motor despite newer throttle input")
			}
			if brakeFails && io.getDigitalOutput("engine_power") {
				t.Fatal("failed interlock left engine powered")
			}
			if got := driveStartTick(v); got != "timeout" {
				t.Fatal(got)
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
		t.Fatal("hibernation cancellation bypassed power gate")
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
		t.Fatal("override bypassed missing supply")
	}
	supplyDriveStart(t, v)
	sendLockEvent(t, v, fsm.EvSeatboxButton)
	if v.machine.CurrentState() != fsm.StateReadyToDrive || !io.getDigitalOutput("engine_brake") {
		t.Fatal("powered manual override did not preserve held brakes")
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
