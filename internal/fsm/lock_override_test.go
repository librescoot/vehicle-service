package fsm

import (
	"context"
	"errors"
	"github.com/librescoot/librefsm"
	"testing"
	"time"
)

type lockActions struct {
	Actions
	closed            bool
	shutdowns, forced int
	shutdownErr       error
}

func (a *lockActions) EnterStandby(*librefsm.Context) error                    { return nil }
func (a *lockActions) EnterParked(*librefsm.Context) error                     { return nil }
func (a *lockActions) EnterAtRest(*librefsm.Context) error                     { return nil }
func (a *lockActions) ExitAtRest(*librefsm.Context) error                      { return nil }
func (a *lockActions) EnterWaitingSeatbox(*librefsm.Context) error             { return nil }
func (a *lockActions) EnterReadyToDrive(*librefsm.Context) error               { return nil }
func (a *lockActions) EnterHopOn(*librefsm.Context) error                      { return nil }
func (a *lockActions) EnterHopOnLearning(*librefsm.Context) error              { return nil }
func (a *lockActions) EnterHibernation(*librefsm.Context) error                { return nil }
func (a *lockActions) EnterHibernationInitialHold(*librefsm.Context) error     { return nil }
func (a *lockActions) EnterHibernationAwaitingConfirm(*librefsm.Context) error { return nil }
func (a *lockActions) EnterHibernationSeatbox(*librefsm.Context) error         { return nil }
func (a *lockActions) EnterHibernationConfirm(*librefsm.Context) error         { return nil }
func (a *lockActions) EnterShuttingDown(*librefsm.Context) error               { a.shutdowns++; return a.shutdownErr }
func (a *lockActions) IsSeatboxClosed(*librefsm.Context) bool                  { return a.closed }
func (a *lockActions) OnForceLock(*librefsm.Context) error                     { a.forced++; return nil }

func TestLockIgnoreSeatboxStates(t *testing.T) {
	for _, state := range []librefsm.StateID{StateParked, StateWaitingSeatbox, StateReadyToDrive, StateStandby, StateShuttingDown, StateUpdating, StateHopOn, StateHopOnLearning, StateHibernationInitialHold, StateHibernationAwaitingConfirm, StateHibernationSeatbox, StateHibernationConfirm} {
		for _, closed := range []bool{false, true} {
			t.Run(string(state)+map[bool]string{false: "/open", true: "/closed"}[closed], func(t *testing.T) {
				a := &lockActions{closed: closed}
				m, err := NewDefinition(a).Initial(state).Build()
				if err != nil {
					t.Fatal(err)
				}
				if err = m.Start(context.Background()); err != nil {
					t.Fatal(err)
				}
				defer m.Stop()
				before := a.shutdowns
				request := &LockIgnoreSeatboxRequest{Deadline: time.Now().Add(time.Second)}
				err = m.SendSync(librefsm.Event{ID: EvLockIgnoreSeatbox, Payload: request})
				if err != nil {
					t.Fatal(err)
				}
				allowed := state == StateParked || state == StateWaitingSeatbox
				if request.Accepted != allowed {
					t.Fatalf("accepted %v, want %v", request.Accepted, allowed)
				}
				want := state
				if allowed {
					want = StateShuttingDown
				}
				if m.CurrentState() != want || a.forced != 0 {
					t.Fatalf("state %s forced %d", m.CurrentState(), a.forced)
				}
				if allowed && a.shutdowns != before+1 {
					t.Fatal("normal shutdown not entered")
				}
			})
		}
	}
}
func TestLockIgnoreSeatboxExpiryAndOrdinaryLock(t *testing.T) {
	a := &lockActions{}
	m, err := NewDefinition(a).Initial(StateParked).Build()
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	for _, data := range []any{nil, &LockIgnoreSeatboxRequest{}, &LockIgnoreSeatboxRequest{Deadline: time.Now().Add(-time.Second)}} {
		if err = m.SendSync(librefsm.Event{ID: EvLockIgnoreSeatbox, Payload: data}); err != nil {
			t.Fatal(err)
		}
		if m.CurrentState() != StateParked {
			t.Fatal("expired/missing intent actuated")
		}
	}
	if err = m.SendSync(librefsm.Event{ID: EvLock}); err != nil {
		t.Fatal(err)
	}
	if m.CurrentState() != StateWaitingSeatbox {
		t.Fatal("ordinary lock no longer waits")
	}
	if err = m.SendSync(librefsm.Event{ID: EvLock}); err != nil {
		t.Fatal(err)
	}
	if m.CurrentState() != StateShuttingDown {
		t.Fatal("ordinary waiting lock changed")
	}
}
func TestLockIgnoreSeatboxTransitionError(t *testing.T) {
	a := &lockActions{shutdownErr: errors.New("shutdown failed")}
	m, err := NewDefinition(a).Initial(StateParked).Build()
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	r := &LockIgnoreSeatboxRequest{Deadline: time.Now().Add(time.Second)}
	if err = m.SendSync(librefsm.Event{ID: EvLockIgnoreSeatbox, Payload: r}); err == nil {
		t.Fatal("missing transition error")
	}
}

