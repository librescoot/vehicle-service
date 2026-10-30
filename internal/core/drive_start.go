package core

import (
	"context"
	"sync"
	"time"

	"github.com/librescoot/librefsm"
	"vehicle-service/internal/fsm"
)

type driveStartGate struct {
	mu            sync.Mutex
	entryRejected bool
	cancel        context.CancelFunc
	done          chan struct{}
}

// Only a new drive entry needs an active battery. This gate does not change an
// ongoing ride or the battery service's activation policy.
func (v *VehicleSystem) HasDriveSupply(c *librefsm.Context) bool {
	v.driveStart.mu.Lock()
	rejected := v.driveStart.entryRejected
	v.driveStart.mu.Unlock()
	if rejected {
		return false
	}
	inputs, err := v.redis.ReadDriveStartInputs(context.Background())
	return err == nil && inputs.ActiveBattery
}

func (v *VehicleSystem) resetDriveStartRequest() {
	v.driveStart.mu.Lock()
	v.driveStart.entryRejected = false
	v.driveStart.mu.Unlock()
}

func (v *VehicleSystem) checkDriveStart(ctx context.Context) string {
	state := v.getCurrentStateID()
	standDown, standErr := v.io.ReadDigitalInput("kickstand")
	if (standErr == nil && standDown) || state == fsm.StateStandby || state == fsm.StateShuttingDown {
		v.resetDriveStartRequest()
	}
	if state != fsm.StateParked || standErr != nil || standDown {
		return ""
	}

	v.driveStart.mu.Lock()
	rejected := v.driveStart.entryRejected
	v.driveStart.mu.Unlock()
	if rejected {
		return "timeout"
	}
	inputs, err := v.redis.ReadDriveStartInputs(ctx)
	if err != nil || !inputs.ActiveBattery {
		return "waiting-battery"
	}
	if v.CanEnterReadyToDrive(nil) {
		v.machine.Send(librefsm.Event{ID: fsm.EvDriveSupplyReady})
	}
	return ""
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
				status := v.checkDriveStart(ctx)
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
