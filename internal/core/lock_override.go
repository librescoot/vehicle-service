package core

import (
	"fmt"
	"github.com/librescoot/librefsm"
	"time"
	"vehicle-service/internal/fsm"
)

func (v *VehicleSystem) handleLockIgnoreSeatboxRequest(deadline time.Time) error {
	if !time.Now().Before(deadline) {
		return fmt.Errorf("expired")
	}
	if v.machine == nil {
		return fmt.Errorf("unavailable")
	}
	r := &fsm.LockIgnoreSeatboxRequest{Deadline: deadline}
	if err := v.machine.SendSync(librefsm.Event{ID: fsm.EvLockIgnoreSeatbox, Payload: r}); err != nil {
		v.logger.Warnf("Seatbox override processing failed: %v", err)
		return fmt.Errorf("processing") // Side effects may have begun; outcome is uncertain.
	}
	if !r.Accepted {
		if !time.Now().Before(deadline) {
			return fmt.Errorf("expired")
		}
		return fmt.Errorf("unsafe-state")
	}
	return nil
}
