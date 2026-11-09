package hardware

import (
	"errors"
	"io"
	"log"
	"reflect"
	"testing"

	"vehicle-service/internal/logger"
)

type stubSwitcher struct {
	calls  []string
	err    error
	closed bool
}

func (s *stubSwitcher) SetUnitActive(unit string, active bool) error {
	if s.err != nil {
		return s.err
	}
	action := "stop"
	if active {
		action = "start"
	}
	s.calls = append(s.calls, action+" "+unit)
	return nil
}

func (s *stubSwitcher) Close() { s.closed = true }

// The DBC link and the PPP backup are switched by unit, not by a shell call,
// so that the units own what the link means and the route monitor can follow
// the link unit.
func TestLinkSwitchingGoesThroughUnits(t *testing.T) {
	stub := &stubSwitcher{}
	io := &LinuxHardwareIO{
		logger:  logger.NewLogger(log.New(io.Discard, "", 0), logger.LogLevelError),
		systemd: stub,
	}

	for _, call := range []struct {
		name   string
		enable bool
		fn     func(bool) error
	}{
		{"usb0 up", true, io.SetUsb0Enabled},
		{"usb0 down", false, io.SetUsb0Enabled},
		{"ppp up", true, io.SetPppLinkEnabled},
		{"ppp down", false, io.SetPppLinkEnabled},
	} {
		if err := call.fn(call.enable); err != nil {
			t.Fatalf("%s: %v", call.name, err)
		}
	}

	want := []string{
		"start " + DbcLinkUnit,
		"stop " + DbcLinkUnit,
		"start " + PppLinkUnit,
		"stop " + PppLinkUnit,
	}
	if !reflect.DeepEqual(stub.calls, want) {
		t.Fatalf("unit calls = %v, want %v", stub.calls, want)
	}
}

func TestLinkSwitchingReportsUnitErrors(t *testing.T) {
	stub := &stubSwitcher{err: errors.New("no such unit")}
	io := &LinuxHardwareIO{
		logger:  logger.NewLogger(log.New(io.Discard, "", 0), logger.LogLevelError),
		systemd: stub,
	}

	for _, call := range []struct {
		name string
		fn   func(bool) error
	}{
		{"usb0", io.SetUsb0Enabled},
		{"ppp", io.SetPppLinkEnabled},
	} {
		if err := call.fn(true); err == nil {
			t.Fatalf("%s: a failed unit switch must not be swallowed", call.name)
		}
	}
}
