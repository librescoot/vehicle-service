package core

import (
	"strconv"
	"time"
)

// usb0Gate is the resolved answer to "should usb0 stay up regardless of
// dashboard_power right now".
//
// The gate exists because usb0 is the recovery path into a scooter whose
// keycards do not work. Until enough cards are paired to survive a bad
// pairing, the link stays up so the installer can reach the board; after
// that it follows dashboard_power and is down whenever the DBC is off.
//
// Threshold: (master >= 1 AND authorized >= 1) OR authorized >= 2.
// keycard-service publishes both counts to the "system" hash.
type usb0Gate int

const (
	// usb0GateUnknown means the keycard counts have not been published yet,
	// so there is no honest answer. valkey does not persist across a reboot
	// and keycard-service publishes at the top of its Run(), so this is the
	// normal state for the first seconds of every boot.
	usb0GateUnknown usb0Gate = iota
	// usb0GateOpen means keep usb0 up: the user picked always-on, or too few
	// keycards are paired to risk taking the recovery path away.
	usb0GateOpen
	// usb0GateClosed means usb0 follows dashboard_power.
	usb0GateClosed
)

func (g usb0Gate) String() string {
	switch g {
	case usb0GateOpen:
		return "open"
	case usb0GateClosed:
		return "closed"
	default:
		return "unknown"
	}
}

const (
	// usb0GateWait bounds how long resolveUsb0Gate waits for keycard-service
	// to publish its pairing counts before giving up and opening the gate.
	// keycard-service reaches its first publish in ~0.7s on an idle board and
	// ~5s under boot-time CPU contention, so only a keycard-service that is
	// not coming up at all reaches this.
	//
	// librescoot-usb0-failsafe.timer in meta-librescoot raises usb0 itself if
	// no decision has been recorded by 120s into the boot. Raising this
	// constant means raising that deadline too, or the failsafe starts firing
	// on healthy boots that are still waiting here.
	usb0GateWait = 30 * time.Second
	// usb0GatePollInterval is how often that wait re-reads the counts.
	usb0GatePollInterval = 500 * time.Millisecond
)

// usb0GateState resolves the gate from the current policy and the keycard
// pairing counts.
func (v *VehicleSystem) usb0GateState() usb0Gate {
	v.mu.RLock()
	policy := v.usb0Policy
	v.mu.RUnlock()
	if policy != "auto" {
		return usb0GateOpen
	}

	master, masterKnown := v.readKeycardCount("keycard-master-count")
	authorized, authorizedKnown := v.readKeycardCount("keycard-authorized-count")
	if !masterKnown || !authorizedKnown {
		return usb0GateUnknown
	}

	if master >= 1 && authorized >= 1 {
		return usb0GateClosed
	}
	if authorized >= 2 {
		return usb0GateClosed
	}
	return usb0GateOpen
}

// readKeycardCount reads one keycard pairing count from the system hash. The
// bool reports whether the field was there at all, which is the whole point:
// absent is not zero. Early in a boot the counts have simply not been written
// yet, and treating that as "no cards paired" answers the gate question with
// data that has not arrived.
func (v *VehicleSystem) readKeycardCount(field string) (int, bool) {
	raw, err := v.redis.GetHashField("system", field)
	if err != nil {
		v.logger.Warnf("Failed to read %s from the system hash: %v", field, err)
		return 0, false
	}
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		v.logger.Warnf("Bad %s in system hash: %q (%v)", field, raw, err)
		return 0, false
	}
	return n, true
}

// recordUsb0Gate publishes a resolved gate decision to system[usb0-gate].
//
// An unresolved gate is deliberately not published. The boot failsafe timer
// reads the absence of that field as "vehicle-service never got far enough to
// decide" and raises usb0 itself, so publishing a guess here would disarm the
// one mechanism that covers a vehicle-service which is dead or wedged.
func (v *VehicleSystem) recordUsb0Gate(state usb0Gate) {
	if state == usb0GateUnknown {
		return
	}
	if err := v.redis.SetUsb0Gate(state == usb0GateOpen); err != nil {
		v.logger.Warnf("Failed to record the usb0 gate decision: %v", err)
	}
}

// applyUsb0Gate drives the link to match a resolved gate state and records
// the decision. A closed gate leaves the link alone: setPower owns it from
// there and it already tracks dashboard_power.
func (v *VehicleSystem) applyUsb0Gate(state usb0Gate) {
	if state == usb0GateOpen {
		if err := v.io.SetUsb0Enabled(true); err != nil {
			v.logger.Warnf("Failed to bring usb0 up: %v", err)
		}
	}
	v.recordUsb0Gate(state)
}

// startUsb0GateResolver applies the gate at startup, waiting for the keycard
// counts in the background if they are not published yet.
//
// The wait is the normal path. keycard-service is ordered after this service
// and loses the race for the CPU during boot, so at the point Start() gets
// here the counts are usually still absent.
func (v *VehicleSystem) startUsb0GateResolver() {
	v.mu.RLock()
	policy := v.usb0Policy
	v.mu.RUnlock()

	state := v.usb0GateState()
	if state != usb0GateUnknown {
		v.logger.Infof("usb0 policy=%s, gate=%s", policy, state)
		v.applyUsb0Gate(state)
		return
	}

	v.logger.Infof("usb0 policy=%s, gate=unknown, waiting up to %v for keycard pairing counts", policy, usb0GateWait)
	v.mu.Lock()
	v.usb0GateDone = make(chan struct{})
	done := v.usb0GateDone
	v.mu.Unlock()
	go v.resolveUsb0Gate(done)
}

// resolveUsb0Gate polls until the keycard counts appear, then applies the
// gate. On timeout it opens the gate: a keycard-service that never published
// is exactly the case where locking the recovery path away is worst.
//
// It can only ever open the gate, never close it. Closing is setPower's job
// on the next dashboard_power transition. That asymmetry is deliberate: an
// installer session must not lose usb0 underneath it because the second
// keycard got paired halfway through.
func (v *VehicleSystem) resolveUsb0Gate(done <-chan struct{}) {
	ticker := time.NewTicker(usb0GatePollInterval)
	defer ticker.Stop()
	deadline := time.NewTimer(usb0GateWait)
	defer deadline.Stop()

	for {
		select {
		case <-done:
			return
		case <-deadline.C:
			v.logger.Warnf("Keycard pairing counts never appeared after %v, opening the usb0 gate", usb0GateWait)
			v.applyUsb0Gate(usb0GateOpen)
			return
		case <-ticker.C:
			state := v.usb0GateState()
			if state == usb0GateUnknown {
				continue
			}
			v.logger.Infof("usb0 gate resolved to %s", state)
			v.applyUsb0Gate(state)
			return
		}
	}
}

// stopUsb0GateResolver releases a resolver still waiting on the counts.
func (v *VehicleSystem) stopUsb0GateResolver() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.usb0GateDone != nil {
		close(v.usb0GateDone)
		v.usb0GateDone = nil
	}
}
