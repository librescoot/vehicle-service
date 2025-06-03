package hardware

const (
	PwmLedCount = 8
	MaxFadeSize = 4096
	MaxCues     = 16

	FadesDir = "/usr/share/led-curves/fades"
	CuesDir  = "/usr/share/led-curves/cues"

	GpioKeysInput = "/dev/input/by-path/platform-gpio-keys-event"
)

var DoMappings = map[string]struct {
	Chip int
	Line int
}{
	"seatbox_lock":         {2, 10},
	"horn":                 {2, 9},
	"engine_brake":         {2, 11},
	"handlebar_lock_close": {4, 6},
	"handlebar_lock_open":  {4, 7},
}
