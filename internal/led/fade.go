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
	PWMPeriod      = 12000 // Curve samples use this duty-cycle range.
	SampleRate     = 250   // Curves are sampled at 4 ms intervals.
	SampleDuration = time.Second / SampleRate
)

// Fade is an LED duty-cycle curve decoded from little-endian uint16 samples.
type Fade struct {
	Index        int
	Name         string
	Samples      []uint16 // Raw PWM values in [0, PWMPeriod].
	Duration     time.Duration
	ZeroPoints   []time.Duration // Times at which the output is off.
	FirstZero    time.Duration   // -1 when the curve never reaches zero.
	LastZero     time.Duration   // -1 when the curve never reaches zero.
	EndsAtZero   bool
	StartsAtZero bool
}

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

func (f *Fade) findZeroPoints() {
	if len(f.Samples) == 0 {
		return
	}

	f.ZeroPoints = []time.Duration{}
	f.FirstZero = -1
	f.LastZero = -1

	f.StartsAtZero = f.Samples[0] == 0

	f.EndsAtZero = f.Samples[len(f.Samples)-1] == 0

	for i, sample := range f.Samples {
		t := time.Duration(i) * SampleDuration

		if sample == 0 {
			f.ZeroPoints = append(f.ZeroPoints, t)
			if f.FirstZero < 0 {
				f.FirstZero = t
			}
			f.LastZero = t
		} else if i > 0 {
			prevSample := f.Samples[i-1]
			if prevSample > 0 && sample > 0 {
				continue
			}
			// This shouldn't happen with unsigned samples, but handle it anyway
		}
	}
}

// DutyAt returns the sampled duty normalized to [0, 1], clamped to the curve.
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

// NextZeroAfter returns -1 when no later off point exists.
func (f *Fade) NextZeroAfter(t time.Duration) time.Duration {
	for _, zp := range f.ZeroPoints {
		if zp >= t {
			return zp
		}
	}
	return -1
}

func extractFadeName(path string) string {
	base := filepath.Base(path)
	name := strings.TrimSuffix(base, ".bin")
	name = strings.TrimSuffix(name, filepath.Ext(name))

	parts := strings.SplitN(name, "_", 2)
	if len(parts) > 1 && strings.HasPrefix(parts[0], "fade") {
		return parts[1]
	}

	return name
}

func extractFadeIndex(path string) int {
	base := filepath.Base(path)
	name := strings.TrimSuffix(base, filepath.Ext(base))

	var index int
	if _, err := fmt.Sscanf(name, "fade%d", &index); err == nil {
		return index
	}
	if _, err := fmt.Sscanf(name, "fade%d_", &index); err == nil {
		return index
	}

	return -1
}
