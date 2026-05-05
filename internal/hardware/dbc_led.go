package hardware

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

const (
	DbcLedI2CDevice = "/dev/i2c-2"
	dbcLedI2CAddr   = 0x30
	i2cSlaveForce   = 0x0706 // I2C_SLAVE_FORCE ioctl (bypasses kernel driver claim)
)

// LP5562 register frame layout starting at register 0x00.
// Format: [reg_addr, ctrl, cfg, ch0, ch1, ch2, t0, t1, t2, mode]
//
// dbcLedColors maps a colour name to the three channel PWM bytes at full
// brightness. The channel-to-colour mapping is determined by the chip wiring
// (not RGB order); these triples were taken from the original canonical
// frames so the visible colour matches what the boot script produces.
// blinker_green / blinker_red carry a ~10% amber tint so the blinker
// indicator visually leans toward the actual amber lamp colour.
var dbcLedColors = map[string][3]byte{
	"green":         {0x00, 0xff, 0x00},
	"red":           {0x00, 0x00, 0xff},
	"amber":         {0xff, 0x00, 0x00},
	"blinker_green": {0x1a, 0xff, 0x00},
	"blinker_red":   {0x1a, 0x00, 0xff},
}

var dbcLedOffFrame = []byte{0x00, 0x40, 0x3f, 0x00, 0x00, 0x00, 0x32, 0x32, 0x32, 0x01}

type DbcLed struct {
	file *os.File
}

func NewDbcLed(path string) (*DbcLed, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.IoctlSetInt(int(f.Fd()), i2cSlaveForce, dbcLedI2CAddr); err != nil {
		f.Close()
		return nil, fmt.Errorf("I2C_SLAVE ioctl: %w", err)
	}
	return &DbcLed{file: f}, nil
}

func (d *DbcLed) Set(color string, brightness uint8) error {
	if color == "off" || brightness == 0 {
		_, err := d.file.Write(dbcLedOffFrame)
		return err
	}
	rgb, ok := dbcLedColors[color]
	if !ok {
		return fmt.Errorf("unknown color: %s", color)
	}
	scale := func(b byte) byte { return byte(uint16(b) * uint16(brightness) / 255) }
	frame := []byte{
		0x00, 0xc0, 0x3f,
		scale(rgb[0]), scale(rgb[1]), scale(rgb[2]),
		0xaf, 0xaf, 0xaf, 0x61,
	}
	_, err := d.file.Write(frame)
	return err
}

func (d *DbcLed) Close() {
	d.file.Close()
}
