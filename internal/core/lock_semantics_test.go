package core

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
	"vehicle-service/internal/logger"

	"github.com/librescoot/librefsm"
	"vehicle-service/internal/fsm"
)

func lockSemanticsSystem(t *testing.T, state librefsm.StateID, closed bool) (*VehicleSystem, *mockHardwareIO, *mockMessagingClient) {
	t.Helper()
	v, io, redis := newTestVehicleSystem()
	io.setDigitalInput("kickstand", true)
	io.setDigitalInput("handlebar_lock_sensor", true)
	io.setDigitalInput("seatbox_lock_sensor", closed)
	initTestFSM(t, v)
	// Hop-on is only reached from parked in production, where the dashboard is
	// already powered. SetState starts from stand-by, so seed the power state
	// the parked->hop-on transition would have inherited.
	if state == fsm.StateHopOn || state == fsm.StateHopOnLearning {
		io.setDigitalOutput("dashboard_power", true)
	}
	t.Cleanup(func() {
		v.machine.Stop()
		v.cancelHandlebarLock()
		v.cancelHandlebarUnlock()
		v.mu.Lock()
		v.stopMapHoldTimer()
		v.mu.Unlock()
	})
	if err := v.machine.SetState(state); err != nil {
		t.Fatal(err)
	}
	return v, io, redis
}

func sendLockEvent(t *testing.T, v *VehicleSystem, event librefsm.EventID) {
	t.Helper()
	if err := v.machine.SendSync(librefsm.Event{ID: event}); err != nil {
		t.Fatal(err)
	}
}

func TestHandleStateRequest_RepeatedLock(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(map[bool]string{true: "closed", false: "open"}[closed], func(t *testing.T) {
			v, _, redis := lockSemanticsSystem(t, fsm.StateParked, closed)
			if err := v.handleStateRequest("lock"); err != nil {
				t.Fatal(err)
			}
			if !closed {
				if got := v.machine.CurrentState(); got != fsm.StateWaitingSeatbox {
					t.Fatalf("first lock: %s", got)
				}
				if err := v.handleStateRequest("lock"); err != nil {
					t.Fatal(err)
				}
			}
			if got := v.machine.CurrentState(); got != fsm.StateShuttingDown {
				t.Fatalf("shutdown: %s", got)
			}
			if n := countPublishedMessages(redis, "dbc:command", "poweroff"); n != 1 {
				t.Fatalf("poweroff count %d", n)
			}
			if err := v.handleStateRequest("lock"); err == nil {
				t.Fatal("shutdown repeat must remain rejected externally")
			}
			sendLockEvent(t, v, fsm.EvLock)
			if n := countPublishedMessages(redis, "dbc:command", "poweroff"); n != 1 {
				t.Fatalf("repeat restarted shutdown: %d", n)
			}
		})
	}
}

func TestForceLock_GracefulAcceptedStates(t *testing.T) {
	for _, state := range []librefsm.StateID{fsm.StateParked, fsm.StateHopOn, fsm.StateHopOnLearning, fsm.StateReadyToDrive, fsm.StateWaitingSeatbox} {
		t.Run(string(state), func(t *testing.T) {
			v, io, redis := lockSemanticsSystem(t, state, false)
			if state == fsm.StateHopOn {
				// Let the pre-existing window install before checking its cancellation.
				deadline := time.Now().Add(time.Second)
				for {
					v.mu.RLock()
					armed := v.handlebarTimer != nil
					v.mu.RUnlock()
					if armed {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("HopOn window not armed")
					}
					time.Sleep(time.Millisecond)
				}
			}
			if err := v.handleForceLockRequest(); err != nil {
				t.Fatal(err)
			}
			if got := v.machine.CurrentState(); got != fsm.StateShuttingDown {
				t.Fatalf("force must shut down gracefully, got %s", got)
			}
			if io.getDigitalOutput("engine_power") {
				t.Fatal("engine remains powered")
			}
			if !io.getDigitalOutput("dashboard_power") && state != fsm.StateWaitingSeatbox {
				t.Fatal("dashboard cut before timeout")
			}
			if n := countPublishedMessages(redis, "dbc:command", "poweroff"); n != 1 {
				t.Fatalf("poweroff count %d", n)
			}
			v.mu.RLock()
			locking := v.handlebarDone != nil || v.handlebarTimer != nil
			v.mu.RUnlock()
			if locking {
				t.Fatal("force shutdown armed/retained a lock window")
			}
			sendLockEvent(t, v, fsm.EvForceLock)
			if n := countPublishedMessages(redis, "dbc:command", "poweroff"); n != 1 {
				t.Fatal("repeat restarted shutdown")
			}
			sendLockEvent(t, v, fsm.EvShutdownTimeout)
			if v.machine.CurrentState() != fsm.StateStandby || io.getDigitalOutput("dashboard_power") {
				t.Fatal("timeout did not finish shutdown")
			}
			sendLockEvent(t, v, fsm.EvForceLock) // Ignored in standby, cannot taint next lock.
			sendLockEvent(t, v, fsm.EvUnlock)
			io.setDigitalInput("seatbox_lock_sensor", true)
			sendLockEvent(t, v, fsm.EvLock)
			v.mu.RLock()
			locking = v.handlebarDone != nil
			v.mu.RUnlock()
			if !locking {
				t.Fatal("forced shutdown tainted subsequent ordinary lock")
			}
		})
	}
}

