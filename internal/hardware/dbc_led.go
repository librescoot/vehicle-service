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

// LP5662 register bytes starting at register 0x00 for each color state.
// Format: [reg_addr, ctrl, cfg, r, g, b, t0, t1, t2, mode]
var dbcLedData = map[string][]byte{
	"green": {0x00, 0xc0, 0x3f, 0x00, 0xff, 0x00, 0xaf, 0xaf, 0xaf, 0x61},
	"red":   {0x00, 0xc0, 0x3f, 0x00, 0x00, 0xff, 0xaf, 0xaf, 0xaf, 0x61},
	"amber": {0x00, 0xc0, 0x3f, 0xff, 0x00, 0x00, 0xaf, 0xaf, 0xaf, 0x61},
	"off":   {0x00, 0x40, 0x3f, 0x00, 0x00, 0x00, 0x32, 0x32, 0x32, 0x01},
}

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

func (d *DbcLed) Set(color string) error {
	data, ok := dbcLedData[color]
	if !ok {
		return fmt.Errorf("unknown color: %s", color)
	}
	_, err := d.file.Write(data)
	return err
}

func (d *DbcLed) Close() {
	d.file.Close()
}
