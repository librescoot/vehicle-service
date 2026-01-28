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
	// ActionTypeFade triggers a fade on a channel
	ActionTypeFade = 0
	// ActionTypeDuty sets a duty cycle directly
	ActionTypeDuty = 1

	// MaxLEDs is the maximum number of LED channels
	MaxLEDs = 8
)

// CueAction represents a single action within a cue
type CueAction struct {
	LEDIndex   int     // LED channel (0-7)
	ActionType int     // 0=fade, 1=duty
	FadeIndex  int     // If ActionType==fade, which fade to play
	DutyCycle  float64 // If ActionType==duty, the duty cycle (0.0-1.0)
}

// Cue represents a parsed LED cue sequence
type Cue struct {
	Index    int          // Cue index (parsed from filename)
	Name     string       // Human-readable name
	Actions  []CueAction  // List of actions in the cue
	Duration time.Duration // Calculated duration (max of referenced fades)
}

// LoadCue loads a cue from a binary file
func LoadCue(filename string) (*Cue, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to open cue file: %w", err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat cue file: %w", err)
	}

	if stat.Size()%4 != 0 {
		return nil, fmt.Errorf("invalid cue file: size not multiple of 4 bytes")
	}

	actionCount := stat.Size() / 4
	actions := make([]CueAction, 0, actionCount)

	for i := int64(0); i < actionCount; i++ {
		var buf [4]byte
		_, err := file.Read(buf[:])
		if err != nil {
			return nil, fmt.Errorf("failed to read action %d: %w", i, err)
		}

		ledIndex := int(buf[0] & 0x1F)
		actionType := int(buf[1] & 0x0F)
		value := binary.LittleEndian.Uint16(buf[2:4])

		if ledIndex >= MaxLEDs {
			return nil, fmt.Errorf("invalid LED index %d in action %d", ledIndex, i)
		}

		action := CueAction{
			LEDIndex:   ledIndex,
			ActionType: actionType,
		}

		switch actionType {
		case ActionTypeFade:
			action.FadeIndex = int(value)
		case ActionTypeDuty:
			action.DutyCycle = float64(value) / float64(PWMPeriod)
		default:
			return nil, fmt.Errorf("invalid action type %d in action %d", actionType, i)
		}

		actions = append(actions, action)
	}

	cue := &Cue{
		Name:    extractCueName(filename),
		Index:   extractCueIndex(filename),
		Actions: actions,
	}

	return cue, nil
}

// CalculateDuration calculates the cue's duration based on referenced fades
func (c *Cue) CalculateDuration(fades map[int]*Fade) {
	c.Duration = 0

	for _, action := range c.Actions {
		if action.ActionType == ActionTypeFade {
			if fade, ok := fades[action.FadeIndex]; ok {
				if fade.Duration > c.Duration {
					c.Duration = fade.Duration
				}
			}
		}
	}
}

// GetFadeIndices returns all fade indices referenced by this cue
func (c *Cue) GetFadeIndices() []int {
	indices := []int{}
	seen := make(map[int]bool)

	for _, action := range c.Actions {
		if action.ActionType == ActionTypeFade {
			if !seen[action.FadeIndex] {
				indices = append(indices, action.FadeIndex)
				seen[action.FadeIndex] = true
			}
		}
	}

	return indices
}

// GetChannelFade returns the fade index for a specific LED channel, or -1 if not found
func (c *Cue) GetChannelFade(ledIndex int) int {
	for _, action := range c.Actions {
		if action.LEDIndex == ledIndex && action.ActionType == ActionTypeFade {
			return action.FadeIndex
		}
	}
	return -1
}

// extractCueName extracts a human-readable name from the filename
func extractCueName(path string) string {
	base := filepath.Base(path)
	name := strings.TrimSuffix(base, ".bin")
	name = strings.TrimSuffix(name, filepath.Ext(name))

	// Handle names like "cue10_blink_left" -> "blink_left"
	parts := strings.SplitN(name, "_", 2)
	if len(parts) > 1 && strings.HasPrefix(parts[0], "cue") {
		return parts[1]
	}

	return name
}

// extractCueIndex extracts the cue index from the filename
func extractCueIndex(path string) int {
	base := filepath.Base(path)
	name := strings.TrimSuffix(base, filepath.Ext(base))

	var index int
	// Try "cue0" format
	if _, err := fmt.Sscanf(name, "cue%d", &index); err == nil {
		return index
	}
	// Try "cue0_name" format
	if _, err := fmt.Sscanf(name, "cue%d_", &index); err == nil {
		return index
	}

	return -1
}