// An already-queued override cannot act after a preceding event enters drive,
// nor after its deadline elapses while another FSM action owns the processor.
func TestLockIgnoreSeatboxQueuedRaceAndExpiry(t *testing.T) {
	for _, drive := range []bool{false, true} {
		t.Run(map[bool]string{false: "expiry", true: "drive"}[drive], func(t *testing.T) {
			a := &lockActions{}
			entered := make(chan struct{})
			release := make(chan struct{})
			dest := StateParked
			if drive {
				dest = StateReadyToDrive
			}
			def := NewDefinition(a).Initial(StateParked).Transition(StateParked, "block", dest, librefsm.WithAction(func(*librefsm.Context) error { close(entered); <-release; return nil }))
			m, err := def.Build()
			if err != nil {
				t.Fatal(err)
			}
			if err = m.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer m.Stop()
			m.Send(librefsm.Event{ID: "block"})
			<-entered
			deadline := time.Now().Add(time.Second)
			if !drive {
				deadline = time.Now().Add(20 * time.Millisecond)
			}
			r := &LockIgnoreSeatboxRequest{Deadline: deadline}
			// Enqueue before releasing the processor, then use a synchronous
			// barrier to observe completion without a goroutine scheduling race.
			m.Send(librefsm.Event{ID: EvLockIgnoreSeatbox, Payload: r})
			if !drive {
				time.Sleep(time.Until(deadline) + time.Millisecond)
			}
			close(release)
			if err = m.SendSync(librefsm.Event{ID: "barrier"}); err != nil {
				t.Fatal(err)
			}
			if r.Accepted || m.CurrentState() != dest || a.shutdowns != 0 {
				t.Fatalf("race actuated: %v %s", r.Accepted, m.CurrentState())
			}
		})
	}
}

func TestOrdinaryLockClosedSeatStillShutsDown(t *testing.T) {
	a := &lockActions{closed: true}
	m, err := NewDefinition(a).Initial(StateParked).Build()
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	if err = m.SendSync(librefsm.Event{ID: EvLock}); err != nil {
		t.Fatal(err)
	}
	if m.CurrentState() != StateShuttingDown || a.shutdowns != 1 || a.forced != 0 {
		t.Fatal("ordinary closed-seat lock changed")
	}
}

func TestLockIgnoreSeatboxWaitingAcceptanceBoundary(t *testing.T) {
	for _, beforeGuard := range []bool{true, false} {
		t.Run(map[bool]string{true: "expired-before-guard", false: "expires-during-exit"}[beforeGuard], func(t *testing.T) {
			a := &lockActions{}
			exited, release := make(chan struct{}), make(chan struct{})
			// Keep the production transition/guard/actions. Only add a deterministic
			// scheduling pause to the waiting state's exit, after librefsm cancels its
			// original declarative timeout and before it enters the target state.
			def := NewDefinition(a).Initial(StateWaitingSeatbox).State(StateWaitingSeatbox,
				librefsm.WithTimeout(WaitingSeatboxTimeout, EvWaitingSeatboxTimeout),
				librefsm.WithOnEnter(a.EnterWaitingSeatbox),
				librefsm.WithOnExit(func(*librefsm.Context) error { close(exited); <-release; return nil }),
			)
			m, err := def.Build()
			if err != nil {
				t.Fatal(err)
			}
			if err = m.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer m.Stop()
			const timer = "_timeout_waiting-seatbox"
			if !m.TimerActive(timer) {
				t.Fatal("waiting timer not armed")
			}
			r := &LockIgnoreSeatboxRequest{Deadline: time.Now().Add(100 * time.Millisecond)}
			if beforeGuard {
				r.Deadline = time.Now().Add(-time.Second)
				if err = m.SendSync(librefsm.Event{ID: EvLockIgnoreSeatbox, Payload: r}); err != nil {
					t.Fatal(err)
				}
				select {
				case <-exited:
					t.Fatal("expired intent exited state")
				default:
				}
				if r.Accepted || m.CurrentState() != StateWaitingSeatbox || !m.TimerActive(timer) || a.shutdowns != 0 {
					t.Fatal("pre-acceptance expiry disturbed waiting state/timer")
				}
				return
			}
			done := make(chan error, 1)
			go func() { done <- m.SendSync(librefsm.Event{ID: EvLockIgnoreSeatbox, Payload: r}) }()
			select {
			case <-exited:
			case err := <-done:
				t.Fatalf("never reached exit: %v", err)
			case <-time.After(time.Second):
				t.Fatal("exit did not start")
			}
			cancelled := !m.TimerActive(timer)
			time.Sleep(time.Until(r.Deadline) + time.Millisecond)
			close(release)
			err = <-done
			if !cancelled {
				t.Fatal("fixture did not pause after timer cancellation")
			}
			if err != nil || !r.Accepted || m.CurrentState() != StateShuttingDown || a.shutdowns != 1 || a.forced != 0 || !m.TimerActive("_timeout_shutting-down") {
				t.Fatalf("accepted transition stranded: err=%v accepted=%v state=%s waiting-timer-active=%v shutdowns=%d", err, r.Accepted, m.CurrentState(), m.TimerActive(timer), a.shutdowns)
			}
		})
	}
}
