package fsm

import (
	"time"

	"github.com/librescoot/librefsm"
)

// LockIgnoreSeatboxRequest is owned by one synchronous dispatch. Accepted is
// meaningful only together with a nil SendSync error; it is not physical locking.
type LockIgnoreSeatboxRequest struct {
	Deadline time.Time
	Accepted bool
}

// acceptLockIgnoreSeatbox is the sole acceptance/expiry boundary, before any
// state exit or timer cancellation. Once accepted, finish normal shutdown even
// if the caller's deadline passes during exit; a lost reply is an unknown outcome.
func acceptLockIgnoreSeatbox(c *librefsm.Context) bool {
	if c.Event == nil {
		return false
	}
	r, ok := c.Event.Payload.(*LockIgnoreSeatboxRequest)
	if !ok || r == nil || !time.Now().Before(r.Deadline) {
		return false
	}
	r.Accepted = true
	return true
}
