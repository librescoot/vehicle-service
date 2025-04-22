package hardware

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	pwmLedConfigure = 0x00007540 // _IO('u', 0x40)
	pwmLedOpenFade  = 0x00007541 // _IO('u', 0x41)
	pwmLedOpenCue   = 0x00007542 // _IO('u', 0x42)
	pwmLedPlayFade  = 0x00007545 // _IO('u', 0x45)
	pwmLedPlayCue   = 0x00007546 // _IO('u', 0x46)
	pwmLedSetActive = 0x00007549 // _IO('u', 0x49)
	pwmLedSetDuty   = 0x0000754A // _IO('u', 0x4A)
	pwmLedSetAdapt  = 0x0000754C // _IO('u', 0x4C)

	// PWM Configuration constants matching kernel module
	pwmPeriod    = 12000 // pwm_period=12000
	pwmPrescaler = 0     // default
	pwmInvert    = 0     // default
	pwmRepeat    = 3     // pwm_repeat=3
)

// PWM configuration bits as defined in kernel module
const (
	pwmCfgBitPeriod    = 0
	pwmCfgBitPrescaler = 16
	pwmCfgBitInvert    = 28
	pwmCfgBitRepeat    = 29
)

// Cue action type as defined in kernel module
type CueActionType uint8

const (
	CueActionTypeFade CueActionType = iota // Value = fade index
	CueActionTypeDuty                      // Value = duty cycle
)

// Cue action structure matching kernel module format
type CueAction struct {
	LED   uint8         // 5 bits
	Type  CueActionType // 4 bits
	Value uint16        // 16 bits
}

type LedDevice struct {
	fd          int
	lock        sync.Mutex
	isAdaptive  bool
	playingFade int
}

type ImxPwmLed struct {
	devices    [PwmLedCount]*LedDevice
	globalLock sync.Mutex
	enabled    bool
}

func NewImxPwmLed() *ImxPwmLed {
	return &ImxPwmLed{}
}

func (l *ImxPwmLed) loadCues() error {
	if !l.enabled || l.devices[0] == nil {
		return fmt.Errorf("LED system not enabled")
	}

	entries, err := os.ReadDir(CuesDir)
	if err != nil {
		return fmt.Errorf("failed to read cues directory: %w", err)
	}

	device := l.devices[0]
	device.lock.Lock()
	defer device.lock.Unlock()

	for _, entry := range entries {
		var idx int
		if _, err := fmt.Sscanf(entry.Name(), "cue%d", &idx); err != nil {
			continue
		}

		if idx < 0 || idx >= MaxCues {
			continue
		}

		file, err := os.Open(CuesDir + "/" + entry.Name())
		if err != nil {
			log.Printf("Failed to open cue file %s: %v", entry.Name(), err)
			continue
		}
		defer file.Close()

		// Get file size for logging
		fileInfo, err := file.Stat()
		if err != nil {
			log.Printf("Failed to get file info for %s: %v", entry.Name(), err)
			continue
		}

		fileSize := fileInfo.Size()
		if fileSize%4 != 0 {
			log.Printf("Invalid cue data size %d in %s, must be multiple of 4", fileSize, entry.Name())
			continue
		}

		log.Printf("Loading cue %d from %s (%d bytes, %d actions)", idx, entry.Name(), fileSize, fileSize/4)

		err = unix.IoctlSetInt(device.fd, pwmLedOpenCue, idx)
		if err != nil {
			log.Printf("Failed to open cue %d: %v", idx, err)
			continue
		}

		// First pass to validate and log actions
		buf := make([]byte, 4)
		for i := int64(0); i < fileSize; i += 4 {
			_, err := file.Read(buf)
			if err != nil {
				log.Printf("Failed to read cue data: %v", err)
				break
			}

			action := binary.LittleEndian.Uint32(buf)
			led := uint8(action & 0x1F)
			actionType := CueActionType((action >> 8) & 0x0F)
			value := uint16(action >> 16)

			log.Printf("  Action %d: LED=%d, Type=%d, Value=%d", i/4, led, actionType, value)
		}

		// Reset file position for actual writing
		_, err = file.Seek(0, 0)
		if err != nil {
			log.Printf("Failed to seek file: %v", err)
			continue
		}

		// Write data in chunks
		buf = make([]byte, 1024)
		for {
			n, err := file.Read(buf)
			if err != nil && err != io.EOF {
				log.Printf("Failed to read cue data: %v", err)
				break
			}
			if n == 0 {
				break
			}

			// Write the chunk
			offset := 0
			for offset < n {
				written, err := unix.Write(device.fd, buf[offset:n])
				if err != nil {
					log.Printf("Failed to write cue data: %v", err)
					break
				}
				offset += written
			}

			if err == io.EOF {
				break
			}
		}
	}

	return nil
}

