package core

import "testing"

// keycardCounts builds the system-hash entries keycard-service publishes.
// A nil count leaves the field out, which is what the hash actually looks
// like before keycard-service has reached its first publish.
func keycardCounts(master, authorized *string) map[string]string {
	fields := map[string]string{}
	if master != nil {
		fields["system/keycard-master-count"] = *master
	}
	if authorized != nil {
		fields["system/keycard-authorized-count"] = *authorized
	}
	return fields
}

func strptr(s string) *string { return &s }

func TestUsb0GateState(t *testing.T) {
	tests := []struct {
		name       string
		policy     string
		master     *string
		authorized *string
		want       usb0Gate
	}{
		{
			name:       "always-on short-circuits before reading counts",
			policy:     "always-on",
			master:     nil,
			authorized: nil,
			want:       usb0GateOpen,
		},
		{
			name:       "counts absent is unknown, not zero",
			policy:     "auto",
			master:     nil,
			authorized: nil,
			want:       usb0GateUnknown,
		},
		{
			name:       "one count absent is still unknown",
			policy:     "auto",
			master:     strptr("1"),
			authorized: nil,
			want:       usb0GateUnknown,
		},
		{
			name:       "unparseable count is unknown",
			policy:     "auto",
			master:     strptr("1"),
			authorized: strptr("not-a-number"),
			want:       usb0GateUnknown,
		},
		{
			name:       "no cards paired keeps the gate open",
			policy:     "auto",
			master:     strptr("0"),
			authorized: strptr("0"),
			want:       usb0GateOpen,
		},
		{
			name:       "master alone is not enough",
			policy:     "auto",
			master:     strptr("1"),
			authorized: strptr("0"),
			want:       usb0GateOpen,
		},
		{
			name:       "one authorized alone is not enough",
			policy:     "auto",
			master:     strptr("0"),
			authorized: strptr("1"),
			want:       usb0GateOpen,
		},
		{
			name:       "master plus authorized closes the gate",
			policy:     "auto",
			master:     strptr("1"),
			authorized: strptr("1"),
			want:       usb0GateClosed,
		},
		{
			name:       "two authorized close the gate without a master",
			policy:     "auto",
			master:     strptr("0"),
			authorized: strptr("2"),
			want:       usb0GateClosed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			system, _, mockRedis := newTestVehicleSystem()
			system.usb0Policy = tt.policy
			mockRedis.hashFields = keycardCounts(tt.master, tt.authorized)

			if got := system.usb0GateState(); got != tt.want {
				t.Errorf("usb0GateState() = %v, want %v", got, tt.want)
			}
		})
	}
}

// An unresolved gate must not be published: the boot failsafe timer reads the
// absence of system[usb0-gate] as "vehicle-service never decided" and opens
// usb0 itself. Publishing "closed" on a guess would disarm it.
func TestRecordUsb0GateSkipsUnknown(t *testing.T) {
	system, _, mockRedis := newTestVehicleSystem()

	system.recordUsb0Gate(usb0GateUnknown)
	if len(mockRedis.usb0GateSets) != 0 {
		t.Fatalf("recorded %v for an unresolved gate, want nothing", mockRedis.usb0GateSets)
	}

	system.recordUsb0Gate(usb0GateClosed)
	system.recordUsb0Gate(usb0GateOpen)
	if got, want := mockRedis.usb0GateSets, []bool{false, true}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("usb0GateSets = %v, want %v", got, want)
	}
}

// applyUsb0Gate only ever raises the link. A closed gate is left to setPower,
// which already tracks dashboard_power.
func TestApplyUsb0Gate(t *testing.T) {
	tests := []struct {
		name  string
		state usb0Gate
		want  []bool
	}{
		{name: "open raises the link", state: usb0GateOpen, want: []bool{true}},
		{name: "closed leaves the link to setPower", state: usb0GateClosed, want: nil},
		{name: "unknown leaves the link alone", state: usb0GateUnknown, want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			system, mockIO, _ := newTestVehicleSystem()
			system.applyUsb0Gate(tt.state)

			if len(mockIO.usb0Enabled) != len(tt.want) {
				t.Fatalf("SetUsb0Enabled calls = %v, want %v", mockIO.usb0Enabled, tt.want)
			}
			for i := range tt.want {
				if mockIO.usb0Enabled[i] != tt.want[i] {
					t.Errorf("SetUsb0Enabled call %d = %v, want %v", i, mockIO.usb0Enabled[i], tt.want[i])
				}
			}
		})
	}
}

// setPower mirrors the link to dashboard_power unless the gate is open. An
// unknown gate follows dashboard_power like a closed one: the cost is usb0
// staying down for the few seconds the resolver needs, where the old
// behaviour asserted the link up on a guess and handed every reboot a window
// with usb0 addressed and valkey reachable on it.
func TestSetPowerUsb0FollowsGate(t *testing.T) {
	tests := []struct {
		name           string
		policy         string
		master         *string
		authorized     *string
		dashboardPower bool
		wantUsb0       bool
	}{
		{
			name:           "closed gate, dashboard off, link goes down",
			policy:         "auto",
			master:         strptr("1"),
			authorized:     strptr("1"),
			dashboardPower: false,
			wantUsb0:       false,
		},
		{
			name:           "closed gate, dashboard on, link goes up",
			policy:         "auto",
			master:         strptr("1"),
			authorized:     strptr("1"),
			dashboardPower: true,
			wantUsb0:       true,
		},
		{
			name:           "open gate holds the link up with the dashboard off",
			policy:         "auto",
			master:         strptr("0"),
			authorized:     strptr("0"),
			dashboardPower: false,
			wantUsb0:       true,
		},
		{
			name:           "always-on holds the link up with the dashboard off",
			policy:         "always-on",
			master:         strptr("1"),
			authorized:     strptr("1"),
			dashboardPower: false,
			wantUsb0:       true,
		},
		{
			name:           "unknown gate, dashboard off, link goes down",
			policy:         "auto",
			master:         nil,
			authorized:     nil,
			dashboardPower: false,
			wantUsb0:       false,
		},
		{
			name:           "unknown gate, dashboard on, link goes up",
			policy:         "auto",
			master:         nil,
			authorized:     nil,
			dashboardPower: true,
			wantUsb0:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			system, mockIO, mockRedis := newTestVehicleSystem()
			system.usb0Policy = tt.policy
			mockRedis.hashFields = keycardCounts(tt.master, tt.authorized)

			if err := system.setPower("dashboard_power", tt.dashboardPower); err != nil {
				t.Fatalf("setPower: %v", err)
			}

			if len(mockIO.usb0Enabled) != 1 {
				t.Fatalf("SetUsb0Enabled calls = %v, want exactly one", mockIO.usb0Enabled)
			}
			if mockIO.usb0Enabled[0] != tt.wantUsb0 {
				t.Errorf("usb0 set to %v, want %v", mockIO.usb0Enabled[0], tt.wantUsb0)
			}
		})
	}
}

// An unresolved gate must not be recorded from setPower either.
func TestSetPowerDoesNotRecordUnknownGate(t *testing.T) {
	system, _, mockRedis := newTestVehicleSystem()
	system.usb0Policy = "auto"
	mockRedis.hashFields = keycardCounts(nil, nil)

	if err := system.setPower("dashboard_power", false); err != nil {
		t.Fatalf("setPower: %v", err)
	}
	if len(mockRedis.usb0GateSets) != 0 {
		t.Errorf("recorded %v for an unresolved gate, want nothing", mockRedis.usb0GateSets)
	}
}
