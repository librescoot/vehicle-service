package core

// Fault codes reported by vehicle-service under the "vehicle" group. The
// numbers are a contract the moment a release raises one: telemetry and the
// dashboard read them off vehicle:fault and events:faults, so this list is
// append only and a number is never reused for a different condition.
//
// Bands, so later additions do not have to interleave:
//
//	1 to 9   actuation outcomes and output write failures
//	10 to 19 input and sensing path failures
//	20 to 29 service lifecycle and internal state
//	30+      unassigned
//
// Codes with no raise site yet are declared anyway, so a later change cannot
// quietly claim a number that was already spoken for.
//
// The description passed alongside a code is the whole user-facing fault: the
// dashboard renders the stream description verbatim at critical severity and
// has no per-code table. Name the specific failure, include the underlying
// error, keep it to roughly a line, no component prefix. The first raise wins,
// because raising an already-active code writes nothing, so every description
// has to stand on its own.
const (
	// FaultSteeringLockNotConfirmed: lock retries exhausted, the sensor still
	// reads unlocked.
	FaultSteeringLockNotConfirmed = 1
	// FaultSteeringUnlockFailed: unlock past the retry burst, or the actuation
	// itself failed.
	FaultSteeringUnlockFailed = 2
	// FaultEnginePowerOutput: the engine_power output write failed.
	FaultEnginePowerOutput = 3
	// FaultDashboardPowerOutput: the dashboard_power output write failed.
	FaultDashboardPowerOutput = 4
	// FaultEngineBrakeOutput: the engine_brake output write failed.
	FaultEngineBrakeOutput = 5
	// FaultSeatboxLockOutput: the seatbox_lock output write failed.
	FaultSeatboxLockOutput = 6

	// FaultInputDeviceUnreadable: the gpio-keys event device cannot be read.
	FaultInputDeviceUnreadable = 10

	// FaultStateRestoreRefused: the saved state could not be restored and the
	// vehicle was forced to stand-by.
	FaultStateRestoreRefused = 20
)

// ownedFaultCodes is every code this service may raise. Used by the startup
// reconcile, which is the only place that needs the whole set.
var ownedFaultCodes = []int{
	FaultSteeringLockNotConfirmed,
	FaultSteeringUnlockFailed,
	FaultEnginePowerOutput,
	FaultDashboardPowerOutput,
	FaultEngineBrakeOutput,
	FaultSeatboxLockOutput,
	FaultInputDeviceUnreadable,
	FaultStateRestoreRefused,
}

// outputFaultCodes maps a GPIO output channel to the fault code raised when a
// write to it fails. Channels absent from the map are not fault reported:
// writeOutput passes them through untouched. A channel earns an entry only when
// a stuck line is something a rider or a mechanic can act on, which is why the
// blinkers, the horn and the lock solenoids are not here.
var outputFaultCodes = map[string]int{
	"engine_power":    FaultEnginePowerOutput,
	"dashboard_power": FaultDashboardPowerOutput,
	"engine_brake":    FaultEngineBrakeOutput,
}
