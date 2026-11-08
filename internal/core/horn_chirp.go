package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"vehicle-service/internal/types"
)

const (
	completionChirpTone = 50 * time.Millisecond
	completionChirpGap  = 75 * time.Millisecond
)

func (v *VehicleSystem) handleHornChirp(ctx context.Context) (err error) {
	v.hornMu.Lock()
	defer v.hornMu.Unlock()
	if v.hornClosed {
		return fmt.Errorf("horn controller is shut down")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Keep an unlock from racing this short, locked-only notification. The
	// output is always lowered locally, even if the requesting client vanishes.
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.state != types.StateStandby || v.hornEnableMode != "true" {
		return fmt.Errorf("completion chirp requires stand-by with the horn enabled")
	}
	defer func() { err = errors.Join(err, v.writeOutput("horn", false)) }()
	wait := func(duration time.Duration) error {
		timer := time.NewTimer(duration)
		defer timer.Stop()
		select {
		case <-timer.C:
			return ctx.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for tone := 0; tone < 2; tone++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := v.writeOutput("horn", true); err != nil {
			return err
		}
		if err := wait(completionChirpTone); err != nil {
			return err
		}
		if tone == 0 {
			if err := v.writeOutput("horn", false); err != nil {
				return err
			}
			if err := wait(completionChirpGap); err != nil {
				return err
			}
		}
	}
	return nil
}
