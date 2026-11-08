package core

import (
	"context"
	"fmt"
	"testing"
	"time"

	"vehicle-service/internal/types"
)

type chirpHardware struct {
	HardwareIO
	writes               []bool
	at                   []time.Time
	failOn               bool
	cancelAfterFirstTone context.CancelFunc
}

func (io *chirpHardware) WriteDigitalOutput(channel string, value bool) error {
	if channel == "horn" {
		io.writes = append(io.writes, value)
		io.at = append(io.at, time.Now())
		if !value && len(io.writes) == 2 && io.cancelAfterFirstTone != nil {
			io.cancelAfterFirstTone()
		}
		if value && io.failOn {
			return fmt.Errorf("GPIO write failed")
		}
	}
	return io.HardwareIO.WriteDigitalOutput(channel, value)
}

func TestHornChirpIsBoundedAndLeavesVehicleLocked(t *testing.T) {
	system, io, _ := newTestVehicleSystem()
	recording := &chirpHardware{HardwareIO: io}
	system.io = recording
	started := time.Now()
	if err := system.handleHornChirp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 2*completionChirpTone+completionChirpGap || elapsed > time.Second {
		t.Fatalf("unexpected chirp duration %v", elapsed)
	}
	if io.getDigitalOutput("horn") {
		t.Fatal("horn remained on")
	}
	if len(recording.writes) != 4 || !recording.writes[0] || recording.writes[1] || !recording.writes[2] || recording.writes[3] {
		t.Fatalf("expected two on/off pulses, got %v", recording.writes)
	}
	for index, minimum := range []time.Duration{completionChirpTone, completionChirpGap, completionChirpTone} {
		if elapsed := recording.at[index+1].Sub(recording.at[index]); elapsed < minimum {
			t.Fatalf("phase %d took %v, expected at least %v", index, elapsed, minimum)
		}
	}
	if system.state != types.StateStandby {
		t.Fatalf("vehicle state changed to %s", system.state)
	}
}

func TestHornChirpCancellationTurnsHornOff(t *testing.T) {
	system, io, _ := newTestVehicleSystem()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := system.handleHornChirp(ctx); err == nil {
		t.Fatal("expected cancellation")
	}
	if io.getDigitalOutput("horn") {
		t.Fatal("cancelled chirp left horn on")
	}
}

func TestHornChirpCancellationInGapSuppressesSecondTone(t *testing.T) {
	system, io, _ := newTestVehicleSystem()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recording := &chirpHardware{HardwareIO: io, cancelAfterFirstTone: cancel}
	system.io = recording
	if err := system.handleHornChirp(ctx); err == nil {
		t.Fatal("expected cancellation")
	}
	on := 0
	for _, value := range recording.writes {
		if value {
			on++
		}
	}
	if on != 1 || io.getDigitalOutput("horn") {
		t.Fatalf("cancelled gap produced another tone or left horn on: %v", recording.writes)
	}
}

func TestHornChirpRefusesUnsafeOrDisabledState(t *testing.T) {
	for _, state := range []types.SystemState{types.StateParked, types.StateReadyToDrive} {
		t.Run(string(state), func(t *testing.T) {
			system, io, _ := newTestVehicleSystem()
			system.state = state
			if err := system.handleHornChirp(context.Background()); err == nil {
				t.Fatal("expected state rejection")
			}
			if io.getDigitalOutput("horn") {
				t.Fatal("rejected chirp activated horn")
			}
		})
	}
	for _, mode := range []string{"false", "in-drive"} {
		system, _, _ := newTestVehicleSystem()
		system.hornEnableMode = mode
		if err := system.handleHornChirp(context.Background()); err == nil {
			t.Fatalf("accepted horn mode %s", mode)
		}
	}
}

func TestHornChirpFailedActivationStillLowersOutput(t *testing.T) {
	system, io, _ := newTestVehicleSystem()
	recording := &chirpHardware{HardwareIO: io, failOn: true}
	system.io = recording
	if err := system.handleHornChirp(context.Background()); err == nil {
		t.Fatal("expected GPIO error")
	}
	if len(recording.writes) != 2 || recording.writes[1] {
		t.Fatalf("failed activation did not lower horn: %v", recording.writes)
	}
}

func TestHornChirpAlreadyCancelledDoesNotWrite(t *testing.T) {
	system, io, _ := newTestVehicleSystem()
	recording := &chirpHardware{HardwareIO: io}
	system.io = recording
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := system.handleHornChirp(ctx); err == nil {
		t.Fatal("expected cancellation")
	}
	if len(recording.writes) != 0 {
		t.Fatalf("cancelled request wrote horn: %v", recording.writes)
	}
}

func TestHornChirpRefusesAfterShutdown(t *testing.T) {
	system, _, _ := newTestVehicleSystem()
	system.hornClosed = true
	if err := system.handleHornChirp(context.Background()); err == nil {
		t.Fatal("accepted chirp after shutdown")
	}
}
