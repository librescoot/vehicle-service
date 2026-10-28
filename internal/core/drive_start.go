package core

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/librescoot/librefsm"
	"vehicle-service/internal/fsm"
	"vehicle-service/internal/types"
)

const (
	driveStartSampleAge   = 500 * time.Millisecond
	driveStartPowerSettle = 1500 * time.Millisecond
	driveStartTimeout     = 30 * time.Second
)

type driveStartGate struct {
	mu            sync.Mutex
	inputs        types.DriveStartInputs
	inputsAt      time.Time
	engineOnAt    time.Time
	railOnAt      time.Time
	motionAt      time.Time
	source        string
	sequence      uint64
	throttleOn    bool
	moving        bool
	requestedAt   time.Time
	timedOut      bool
	entryRejected bool
	cancel        context.CancelFunc
	done          chan struct{}
}

type driveMotion struct {
	Version    int        `json:"version"`
	Source     string     `json:"source"`
	Sequence   uint64     `json:"sequence"`
	ObservedAt *time.Time `json:"observed_at"`
	Valid      *bool      `json:"valid"`
	RPM        *uint16    `json:"rpm"`
	RawSpeed   *uint8     `json:"raw_speed"`
	ThrottleOn *bool      `json:"throttle_on"`
}

func (v *VehicleSystem) receiveDriveMotion(payload string) {
	v.receiveDriveMotionAt(payload, time.Now())
}

func (v *VehicleSystem) receiveDriveMotionAt(payload string, now time.Time) {
	g := &v.driveStart
	g.mu.Lock()
	defer g.mu.Unlock()
	g.motionAt = time.Time{}
	if payload == "" {
		g.source, g.sequence = "", 0
		return
	}
	var e driveMotion
	if len(payload) > 1024 || json.Unmarshal([]byte(payload), &e) != nil {
		return
	}
	source, err := hex.DecodeString(e.Source)
	if err != nil || len(source) != 16 || e.Source != strings.ToLower(e.Source) || e.Version != 1 || e.Sequence == 0 {
		return
	}
	if e.Source == g.source && e.Sequence <= g.sequence {
		return
	}
	g.source, g.sequence = e.Source, e.Sequence
	if e.Valid == nil || !*e.Valid || e.ObservedAt == nil || e.RPM == nil || e.RawSpeed == nil || e.ThrottleOn == nil {
		return
	}
	age := now.Sub(*e.ObservedAt)
	if e.ObservedAt.IsZero() || age < 0 || age > driveStartSampleAge {
		return
	}
	g.motionAt = now.Add(-age)
	g.throttleOn = *e.ThrottleOn
	g.moving = *e.RPM != 0 || *e.RawSpeed != 0
}

func (v *VehicleSystem) noteDriveEnginePower(on bool) {
	g := &v.driveStart
	g.mu.Lock()
	defer g.mu.Unlock()
	if !on {
		g.engineOnAt, g.motionAt = time.Time{}, time.Time{}
	} else if g.engineOnAt.IsZero() {
		g.engineOnAt = time.Now()
	}
}

func (v *VehicleSystem) noteDriveMainPower(on bool) {
	g := &v.driveStart
	g.mu.Lock()
	defer g.mu.Unlock()
	if !on {
		g.railOnAt, g.motionAt = time.Time{}, time.Time{}
	} else if g.railOnAt.IsZero() {
		g.railOnAt = time.Now()
	}
}

func recentDriveSample(at, now time.Time) bool {
	return !at.IsZero() && now.Sub(at) >= 0 && now.Sub(at) <= driveStartSampleAge
}

