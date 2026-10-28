package core

import (
	"fmt"
	"reflect"
	"testing"

	"vehicle-service/internal/types"
)

type standbySnapshotClient struct {
	*mockMessagingClient
	values map[string]bool
}

func (m *standbySnapshotClient) SetHornButton(v bool) error    { m.values["horn"] = v; return nil }
func (m *standbySnapshotClient) SetSeatboxButton(v bool) error { m.values["seatbox"] = v; return nil }
func (m *standbySnapshotClient) SetBrakeState(side string, v bool) error {
	m.values["brake:"+side] = v
	return nil
}

func TestStandbyButtonsUpdateSnapshotsWithoutHardwareActions(t *testing.T) {
	system, io, redis := newTestVehicleSystem()
	recorder := &standbySnapshotClient{mockMessagingClient: redis, values: make(map[string]bool)}
	system.redis = recorder
	for _, tc := range []struct{ channel, key, event string }{
		{"horn_button", "horn", "horn"}, {"seatbox_button", "seatbox", "seatbox"},
		{"brake_left", "brake:left", "brake:left"}, {"brake_right", "brake:right", "brake:right"},
	} {
		for _, v := range []bool{true, false} {
			if err := system.handleInputChange(tc.channel, v); err != nil {
				t.Fatal(err)
			}
			if got, ok := recorder.values[tc.key]; !ok || got != v {
				t.Fatalf("%s snapshot = %v, %v", tc.key, got, ok)
			}
			edge := "off"
			if v {
				edge = "on"
			}
			want := fmt.Sprintf("%s:%s", tc.event, edge)
			if got := redis.publishedButtonEvents[len(redis.publishedButtonEvents)-1]; got != want {
				t.Fatalf("event = %q, want %q", got, want)
			}
		}
	}
	if system.getCurrentState() != types.StateStandby {
		t.Fatal("button changed vehicle state")
	}
	if len(io.digitalOutputs) != 0 || len(io.pwmCues) != 0 {
		t.Fatalf("hardware actions: outputs=%v cues=%v", io.digitalOutputs, io.pwmCues)
	}
	before := append([]string(nil), redis.publishedButtonEvents...)
	if err := system.handleInputChange("blinker_left", true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, redis.publishedButtonEvents) || len(redis.setBlinkerSwitches) != 0 {
		t.Fatal("blinker behavior changed in standby")
	}
}
