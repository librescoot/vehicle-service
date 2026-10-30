//go:build go1.25

package core

import (
	"testing"
	"testing/synctest"
	"time"

	"vehicle-service/internal/fsm"
)

func TestRideStartAcceptsBatteryAfterLongWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		v, io, _ := rideStartSystem(t)
		if got := driveStartTick(v); got != "waiting-battery" {
			t.Fatal(got)
		}
		time.Sleep(time.Minute)
		if got := driveStartTick(v); got != "waiting-battery" {
			t.Fatalf("waiting for a battery became a latched failure: %q", got)
		}
		supplyDriveStart(t, v)
		if got := driveStartTick(v); got != "" {
			t.Fatal(got)
		}
		if v.machine.CurrentState() != fsm.StateReadyToDrive || io.getDigitalOutput("engine_brake") {
			t.Fatal("late battery activation required a new rider request")
		}
	})
}
