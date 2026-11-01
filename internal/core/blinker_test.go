package core

import (
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"vehicle-service/internal/led"
)

type gatedBlinkerIO struct {
	*mockHardwareIO
	entered chan struct{}
	release chan struct{}
}

func (m *gatedBlinkerIO) ReadDigitalInput(channel string) (bool, error) {
	if channel == "blinker_right" {
		close(m.entered)
		<-m.release
	}
	return m.mockHardwareIO.ReadDigitalInput(channel)
}

func TestBlinkerConcurrentGPIOAndCommand(t *testing.T) {
	for _, command := range []string{"both", "off"} {
		t.Run(command, func(t *testing.T) {
			v, hw, _ := newTestVehicleSystem()
			gate := &gatedBlinkerIO{hw, make(chan struct{}), make(chan struct{})}
			v.io = gate
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				if err := v.handleBlinkerChange("blinker_left", true); err != nil {
					t.Error(err)
				}
			}()
			<-gate.entered
			commandDone := make(chan struct{})
			go func() {
				defer wg.Done()
				defer close(commandDone)
				if err := v.handleBlinkerRequest(command); err != nil {
					t.Error(err)
				}
			}()
			// Mutex waits are not durable blocks in testing/synctest.
			select {
			case <-commandDone:
			case <-time.After(100 * time.Millisecond):
			}

			// Retain any prematurely started worker so a failing regression
			// test can still clean up the orphan instead of leaking it.
			v.mu.RLock()
			prematureStop, prematureExit := v.blinkerStopChan, v.blinkerExited
			v.mu.RUnlock()
			close(gate.release)
			wg.Wait()
			if command == "off" && v.getBlinkerState() != BlinkerOff {
				t.Error("off was overtaken by the older GPIO request")
			}
			if err := v.handleBlinkerRequest("off"); err != nil {
				t.Fatal(err)
			}
			hw.mu.Lock()
			before := len(hw.pwmCues)
			hw.mu.Unlock()
			time.Sleep(2 * blinkerInterval)
			hw.mu.Lock()
			after := len(hw.pwmCues)
			hw.mu.Unlock()
			if after != before {
				t.Errorf("orphan worker wrote %d cues after off", after-before)
			}
			if prematureStop != nil {
				select {
				case <-prematureStop:
				default:
					close(prematureStop)
				}
				<-prematureExit
			}
		})
	}
}

func TestBlinkerInvalidRequestPreservesActiveWorker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		v, _, _ := newTestVehicleSystem()
		defer v.stopBlinker()
		if err := v.handleBlinkerRequest("both"); err != nil {
			t.Fatal(err)
		}
		stop := v.blinkerStopChan
		if err := v.handleBlinkerRequest("invalid"); err == nil {
			t.Fatal("invalid request accepted")
		}
		select {
		case <-stop:
			t.Error("invalid request stopped the active worker")
		default:
		}
	})
}

func TestBlinkerOffUsesMonotonicFadeTime(t *testing.T) {
	for _, wallShift := range []time.Duration{-3 * time.Minute, 3 * time.Minute} {
		t.Run(wallShift.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				v, _, _ := newTestVehicleSystem()
				lib := led.NewCurveLibrary(v.logger)
				samples := make([]uint16, 126)
				for i := 1; i < 125; i++ {
					samples[i] = led.PWMPeriod
				}
				lib.Fades[10] = &led.Fade{
					Samples: samples, Duration: 504 * time.Millisecond,
					ZeroPoints: []time.Duration{0, 500 * time.Millisecond}, EndsAtZero: true,
				}
				lib.Cues[12] = &led.Cue{
					Duration: 504 * time.Millisecond,
					Actions:  []led.CueAction{{ActionType: led.ActionTypeFade, FadeIndex: 10}},
				}
				v.ledCurves = lib
				if err := v.handleBlinkerRequest("both"); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				time.Sleep(100 * time.Millisecond)
				// The externally published Unix anchor must not control the
				// remaining fade time when wall and monotonic clocks disagree.
				v.blinkerStartNanos.Add(int64(wallShift))
				before := time.Now()
				if err := v.handleBlinkerRequest("off"); err != nil {
					t.Fatal(err)
				}
				if elapsed := time.Since(before); elapsed != 400*time.Millisecond {
					t.Errorf("off waited %v, want 400ms", elapsed)
				}
			})
		})
	}
}

func TestBlinkerShutdownRejectsLateCallbacks(t *testing.T) {
	v, hw, _ := newTestVehicleSystem()
	if err := v.handleBlinkerRequest("both"); err != nil {
		t.Fatal(err)
	}
	v.Shutdown()
	hw.mu.Lock()
	before := len(hw.pwmCues)
	hw.mu.Unlock()
	if err := v.handleBlinkerRequest("both"); err == nil {
		t.Error("command accepted after shutdown")
	}
	if err := v.handleBlinkerChange("blinker_left", true); err == nil {
		t.Error("GPIO callback accepted after shutdown")
	}
	hw.mu.Lock()
	defer hw.mu.Unlock()
	if len(hw.pwmCues) != before {
		t.Error("late callback wrote to hardware")
	}
}

type failingBlinkerRedis struct {
	*mockMessagingClient
}

func (m *failingBlinkerRedis) SetBlinkerState(string, int64) error {
	return fmt.Errorf("Redis unavailable")
}

func TestBlinkerStartReturnsPublishError(t *testing.T) {
	v, _, redis := newTestVehicleSystem()
	v.redis = &failingBlinkerRedis{redis}
	defer v.stopBlinker()
	if err := v.handleBlinkerRequest("both"); err == nil {
		t.Error("state publication error not returned")
	}
}

func TestBlinkerPublishesActiveStateOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		v, _, redis := newTestVehicleSystem()
		if err := v.handleBlinkerRequest("both"); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		v.stopBlinker()
		if len(redis.setBlinkerStates) != 1 {
			t.Errorf("published %d states for one command", len(redis.setBlinkerStates))
		}
	})
}

func TestBlinkerPublishedWallAnchorDoesNotDriveSchedule(t *testing.T) {
	for _, shift := range []time.Duration{-3 * time.Minute, 3 * time.Minute} {
		t.Run(shift.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				v, hw, _ := newTestVehicleSystem()
				if err := v.handleBlinkerRequest("both"); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				anchor := v.blinkerStartNanos.Load()
				v.blinkerStartNanos.Add(int64(shift))
				time.Sleep(2500 * time.Millisecond)
				v.stopBlinker()
				if len(hw.pwmCues) != 4 {
					t.Errorf("got %d cues, want 4 at 800ms intervals", len(hw.pwmCues))
				}
				if got := v.blinkerStartNanos.Load(); got != anchor {
					t.Errorf("published phase not reconciled: got %d, want %d", got, anchor)
				}
			})
		})
	}
}

type blinkerIndicatorIO struct {
	*mockHardwareIO
	colors     []string
	brightness []uint8
	delayFirst time.Duration
}

func (m *blinkerIndicatorIO) SetDbcLed(color string, brightness uint8) error {
	m.colors = append(m.colors, color)
	m.brightness = append(m.brightness, brightness)
	if len(m.brightness) == 1 {
		time.Sleep(m.delayFirst)
	}
	return nil
}

func TestBlinkerLateTimerSkipsMissedCycles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		v, hw, _ := newTestVehicleSystem()
		v.io = &blinkerIndicatorIO{mockHardwareIO: hw, delayFirst: 3 * blinkerInterval}
		v.dbcBlinkerLed = true
		if err := v.handleBlinkerRequest("both"); err != nil {
			t.Fatal(err)
		}
		anchor := v.blinkerStartNanos.Load()
		time.Sleep(4*blinkerInterval + 100*time.Millisecond)
		v.stopBlinker()
		if len(hw.pwmCues) != 3 {
			t.Errorf("got %d cues after a stalled indicator, want 3", len(hw.pwmCues))
		}
		if got := v.blinkerStartNanos.Load() - anchor; got != int64(3*blinkerInterval) {
			t.Errorf("phase moved by %v, want %v", time.Duration(got), 3*blinkerInterval)
		}
	})
}

func TestBlinkerDashboardIndicatorStops(t *testing.T) {
	for _, state := range []string{"left", "right", "both"} {
		t.Run(state, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				v, hw, _ := newTestVehicleSystem()
				indicator := &blinkerIndicatorIO{mockHardwareIO: hw}
				v.io = indicator
				v.dbcBlinkerLed = true
				if err := v.handleBlinkerRequest(state); err != nil {
					t.Fatal(err)
				}
				time.Sleep(2 * blinkerInterval)
				if err := v.handleBlinkerRequest("off"); err != nil {
					t.Fatal(err)
				}
				wantColor := "blinker_green"
				if state == "both" {
					wantColor = "blinker_red"
				}
				lit := false
				for i, b := range indicator.brightness {
					lit = lit || b > 0
					if indicator.colors[i] != wantColor {
						t.Errorf("indicator color %q, want %q", indicator.colors[i], wantColor)
					}
				}
				if !lit || indicator.brightness[len(indicator.brightness)-1] != 0 {
					t.Error("indicator must light up and finish off")
				}
			})
		})
	}
}

type delayedBlinkerIO struct {
	*mockHardwareIO
	calls   int
	times   []time.Time
	delayAt int
	delay   time.Duration
}

func (m *delayedBlinkerIO) PlayPwmCue(cue int) error {
	if cue == 12 {
		m.calls++
		m.times = append(m.times, time.Now())
		if m.delayAt == 0 || m.calls == m.delayAt {
			// A blocked ioctl or a stalled process must not cause a burst of
			// historical cues when execution resumes.
			time.Sleep(m.delay)
		}
	}
	return m.mockHardwareIO.PlayPwmCue(cue)
}

func TestBlinkerCadenceDoesNotDriftWithIOTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		v, hw, _ := newTestVehicleSystem()
		delayed := &delayedBlinkerIO{mockHardwareIO: hw, delay: 10 * time.Millisecond}
		v.io = delayed
		if err := v.handleBlinkerRequest("both"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10*blinkerInterval + 100*time.Millisecond)
		v.stopBlinker()
		if len(delayed.times) != 11 {
			t.Fatalf("got %d cues, want 11", len(delayed.times))
		}
		for i, at := range delayed.times {
			if elapsed := at.Sub(delayed.times[0]); elapsed != time.Duration(i)*blinkerInterval {
				t.Errorf("cue %d at %v, want %v", i, elapsed, time.Duration(i)*blinkerInterval)
			}
		}
	})
}

func TestBlinkerDoesNotReplayMissedCycles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		v, hw, _ := newTestVehicleSystem()
		delayed := &delayedBlinkerIO{mockHardwareIO: hw, delayAt: 2, delay: 4 * blinkerInterval}
		v.io = delayed
		if err := v.handleBlinkerRequest("both"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(7 * blinkerInterval)
		v.stopBlinker()
		for i := 1; i < len(delayed.times); i++ {
			if gap := delayed.times[i].Sub(delayed.times[i-1]); gap < blinkerInterval {
				t.Errorf("cues %d/%d only %v apart", i-1, i, gap)
			}
		}
	})
}