func TestForceLock_HoldsAndAbortDoNotTaintOrdinaryLock(t *testing.T) {
	for _, hold := range []string{"update", "map"} {
		t.Run(hold, func(t *testing.T) {
			v, io, redis := lockSemanticsSystem(t, fsm.StateParked, true)
			v.mu.Lock()
			v.dbcUpdating = hold == "update"
			v.mapDownloading = hold == "map"
			v.mu.Unlock()
			sendLockEvent(t, v, fsm.EvForceLock)
			if v.machine.CurrentState() != fsm.StateShuttingDown {
				t.Fatal("force skipped shutdown")
			}
			if countPublishedMessages(redis, "dbc:command", "poweroff") != 0 {
				t.Fatal("hold ignored")
			}
			sendLockEvent(t, v, fsm.EvUnlock)
			if v.machine.CurrentState() != fsm.StateParked {
				t.Fatal("uncommitted shutdown cannot abort")
			}
			sendLockEvent(t, v, fsm.EvLock)
			v.mu.RLock()
			locking := v.handlebarDone != nil
			v.mu.RUnlock()
			if !locking {
				t.Fatal("force marker survived abort")
			}
			sendLockEvent(t, v, fsm.EvShutdownTimeout)
			if !io.getDigitalOutput("dashboard_power") {
				t.Fatal("hold lost on standby entry")
			}
		})
	}
}

func TestForceLock_UnhandledStatesDoNotTaintLock(t *testing.T) {
	for _, state := range []librefsm.StateID{fsm.StateStandby, fsm.StateUpdating, fsm.StateHibernationInitialHold, fsm.StateHibernationAwaitingConfirm, fsm.StateHibernationSeatbox, fsm.StateHibernationConfirm} {
		t.Run(string(state), func(t *testing.T) {
			v, _, _ := lockSemanticsSystem(t, state, true)
			sendLockEvent(t, v, fsm.EvForceLock)
			if v.machine.CurrentState() != state {
				t.Fatal("force broadened accepted states")
			}
			if err := v.machine.SetState(fsm.StateParked); err != nil {
				t.Fatal(err)
			}
			sendLockEvent(t, v, fsm.EvLock)
			v.mu.RLock()
			locking := v.handlebarDone != nil
			v.mu.RUnlock()
			if !locking {
				t.Fatal("ignored force tainted ordinary lock")
			}
		})
	}
}

func TestForceLock_EngineWriteFailureDoesNotLeakNoLockIntent(t *testing.T) {
	v, io, redis := lockSemanticsSystem(t, fsm.StateParked, true)
	io.failOutput("engine_power", fmt.Errorf("injected output failure"))
	sendLockEvent(t, v, fsm.EvForceLock)
	if v.machine.CurrentState() != fsm.StateShuttingDown {
		t.Fatal("output failure skipped graceful shutdown")
	}
	if countPublishedMessages(redis, "dbc:command", "poweroff") != 1 {
		t.Fatal("dashboard shutdown skipped on engine error")
	}
	v.mu.RLock()
	locking := v.handlebarDone != nil
	v.mu.RUnlock()
	if locking {
		t.Fatal("failed force shutdown started steering lock")
	}
	io.failOutput("engine_power", nil)
	sendLockEvent(t, v, fsm.EvShutdownTimeout)
	sendLockEvent(t, v, fsm.EvUnlock)
	sendLockEvent(t, v, fsm.EvLock)
	v.mu.RLock()
	locking = v.handlebarDone != nil
	v.mu.RUnlock()
	if !locking {
		t.Fatal("output failure leaked no-lock intent")
	}
}

func TestForceLock_HibernationOverridesDashboardHolds(t *testing.T) {
	v, _, redis := lockSemanticsSystem(t, fsm.StateParked, true)
	v.mu.Lock()
	v.dbcUpdating = true
	v.mapDownloading = true
	v.hibernationRequest = true
	v.mu.Unlock()
	sendLockEvent(t, v, fsm.EvForceLock)
	if countPublishedMessages(redis, "dbc:command", "poweroff") != 1 {
		t.Fatal("hibernation did not override holds")
	}
	v.mu.RLock()
	locking := v.handlebarDone != nil
	updating := v.dbcUpdating
	mapping := v.mapDownloading
	v.mu.RUnlock()
	if locking || updating || mapping {
		t.Fatal("forced hibernation retained lock work or holds")
	}
}

