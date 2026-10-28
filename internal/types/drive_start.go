package types

// DriveStartInputs is one bounded read of the battery and ECU publications.
// Missing fields do not establish an active supply or a healthy controller.
type DriveStartInputs struct {
	ActiveBattery bool
	ECUHealthy    bool
}
