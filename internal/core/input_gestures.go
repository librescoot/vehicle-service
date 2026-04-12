package core

import (
	"sync"
	"time"
)

const (
	gestureLongTapThreshold = 500 * time.Millisecond
	gestureHoldThreshold    = 2 * time.Second
)

type gestureState struct {
	pressed      bool
	pressTime    time.Time
	longTapTimer *time.Timer
	holdTimer    *time.Timer
	longTapFired bool
	holdFired    bool
}

type gestureDetector struct {
	mu            sync.Mutex
	inputs        map[string]*gestureState
	emit          func(event string)
	longTapDelay  time.Duration
	holdDelay     time.Duration
}

func newGestureDetector(emit func(string), longTapThreshold, holdThreshold time.Duration) *gestureDetector {
	return &gestureDetector{
		inputs:       make(map[string]*gestureState),
		emit:         emit,
		longTapDelay: longTapThreshold,
		holdDelay:    holdThreshold,
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
				g.emit(name + ":tap")
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
