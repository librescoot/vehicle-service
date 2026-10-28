package core

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"vehicle-service/internal/fsm"
	"vehicle-service/internal/types"
)

type blockingDriveRedis struct {
	*mockMessagingClient
	blocked     string
	entered     chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
}

func (m *blockingDriveRedis) unblock() {
	m.releaseOnce.Do(func() { close(m.release) })
}

func (m *blockingDriveRedis) wait(method string) {
	if m.blocked == method {
		close(m.entered)
		<-m.release
	}
}

func (m *blockingDriveRedis) SetBacklightEnabled(enabled bool) error {
	m.wait("backlight")
	return m.mockMessagingClient.SetBacklightEnabled(enabled)
}

func (m *blockingDriveRedis) SetEnginePower(enabled bool) error {
	m.wait("engine-power")
	return m.mockMessagingClient.SetEnginePower(enabled)
}

func TestEnterReadyToDrive_GPIORunsBeforeRedisAndStatePublish(t *testing.T) {
	for _, blocked := range []string{"backlight", "engine-power"} {
		t.Run(blocked, func(t *testing.T) {
			system, io, redis := restoreTestSystem(t)
			io.setDigitalInput("handlebar_lock_sensor", true)
			if err := system.machine.SetState(fsm.StateParked); err != nil {
				t.Fatalf("enter parked: %v", err)
			}
			io.setDigitalInput("kickstand", false)
			system.dashboardReady = true
			gate := &blockingDriveRedis{
				mockMessagingClient: redis,
				blocked:             blocked,
				entered:             make(chan struct{}),
				release:             make(chan struct{}),
			}
			system.redis = gate
			defer gate.unblock()

			done := make(chan error, 1)
			go func() { done <- system.machine.SetState(fsm.StateReadyToDrive) }()
			select {
			case <-gate.entered:
			case err := <-done:
				t.Fatalf("transition completed before %s was blocked: %v", blocked, err)
			case <-time.After(time.Second):
				t.Fatalf("%s was not called", blocked)
			}

			if !io.getDigitalOutput("engine_power") || !io.getDigitalOutput("dashboard_power") {
				t.Error("power GPIO writes must precede Redis completion")
			}
			if io.getDigitalOutput("engine_brake") {
				t.Error("brake GPIO must follow the levers before Redis completion")
			}
			redis.mu.Lock()
			published := append([]types.SystemState(nil), redis.publishedStates...)
			redis.mu.Unlock()
			if len(published) > 0 && published[len(published)-1] == types.StateReadyToDrive {
				t.Error("ready-to-drive must not publish until Redis effects join")
			}
			gate.unblock()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("enter ready-to-drive: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("transition did not join Redis effects")
			}
		})
	}
}

func TestEnterReadyToDrive_DashboardFailureDoesNotPublishStaleEngineOn(t *testing.T) {
	system, io, redis := restoreTestSystem(t)
	io.setDigitalInput("kickstand", false)
	io.setDigitalInput("handlebar_lock_sensor", true)
	io.setDigitalOutput("engine_brake", true)
	system.dashboardReady = true
	io.failOutput("dashboard_power", fmt.Errorf("gpio line busy"))

	_ = system.machine.SetState(fsm.StateReadyToDrive)
	if io.getDigitalOutput("engine_power") || !io.getDigitalOutput("engine_brake") {
		t.Fatal("failed dashboard power must cut the engine and retain the brake")
	}
	redis.mu.Lock()
	commands := append([]bool(nil), redis.enginePowerSets...)
	redis.mu.Unlock()
	if len(commands) != 1 || commands[0] {
		t.Errorf("expected only the final engine-power off command, got %v", commands)
	}
}
