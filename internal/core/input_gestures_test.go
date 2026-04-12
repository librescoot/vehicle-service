package core

import (
	"sync"
	"testing"
	"time"
)

const (
	testLongTap = 50 * time.Millisecond
	testHold    = 200 * time.Millisecond
)

type eventRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *eventRecorder) record(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *eventRecorder) get() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	copy(out, r.events)
	return out
}

func TestGestureTap(t *testing.T) {
	rec := &eventRecorder{}
	gd := newGestureDetector(rec.record, testLongTap, testHold)
	defer gd.Stop()

	gd.OnChange("btn", true)
	time.Sleep(10 * time.Millisecond)
	gd.OnChange("btn", false)
	time.Sleep(10 * time.Millisecond)

	events := rec.get()
	expected := []string{"btn:press", "btn:release", "btn:tap"}
	if len(events) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, events)
	}
	for i, e := range expected {
		if events[i] != e {
			t.Errorf("event[%d]: expected %q, got %q", i, e, events[i])
		}
	}
}

func TestGestureLongTap(t *testing.T) {
	rec := &eventRecorder{}
	gd := newGestureDetector(rec.record, testLongTap, testHold)
	defer gd.Stop()

	gd.OnChange("btn", true)
	time.Sleep(testLongTap + 20*time.Millisecond)
	gd.OnChange("btn", false)
	time.Sleep(10 * time.Millisecond)

	events := rec.get()
	expected := []string{"btn:press", "btn:long-tap", "btn:release"}
	if len(events) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, events)
	}
	for i, e := range expected {
		if events[i] != e {
			t.Errorf("event[%d]: expected %q, got %q", i, e, events[i])
		}
	}
}

func TestGestureHold(t *testing.T) {
	rec := &eventRecorder{}
	gd := newGestureDetector(rec.record, testLongTap, testHold)
	defer gd.Stop()

	gd.OnChange("btn", true)
	time.Sleep(testHold + 20*time.Millisecond)
	gd.OnChange("btn", false)
	time.Sleep(10 * time.Millisecond)

	events := rec.get()
	expected := []string{"btn:press", "btn:long-tap", "btn:hold", "btn:release"}
	if len(events) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, events)
	}
	for i, e := range expected {
		if events[i] != e {
			t.Errorf("event[%d]: expected %q, got %q", i, e, events[i])
		}
	}
}

func TestGestureReleaseWithoutPress(t *testing.T) {
	rec := &eventRecorder{}
	gd := newGestureDetector(rec.record, testLongTap, testHold)
	defer gd.Stop()

	gd.OnChange("btn", false)
	time.Sleep(10 * time.Millisecond)

	events := rec.get()
	if len(events) != 0 {
		t.Fatalf("expected no events, got %v", events)
	}
}

func TestGestureMultipleInputs(t *testing.T) {
	rec := &eventRecorder{}
	gd := newGestureDetector(rec.record, testLongTap, testHold)
	defer gd.Stop()

	gd.OnChange("a", true)
	time.Sleep(10 * time.Millisecond)
	gd.OnChange("b", true)
	time.Sleep(testLongTap + 20*time.Millisecond)

	// "a" should have long-tap by now; release it
	gd.OnChange("a", false)
	time.Sleep(10 * time.Millisecond)
	// "b" should also have long-tap; quick release
	gd.OnChange("b", false)
	time.Sleep(10 * time.Millisecond)

	events := rec.get()

	hasATap := false
	hasBTap := false
	hasALongTap := false
	hasBLongTap := false
	for _, e := range events {
		switch e {
		case "a:tap":
			hasATap = true
		case "b:tap":
			hasBTap = true
		case "a:long-tap":
			hasALongTap = true
		case "b:long-tap":
			hasBLongTap = true
		}
	}

	if hasATap {
		t.Error("input 'a' should not have tap (held past long-tap threshold)")
	}
	if hasBTap {
		t.Error("input 'b' should not have tap (held past long-tap threshold)")
	}
	if !hasALongTap {
		t.Error("input 'a' should have long-tap")
	}
	if !hasBLongTap {
		t.Error("input 'b' should have long-tap")
	}
}

func TestGestureStop(t *testing.T) {
	rec := &eventRecorder{}
	gd := newGestureDetector(rec.record, testLongTap, testHold)

	gd.OnChange("btn", true)
	gd.Stop()
	time.Sleep(testHold + 50*time.Millisecond)

	events := rec.get()
	for _, e := range events {
		if e == "btn:long-tap" || e == "btn:hold" {
			t.Errorf("timer fired after Stop(): got %q", e)
		}
	}
}
