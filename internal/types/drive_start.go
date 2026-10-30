package types

// DriveStartInputs is one bounded read of both batteries' presence and state.
// Missing fields do not establish an active battery.
type DriveStartInputs struct {
	ActiveBattery bool
}