func (l *ImxPwmLed) configurePWM(device *LedDevice) error {
	// Construct configuration value according to kernel module format
	config := uint32(pwmPeriod) |
		(uint32(pwmPrescaler) << pwmCfgBitPrescaler) |
		(uint32(pwmInvert) << pwmCfgBitInvert) |
		(uint32(pwmRepeat) << pwmCfgBitRepeat)

	err := unix.IoctlSetInt(device.fd, pwmLedConfigure, int(config))
	if err != nil {
		return fmt.Errorf("failed to configure PWM: %v", err)
	}
	return nil
}

func (l *ImxPwmLed) Init() error {
	log.Printf("Initializing PWM LED module")
	if _, err := os.Stat("/sys/module/imx_pwm_led"); os.IsNotExist(err) {
		log.Printf("Error: PWM LED module not loaded")
		return fmt.Errorf("PWM LED module not loaded")
	}

	l.globalLock.Lock()
	defer l.globalLock.Unlock()

	// Initialize LED devices
	for i := 0; i < PwmLedCount; i++ {
		devPath := fmt.Sprintf("/dev/pwm_led%d", i)
		log.Printf("Opening LED device: %s", devPath)
		fd, err := unix.Open(devPath, unix.O_RDWR, 0)
		if err != nil {
			log.Printf("Failed to open LED device %s: %v", devPath, err)
			continue
		}

		device := &LedDevice{
			fd:          fd,
			isAdaptive:  false,
			playingFade: -1,
		}

		// Configure PWM parameters first
		if err := l.configurePWM(device); err != nil {
			log.Printf("Failed to configure PWM for LED %d: %v", i, err)
			unix.Close(fd)
			continue
		}

		// Add delay after PWM configuration
		time.Sleep(10 * time.Millisecond)

		// Set adaptive mode for non-blinker LEDs
		isBlinker := i == 3 || i == 4 || i == 6 || i == 7
		if !isBlinker {
			device.isAdaptive = true
			log.Printf("Setting LED %d to adaptive mode", i)
			err = unix.IoctlSetInt(fd, pwmLedSetAdapt, 1)
			if err != nil {
				log.Printf("Failed to set adaptive mode for LED %d: %v", i, err)
			}
			// Add delay after setting adaptive mode
			time.Sleep(5 * time.Millisecond)
		}

		l.devices[i] = device
		// Add delay before activation
		time.Sleep(5 * time.Millisecond)
		if err := l.setActive(i, true); err != nil {
			log.Printf("Failed to activate LED %d: %v", i, err)
		} else {
			log.Printf("Successfully activated LED %d", i)
		}
		// Add delay after activation
		time.Sleep(10 * time.Millisecond)
	}

	if l.devices[0] != nil {
		l.enabled = true
		log.Printf("Loading LED patterns...")
		if err := l.loadFades(); err != nil {
			log.Printf("Failed to load fades: %v", err)
			return fmt.Errorf("failed to load fades: %w", err)
		}
		if err := l.loadCues(); err != nil {
			log.Printf("Failed to load cues: %v", err)
			return fmt.Errorf("failed to load cues: %w", err)
		}
		log.Printf("LED patterns loaded successfully")
		if err := l.PlayCue(0); err != nil {
			log.Printf("Failed to play initial cue: %v", err)
		} else {
			log.Printf("Initial LED cue played successfully")
		}
		return nil
	}

	log.Printf("Failed to initialize any LED devices")
	return fmt.Errorf("failed to initialize any LED devices")
}

func (l *ImxPwmLed) PlayCue(idx int) error {
	if !l.enabled || l.devices[0] == nil {
		return fmt.Errorf("LED system not enabled")
	}

	if idx < 0 || idx >= MaxCues {
		return fmt.Errorf("invalid cue index: %d", idx)
	}

	device := l.devices[0]
	device.lock.Lock()
	defer device.lock.Unlock()

	log.Printf("Playing LED cue %d", idx)
	err := unix.IoctlSetInt(device.fd, pwmLedPlayCue, idx)
	if err != nil {
		log.Printf("Failed to play cue %d: %v", idx, err)
		return fmt.Errorf("failed to play cue %d: %v", idx, err)
	}
	log.Printf("Successfully played LED cue %d", idx)
	return nil
}

