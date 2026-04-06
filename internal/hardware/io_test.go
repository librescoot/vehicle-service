package hardware

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDebounce_FiltersBounces(t *testing.T) {
	io := NewLinuxHardwareIO(nil)
	io.SetDebounce("handlebar_position", 200*time.Millisecond)

	var callCount int32
	var lastValue bool
	var mu sync.Mutex

	io.mu.Lock()
	io.inputCallbacks["handlebar_position"] = func(channel string, value bool) error {
		atomic.AddInt32(&callCount, 1)
		mu.Lock()
		lastValue = value
		mu.Unlock()
		return nil
	}
	io.mu.Unlock()

	// Rapid bounces: true → false → true within the debounce window
	io.debounceInput("handlebar_position", true)
	time.Sleep(50 * time.Millisecond)
	io.debounceInput("handlebar_position", false)
	time.Sleep(50 * time.Millisecond)
	io.debounceInput("handlebar_position", true)

	// Wait for the debounce timer to fire
	time.Sleep(300 * time.Millisecond)

	count := atomic.LoadInt32(&callCount)
	if count != 1 {
		t.Errorf("expected 1 callback, got %d", count)
	}
	mu.Lock()
	got := lastValue
	mu.Unlock()
	if !got {
		t.Errorf("expected final value true, got false")
	}
}

func TestDebounce_PassthroughWhenNotConfigured(t *testing.T) {
	io := NewLinuxHardwareIO(nil)

	var callCount int32

	io.mu.Lock()
	io.inputCallbacks["kickstand"] = func(channel string, value bool) error {
		atomic.AddInt32(&callCount, 1)
		return nil
	}
	io.mu.Unlock()

	// No debounce configured for this channel — should fire immediately
	io.debounceInput("kickstand", true)
	io.debounceInput("kickstand", false)
	io.debounceInput("kickstand", true)

	count := atomic.LoadInt32(&callCount)
	if count != 3 {
		t.Errorf("expected 3 immediate callbacks, got %d", count)
	}
}

func TestDebounce_CancelledTimerDoesNotFire(t *testing.T) {
	var mu sync.Mutex
	var calls []bool

	cb := func(channel string, value bool) error {
		mu.Lock()
		calls = append(calls, value)
		mu.Unlock()
		return nil
	}

	io := NewLinuxHardwareIO(nil)
	io.SetDebounce("test", 100*time.Millisecond)
	io.mu.Lock()
	io.inputCallbacks["test"] = cb
	io.mu.Unlock()

	// First event — will be superseded immediately
	io.debounceInput("test", true)
	io.debounceInput("test", false)

	// Wait for both timers to have had a chance to fire
	time.Sleep(250 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 {
		t.Errorf("expected 1 call, got %d", len(calls))
	}
	if len(calls) > 0 && calls[0] != false {
		t.Errorf("expected false (final value), got %v", calls[0])
	}
}

func TestDebounce_StableInputFiresOnce(t *testing.T) {
	io := NewLinuxHardwareIO(nil)
	io.SetDebounce("handlebar_position", 100*time.Millisecond)

	var callCount int32
	var lastValue bool
	var mu sync.Mutex

	io.mu.Lock()
	io.inputCallbacks["handlebar_position"] = func(channel string, value bool) error {
		atomic.AddInt32(&callCount, 1)
		mu.Lock()
		lastValue = value
		mu.Unlock()
		return nil
	}
	io.mu.Unlock()

	io.debounceInput("handlebar_position", false)

	// Wait past the debounce window
	time.Sleep(200 * time.Millisecond)

	count := atomic.LoadInt32(&callCount)
	if count != 1 {
		t.Errorf("expected 1 callback after stable input, got %d", count)
	}
	mu.Lock()
	got := lastValue
	mu.Unlock()
	if got {
		t.Errorf("expected final value false, got true")
	}
}
