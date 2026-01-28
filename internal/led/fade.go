package led

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// PWMPeriod is the PWM duty cycle range (0-12000)
	PWMPeriod = 12000
	// SampleRate is the fade sample rate in Hz (250Hz = 4ms per sample)
	SampleRate = 250
	// SampleDuration is the duration of each sample
	SampleDuration = time.Second / SampleRate // 4ms
)

// Fade represents a parsed LED fade curve
type Fade struct {
	Index       int             // Fade index (parsed from filename)
	Name        string          // Human-readable name
	Samples     []uint16        // Raw duty cycle samples (0-12000)
	Duration    time.Duration   // Total duration of the fade
	ZeroPoints  []time.Duration // Times where the fade crosses or reaches zero
	FirstZero   time.Duration   // Time of first zero point (-1 if none)
	LastZero    time.Duration   // Time of last zero point (-1 if none)
	EndsAtZero  bool            // Whether the fade ends at zero duty
	StartsAtZero bool           // Whether the fade starts at zero duty
}

// LoadFade loads a fade from a binary file
func LoadFade(filename string) (*Fade, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to open fade file: %w", err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat fade file: %w", err)
	}

	if stat.Size()%2 != 0 {
		return nil, fmt.Errorf("invalid fade file: odd number of bytes")
	}

	sampleCount := stat.Size() / 2
	samples := make([]uint16, sampleCount)
	err = binary.Read(file, binary.LittleEndian, &samples)
	if err != nil {
		return nil, fmt.Errorf("failed to read fade samples: %w", err)
	}

	fade := &Fade{
		Name:     extractFadeName(filename),
		Index:    extractFadeIndex(filename),
		Samples:  samples,
		Duration: time.Duration(sampleCount) * SampleDuration,
	}

	fade.findZeroPoints()

	return fade, nil
}

// findZeroPoints analyzes the samples to find all zero crossings and zero points
func (f *Fade) findZeroPoints() {
	if len(f.Samples) == 0 {
		return
	}

	f.ZeroPoints = []time.Duration{}
	f.FirstZero = -1
	f.LastZero = -1

	// Check if starts at zero
	f.StartsAtZero = f.Samples[0] == 0

	// Check if ends at zero
	f.EndsAtZero = f.Samples[len(f.Samples)-1] == 0

	// Find all zero points and zero crossings
	for i, sample := range f.Samples {
		t := time.Duration(i) * SampleDuration

		if sample == 0 {
			// Exact zero point
			f.ZeroPoints = append(f.ZeroPoints, t)
			if f.FirstZero < 0 {
				f.FirstZero = t
			}
			f.LastZero = t
		} else if i > 0 {
			// Check for zero crossing (sign change through zero)
			prevSample := f.Samples[i-1]
			if prevSample > 0 && sample > 0 {
				// No crossing, both positive
				continue
			}
			// This shouldn't happen with unsigned samples, but handle it anyway
		}
	}
}

// DutyAt returns the duty cycle (0.0-1.0) at a given time
func (f *Fade) DutyAt(t time.Duration) float64 {
	if len(f.Samples) == 0 {
		return 0.0
	}

	sampleIndex := int(t / SampleDuration)
	if sampleIndex < 0 {
		sampleIndex = 0
	}
	if sampleIndex >= len(f.Samples) {
		sampleIndex = len(f.Samples) - 1
	}

	return float64(f.Samples[sampleIndex]) / float64(PWMPeriod)
}

// IsZeroAt returns true if the fade is at zero duty at the given time
func (f *Fade) IsZeroAt(t time.Duration) bool {
	if len(f.Samples) == 0 {
		return true
	}

	sampleIndex := int(t / SampleDuration)
	if sampleIndex < 0 || sampleIndex >= len(f.Samples) {
		return f.EndsAtZero
	}

	return f.Samples[sampleIndex] == 0
}

// NextZeroAfter returns the next zero point at or after the given time
// Returns -1 if no zero point exists after the given time
func (f *Fade) NextZeroAfter(t time.Duration) time.Duration {
	for _, zp := range f.ZeroPoints {
		if zp >= t {
			return zp
		}
	}
	return -1
}

// extractFadeName extracts a human-readable name from the filename
func extractFadeName(path string) string {
	base := filepath.Base(path)
	name := strings.TrimSuffix(base, ".bin")
	name = strings.TrimSuffix(name, filepath.Ext(name))

	// Handle names like "fade0_smooth-on" -> "smooth-on"
	parts := strings.SplitN(name, "_", 2)
	if len(parts) > 1 && strings.HasPrefix(parts[0], "fade") {
		return parts[1]
	}

	return name
}

// extractFadeIndex extracts the fade index from the filename
func extractFadeIndex(path string) int {
	base := filepath.Base(path)
	name := strings.TrimSuffix(base, filepath.Ext(base))

	var index int
	// Try "fade0" format
	if _, err := fmt.Sscanf(name, "fade%d", &index); err == nil {
		return index
	}
	// Try "fade0_name" format
	if _, err := fmt.Sscanf(name, "fade%d_", &index); err == nil {
		return index
	}

	return -1
}