func (l *ImxPwmLed) loadFades() error {
	if !l.enabled || l.devices[0] == nil {
		return fmt.Errorf("LED system not enabled")
	}

	entries, err := os.ReadDir(FadesDir)
	if err != nil {
		return fmt.Errorf("failed to read fades directory: %w", err)
	}

	device := l.devices[0]
	device.lock.Lock()
	defer device.lock.Unlock()

	for _, entry := range entries {
		var idx int
		if _, err := fmt.Sscanf(entry.Name(), "fade%d", &idx); err != nil {
			continue
		}

		if idx < 0 || idx >= MaxFadeSize {
			continue
		}

		file, err := os.Open(FadesDir + "/" + entry.Name())
		if err != nil {
			log.Printf("Failed to open fade file %s: %v", entry.Name(), err)
			continue
		}
		defer file.Close()

		// Get file size for logging
		fileInfo, err := file.Stat()
		if err != nil {
			log.Printf("Failed to get file info for %s: %v", entry.Name(), err)
			continue
		}

		fileSize := fileInfo.Size()
		if fileSize%2 != 0 {
			log.Printf("Invalid fade data size %d in %s, must be multiple of 2", fileSize, entry.Name())
			continue
		}

		if fileSize > MaxFadeSize {
			log.Printf("Fade data too large in %s: %d > %d", entry.Name(), fileSize, MaxFadeSize)
			continue
		}

		log.Printf("Loading fade %d from %s (%d bytes, %d samples)", idx, entry.Name(), fileSize, fileSize/2)

		// First pass to log samples
		buf := make([]byte, 2)
		for i := 0; i < min(8, int(fileSize/2)); i++ {
			_, err := file.Read(buf)
			if err != nil {
				log.Printf("Failed to read fade data: %v", err)
				break
			}
			sample := binary.LittleEndian.Uint16(buf)
			log.Printf("  Sample %d: %d", i, sample)
		}

		// Reset file position for actual writing
		_, err = file.Seek(0, 0)
		if err != nil {
			log.Printf("Failed to seek file: %v", err)
			continue
		}

		err = unix.IoctlSetInt(device.fd, pwmLedOpenFade, idx)
		if err != nil {
			log.Printf("Failed to open fade %d: %v", idx, err)
			continue
		}

		// Write data in chunks
		buf = make([]byte, 1024)
		for {
			n, err := file.Read(buf)
			if err != nil && err != io.EOF {
				log.Printf("Failed to read fade data: %v", err)
				break
			}
			if n == 0 {
				break
			}

			// Write the chunk
			offset := 0
			for offset < n {
				written, err := unix.Write(device.fd, buf[offset:n])
				if err != nil {
					log.Printf("Failed to write fade data: %v", err)
					break
				}
				offset += written
			}

			if err == io.EOF {
				break
			}
		}
	}

	return nil
}

// Helper function for min
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (l *ImxPwmLed) setActive(ch int, active bool) error {
	if ch < 0 || ch >= PwmLedCount || l.devices[ch] == nil {
		return fmt.Errorf("invalid LED channel: %d", ch)
	}

	device := l.devices[ch]
	device.lock.Lock()
	defer device.lock.Unlock()

	var value int
	if active {
		value = 1
	}

	err := unix.IoctlSetInt(device.fd, pwmLedSetActive, value)
	if err != nil {
		return fmt.Errorf("failed to set active state: %v", err)
	}

	return nil
}

func (l *ImxPwmLed) Cleanup() {
	l.globalLock.Lock()
	defer l.globalLock.Unlock()

	for i, device := range l.devices {
		if device != nil {
			l.setActive(i, false)
			unix.Close(device.fd)
			l.devices[i] = nil
		}
	}
	l.enabled = false
}

func (l *ImxPwmLed) PlayFade(ch int, idx int) error {
	if !l.enabled || l.devices[ch] == nil {
		return fmt.Errorf("LED system not enabled or invalid channel")
	}

	if idx < 0 || idx >= MaxFadeSize {
		return fmt.Errorf("invalid fade index: %d", idx)
	}

	device := l.devices[ch]
	device.lock.Lock()
	defer device.lock.Unlock()

	// First ensure the LED is active
	err := l.setActive(ch, true)
	if err != nil {
		log.Printf("Failed to activate LED %d: %v", ch, err)
		return err
	}

	// First open the fade
	log.Printf("Opening fade %d on LED %d (fd=%d)", idx, ch, device.fd)
	err = unix.IoctlSetInt(device.fd, pwmLedOpenFade, idx)
	if err != nil {
		log.Printf("Failed to open fade %d on LED %d: %v (errno=%d)", idx, ch, err, err.(*os.SyscallError).Err.(syscall.Errno))
		return fmt.Errorf("failed to open fade %d: %v", idx, err)
	}

	// Then play it
	log.Printf("Playing fade %d on LED %d (fd=%d)", idx, ch, device.fd)
	err = unix.IoctlSetInt(device.fd, pwmLedPlayFade, idx)
	if err != nil {
		log.Printf("Failed to play fade %d on LED %d: %v (errno=%d)", idx, ch, err, err.(*os.SyscallError).Err.(syscall.Errno))
		return fmt.Errorf("failed to play fade %d: %v", idx, err)
	}

	device.playingFade = idx
	log.Printf("Successfully started fade %d on LED %d", idx, ch)
	return nil
}
