package core

import (
	"sync"
	"time"
)

const (
	gestureLongTapThreshold   = 500 * time.Millisecond
	gestureHoldThreshold      = 2 * time.Second
	gestureDoubleTapThreshold = 500 * time.Millisecond
)

type gestureState struct {
	pressed      bool
	pressTime    time.Time
	longTapTimer *time.Timer
	holdTimer    *time.Timer
	longTapFired bool
	holdFired    bool

	// Tracks the most recent emitted tap so the second tap within
	// the double-tap window triggers an additional double-tap event.
	// Cleared by any long-tap / hold emission so a long press between
	// two taps does not glue them together.
	lastTapAt    time.Time
	lastTapValid bool
}

type gestureDetector struct {
	mu             sync.Mutex
	inputs         map[string]*gestureState
	emit           func(event string)
	longTapDelay   time.Duration
	holdDelay      time.Duration
	doubleTapDelay time.Duration
}

func newGestureDetector(emit func(string), longTapThreshold, holdThreshold, doubleTapThreshold time.Duration) *gestureDetector {
	return &gestureDetector{
		inputs:         make(map[string]*gestureState),
		emit:           emit,
		longTapDelay:   longTapThreshold,
		holdDelay:      holdThreshold,
		doubleTapDelay: doubleTapThreshold,
	}
}

func (g *gestureDetector) OnChange(name string, pressed bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	gs, ok := g.inputs[name]
	if !ok {
		gs = &gestureState{}
		g.inputs[name] = gs
	}

	if pressed {
		gs.pressed = true
		gs.pressTime = time.Now()
		gs.longTapFired = false
		gs.holdFired = false
		g.emit(name + ":press")

		g.stopTimers(gs)

		gs.longTapTimer = time.AfterFunc(g.longTapDelay, func() {
			g.mu.Lock()
			stillPressed := gs.pressed
			if stillPressed {
				gs.longTapFired = true
				// Long press between taps breaks the double-tap window.
				gs.lastTapValid = false
			}
			g.mu.Unlock()
			if stillPressed {
				g.emit(name + ":long-tap")
			}
		})

		gs.holdTimer = time.AfterFunc(g.holdDelay, func() {
			g.mu.Lock()
			stillPressed := gs.pressed
			if stillPressed {
				gs.holdFired = true
				gs.lastTapValid = false
			}
			g.mu.Unlock()
			if stillPressed {
				g.emit(name + ":hold")
			}
		})
	} else {
		wasPressed := gs.pressed
		gs.pressed = false

		g.stopTimers(gs)

		if wasPressed {
			g.emit(name + ":release")

			if !gs.longTapFired && !gs.holdFired {
				now := time.Now()
				g.emit(name + ":tap")

				if gs.lastTapValid && now.Sub(gs.lastTapAt) <= g.doubleTapDelay {
					gs.lastTapValid = false
					g.emit(name + ":double-tap")
				} else {
					gs.lastTapAt = now
					gs.lastTapValid = true
				}
			}
		}
	}
}

func (g *gestureDetector) stopTimers(gs *gestureState) {
	if gs.longTapTimer != nil {
		gs.longTapTimer.Stop()
		gs.longTapTimer = nil
	}
	if gs.holdTimer != nil {
		gs.holdTimer.Stop()
		gs.holdTimer = nil
	}
}

func (g *gestureDetector) Stop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, gs := range g.inputs {
		g.stopTimers(gs)
	}
}
