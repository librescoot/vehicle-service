package core

import (
	"context"
	"fmt"
	"github.com/librescoot/librefsm"
	"testing"
	"time"
	"vehicle-service/internal/fsm"
)

func TestHandleLockIgnoreSeatboxRequest(t *testing.T) {
	system, _, _ := newTestVehicleSystem()
	if err := system.handleLockIgnoreSeatboxRequest(time.Now().Add(time.Second)); err == nil {
		t.Fatal("uninitialised accepted")
	}
	initTestFSM(t, system)
	defer system.machine.Stop()
	if err := system.machine.SetState(fsm.StateParked); err != nil {
		t.Fatal(err)
	}
	if err := system.handleLockIgnoreSeatboxRequest(time.Now().Add(-time.Second)); err == nil {
		t.Fatal("expired accepted")
	}
	// Ordinary handler is still parked-only and does not gain override intent.
	if err := system.machine.SetState(fsm.StateWaitingSeatbox); err != nil {
		t.Fatal(err)
	}
	if err := system.handleStateRequest("lock"); err == nil {
		t.Fatal("ordinary handler changed")
	}
	if err := system.handleLockIgnoreSeatboxRequest(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if system.machine.CurrentState() != fsm.StateShuttingDown {
		t.Fatal("did not enter normal shutdown")
	}
	if system.forceStandbyNoLock {
		t.Fatal("force lock was used")
	}
	if err := system.machine.SetState(fsm.StateReadyToDrive); err != nil {
		t.Fatal(err)
	}
	// Simulate a stale external state precheck: only the FSM is authoritative.
	if err := system.handleLockIgnoreSeatboxRequest(time.Now().Add(time.Second)); err == nil || err.Error() != "unsafe-state" {
		t.Fatalf("drive: %v", err)
	}
}

func TestHandleLockIgnoreSeatboxProcessingErrorIsNotAccepted(t *testing.T) {
	system, _, _ := newTestVehicleSystem()
	def := librefsm.NewDefinition().State(fsm.StateParked).State(fsm.StateShuttingDown).
		Initial(fsm.StateParked).Transition(fsm.StateParked, fsm.EvLockIgnoreSeatbox, fsm.StateShuttingDown,
		librefsm.WithAction(func(c *librefsm.Context) error {
			c.Event.Payload.(*fsm.LockIgnoreSeatboxRequest).Accepted = true
			return fmt.Errorf("entry failure")
		}))
	machine, err := def.Build()
	if err != nil {
		t.Fatal(err)
	}
	system.machine = machine
	if err = machine.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer machine.Stop()
	if err = system.handleLockIgnoreSeatboxRequest(time.Now().Add(time.Second)); err == nil || err.Error() != "processing" {
		t.Fatalf("processing error: %v", err)
	}
}

// The real ExitAtRest must be allowed to finish once the pre-exit guard has
// accepted intent, even if clearing Redis's deadline outlasts the caller.
type blockingLockDeadlineClient struct {
	MessagingClient
	started, release chan struct{}
	clears           int
}

func (m *blockingLockDeadlineClient) ClearAutoStandbyDeadline() error {
	m.clears++
	close(m.started)
	<-m.release
	return nil
}

func TestLockIgnoreSeatboxParkedAcceptanceBoundary(t *testing.T) {
	for _, beforeGuard := range []bool{true, false} {
		t.Run(map[bool]string{true: "expired-before-guard", false: "expires-during-exit"}[beforeGuard], func(t *testing.T) {
			system, _, redis := newTestVehicleSystem()
			initTestFSM(t, system)
			defer system.machine.Stop()
			system.autoStandbySeconds = 60
			if err := system.machine.SetState(fsm.StateParked); err != nil {
				t.Fatal(err)
			}
			originalDeadline := system.autoStandbyDeadline
			if !system.machine.TimerActive(fsm.TimerAutoStandby) {
				t.Fatal("auto-standby not armed")
			}
			blocked := &blockingLockDeadlineClient{MessagingClient: redis, started: make(chan struct{}), release: make(chan struct{})}
			system.redis = blocked
			if beforeGuard {
				r := &fsm.LockIgnoreSeatboxRequest{Deadline: time.Now().Add(-time.Second)}
				if err := system.machine.SendSync(librefsm.Event{ID: fsm.EvLockIgnoreSeatbox, Payload: r}); err != nil {
					t.Fatal(err)
				}
				if r.Accepted || system.machine.CurrentState() != fsm.StateParked || !system.machine.TimerActive(fsm.TimerAutoStandby) || !system.autoStandbyDeadline.Equal(originalDeadline) || blocked.clears != 0 {
					t.Fatal("expiry before acceptance changed parked state/deadline/timer")
				}
				return
			}
			deadline := time.Now().Add(100 * time.Millisecond)
			done := make(chan error, 1)
			go func() { done <- system.handleLockIgnoreSeatboxRequest(deadline) }()
			select {
			case <-blocked.started:
			case err := <-done:
				t.Fatalf("never reached exit: %v", err)
			case <-time.After(time.Second):
				t.Fatal("exit did not start")
			}
			// Timer teardown has happened while the machine still owns the transition.
			cancelled := !system.machine.TimerActive(fsm.TimerAutoStandby)
			time.Sleep(time.Until(deadline) + time.Millisecond)
			close(blocked.release)
			err := <-done
			if !cancelled {
				t.Fatal("fixture did not reach timer cancellation")
			}
			if err != nil || system.machine.CurrentState() != fsm.StateShuttingDown {
				t.Fatalf("accepted transition stranded: err=%v state=%s auto-standby-active=%v", err, system.machine.CurrentState(), system.machine.TimerActive(fsm.TimerAutoStandby))
			}
			if blocked.clears != 1 || countPublishedMessages(redis, "dbc:command", "poweroff") != 1 || !system.machine.TimerActive("_timeout_shutting-down") || system.forceStandbyNoLock {
				t.Fatal("accepted request did not finish exactly one normal shutdown")
			}
		})
	}
}