// Gate the handler just after its parked precheck, then put drive mode first
// in the FSM queue. The FSM, not the stale external snapshot, is authoritative.
type lockRequestLogGate struct{ reached, release chan struct{} }

func (g *lockRequestLogGate) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "Sending EvLock") {
		close(g.reached)
		<-g.release
	}
	return len(p), nil
}

func TestHandleStateRequest_LockDriveRace(t *testing.T) {
	v, _, _ := lockSemanticsSystem(t, fsm.StateParked, true)
	gate := &lockRequestLogGate{make(chan struct{}), make(chan struct{})}
	v.logger = logger.NewLogger(log.New(gate, "", 0), logger.LogLevelInfo)
	result := make(chan error, 1)
	go func() { result <- v.handleStateRequest("lock") }()
	select {
	case <-gate.reached:
	case <-time.After(time.Second):
		t.Fatal("handler gate not reached")
	}
	// Queue drive without waiting: the log writer is deliberately held by the
	// handler, and drive entry logs too. Queue order is all this test needs.
	v.mu.Lock()
	v.dashboardReady = true
	v.handlebarUnlocked = true
	v.mu.Unlock()
	v.machine.Send(librefsm.Event{ID: fsm.EvKickstandUp})
	close(gate.release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if v.machine.CurrentState() != fsm.StateReadyToDrive {
		t.Fatalf("ordinary lock changed drive state: %s", v.machine.CurrentState())
	}
}

// ExitHopOn's owned release predates graceful force shutdown and is independent
// of the cancellable unlock retry loop. Hold its first output write to prove
// cancellation does not own (or wait for) that release. Do not treat a returned
// force-lock request as proof that all handlebar work has finished.
type hopOnReleaseGate struct {
	*mockHardwareIO
	once                     sync.Once
	reached, release, opened chan struct{}
}

func (g *hopOnReleaseGate) WriteDigitalOutput(channel string, value bool) error {
	if channel == "handlebar_lock_close" && !value {
		g.once.Do(func() { close(g.reached); <-g.release })
	}
	err := g.mockHardwareIO.WriteDigitalOutput(channel, value)
	if channel == "handlebar_lock_open" && value {
		close(g.opened)
	}
	return err
}

func TestExitHopOn_OwnedReleaseIsIndependentOfRetryCancellation(t *testing.T) {
	v, io, _ := lockSemanticsSystem(t, fsm.StateHopOnLearning, true)
	// HopOn entry normally sets ownership only after sensor-confirmed locking.
	// Seed that already-completed work without starting a positioning goroutine.
	v.mu.Lock()
	v.hopOnLockedHandlebar = true
	v.mu.Unlock()
	gate := &hopOnReleaseGate{mockHardwareIO: io, reached: make(chan struct{}), release: make(chan struct{}), opened: make(chan struct{})}
	v.io = gate
	if err := v.ExitHopOn(&librefsm.Context{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.reached:
	case <-time.After(time.Second):
		t.Fatal("owned release did not start")
	}
	sendLockEvent(t, v, fsm.EvForceLock)
	if v.machine.CurrentState() != fsm.StateShuttingDown {
		t.Fatal("force did not enter shutdown")
	}
	close(gate.release)
	select {
	case <-gate.opened:
	case <-time.After(time.Second):
		t.Fatal("existing owned release was unexpectedly cancelled")
	}
	v.mu.RLock()
	locking := v.handlebarDone != nil
	v.mu.RUnlock()
	if locking {
		t.Fatal("forced shutdown added lock positioning work")
	}
}

func TestForceLock_OrdinaryShutdownRepeatPreservesOwnership(t *testing.T) {
	v, _, redis := lockSemanticsSystem(t, fsm.StateParked, true)
	sendLockEvent(t, v, fsm.EvLock)
	v.mu.RLock()
	done := v.handlebarDone
	v.mu.RUnlock()
	if done == nil {
		t.Fatal("ordinary shutdown did not start locking")
	}
	v.pendingUnlock.Store(true)
	sendLockEvent(t, v, fsm.EvForceLock)
	v.mu.RLock()
	sameLock := v.handlebarDone == done
	v.mu.RUnlock()
	if !sameLock || !v.pendingUnlock.Load() || !v.dbcPoweroffSent.Load() {
		t.Fatal("ignored force interrupted ordinary shutdown ownership")
	}
	if countPublishedMessages(redis, "dbc:command", "poweroff") != 1 {
		t.Fatal("ignored force restarted shutdown")
	}
	v.pendingUnlock.Store(false)
}