func (g *driveStartGate) reason(now time.Time) string {
	if g.timedOut || g.entryRejected {
		return "timeout"
	}
	if !recentDriveSample(g.inputsAt, now) || !g.inputs.ActiveBattery {
		return "waiting-battery"
	}
	if g.railOnAt.IsZero() || g.engineOnAt.IsZero() {
		return "waiting-power"
	}
	if !g.inputs.ECUHealthy || g.motionAt.IsZero() ||
		g.motionAt.Before(g.engineOnAt.Add(driveStartPowerSettle)) ||
		g.motionAt.Before(g.railOnAt.Add(driveStartPowerSettle)) {
		return "waiting-ecu"
	}
	if g.throttleOn {
		return "release-throttle"
	}
	if g.moving {
		return "stop-vehicle"
	}
	if !recentDriveSample(g.motionAt, now) {
		return "waiting-ecu"
	}
	return ""
}

// The gate is only consulted on entry to drive. Losing supply during a ride
// neither starts a battery admission request nor automatically re-enables drive.
func (v *VehicleSystem) HasDriveSupply(c *librefsm.Context) bool {
	main, err := v.io.ReadDigitalInput("48v_detect")
	if err != nil || !main || !v.IsSeatboxClosed(c) {
		return false
	}
	g := &v.driveStart
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reason(time.Now()) == ""
}

func (v *VehicleSystem) resetDriveStartRequest() {
	g := &v.driveStart
	g.mu.Lock()
	g.requestedAt, g.timedOut, g.entryRejected = time.Time{}, false, false
	g.mu.Unlock()
}

func (v *VehicleSystem) checkDriveStart(ctx context.Context, now time.Time) string {
	main, mainErr := v.io.ReadDigitalInput("48v_detect")
	state := v.getCurrentStateID()
	var inputs types.DriveStartInputs
	var err error
	switch state {
	case fsm.StateParked, fsm.StateHibernationInitialHold, fsm.StateHibernationAwaitingConfirm,
		fsm.StateHibernationSeatbox, fsm.StateHibernationConfirm:
		inputs, err = v.redis.ReadDriveStartInputs(ctx)
	}
	standDown, standErr := v.io.ReadDigitalInput("kickstand")
	requested := state == fsm.StateParked && standErr == nil && !standDown
	closed := v.IsSeatboxClosed(nil)
	g := &v.driveStart
	g.mu.Lock()
	g.inputs, g.inputsAt = inputs, now
	if err != nil {
		g.inputsAt = time.Time{}
	}
	if mainErr != nil || !main {
		g.railOnAt, g.motionAt = time.Time{}, time.Time{}
	} else if g.railOnAt.IsZero() {
		g.railOnAt = now
	}
	status := ""
	if !requested {
		if (standErr == nil && standDown) || state == fsm.StateStandby || state == fsm.StateShuttingDown ||
			(state == fsm.StateReadyToDrive && !g.entryRejected) {
			g.requestedAt, g.timedOut, g.entryRejected = time.Time{}, false, false
		}
	} else {
		if g.requestedAt.IsZero() {
			g.requestedAt = now
		}
		if now.Sub(g.requestedAt) >= driveStartTimeout {
			g.timedOut = true
		}
		status = g.reason(now)
		if !g.timedOut && !closed {
			status = "close-seatbox"
		}
	}
	g.mu.Unlock()
	if requested && status == "" && v.CanEnterReadyToDrive(nil) {
		v.machine.Send(librefsm.Event{ID: fsm.EvDriveSupplyReady})
	}
	return status
}

func (v *VehicleSystem) startDriveStartMonitor() {
	ctx, cancel := context.WithCancel(context.Background())
	v.driveStart.cancel, v.driveStart.done = cancel, make(chan struct{})
	go func() {
		defer close(v.driveStart.done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		lastStatus := "unpublished"
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				status := v.checkDriveStart(ctx, time.Now())
				if status != lastStatus {
					if err := v.redis.SetDriveStartStatus(status); err != nil {
						v.logger.Warnf("Failed to publish drive start status: %v", err)
					} else {
						lastStatus = status
					}
				}
			}
		}
	}()
}

func (v *VehicleSystem) stopDriveStartMonitor() {
	if v.driveStart.cancel != nil {
		v.driveStart.cancel()
		<-v.driveStart.done
	}
}
