package core

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

const (
	pwmLEDModuleName  = "imx_pwm_led"
	pwmLEDDeviceCount = 8
)

type pwmDriverOps struct {
	moduleDir  string
	devicePath func(int) string
	stat       func(string) (os.FileInfo, error)
	run        func(string, ...string) error
	sleep      func(time.Duration)
}

func systemPWMDriverOps() pwmDriverOps {
	return pwmDriverOps{
		moduleDir: "/sys/module/imx_pwm_led",
		devicePath: func(index int) string {
			return fmt.Sprintf("/dev/pwm_led%d", index)
		},
		stat: os.Stat,
		run: func(name string, args ...string) error {
			return exec.Command(name, args...).Run()
		},
		sleep: time.Sleep,
	}
}

// existingPWMDriverReady deliberately requires the complete expected device
// surface. A loaded module alone is not enough: udev may still be creating
// nodes, or probe may have failed partway through the eight PWM channels.
//
// The driver's module parameters use mode 0 and are therefore not exposed in
// sysfs. They cannot be verified without changing the kernel module; the module
// marker plus all eight character devices is the strongest live check available.
func existingPWMDriverReady(ops pwmDriverOps) (bool, string) {
	if _, err := ops.stat(ops.moduleDir); err != nil {
		return false, fmt.Sprintf("module unavailable: %v", err)
	}

	for index := 0; index < pwmLEDDeviceCount; index++ {
		path := ops.devicePath(index)
		info, err := ops.stat(path)
		if err != nil {
			return false, fmt.Sprintf("%s unavailable: %v", path, err)
		}
		if info.Mode()&os.ModeCharDevice == 0 {
			return false, fmt.Sprintf("%s is not a character device", path)
		}
	}
	return true, ""
}

// preparePWMDriver reuses a fully verified driver. Any incomplete or mismatched
// state falls back to the historical unload/reload sequence. The bool reports
// whether the existing instance was reused; removeErr is diagnostic only,
// matching the old non-fatal rmmod behavior.
func preparePWMDriver(ops pwmDriverOps) (reused bool, reason string, removeErr error, err error) {
	if ready, notReadyReason := existingPWMDriverReady(ops); ready {
		return true, "", nil, nil
	} else {
		reason = notReadyReason
	}

	removeErr = ops.run("rmmod", pwmLEDModuleName)
	if loadErr := ops.run("modprobe", pwmLEDModuleName); loadErr != nil {
		return false, reason, removeErr, fmt.Errorf("failed to load PWM LED module: %w", loadErr)
	}

	// Retain the existing post-modprobe wait. ImxPwmLed.Init performs the final
	// per-node opens and reports a fatal error if no channel can initialize.
	for attempt := 0; attempt < 10; attempt++ {
		if _, statErr := ops.stat(ops.devicePath(0)); statErr == nil {
			break
		}
		ops.sleep(100 * time.Millisecond)
	}
	return false, reason, removeErr, nil
}
