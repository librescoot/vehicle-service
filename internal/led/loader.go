package led

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"vehicle-service/internal/logger"
)

const (
	// Default paths for LED curve files
	DefaultFadesDir = "/usr/share/led-curves/fades"
	DefaultCuesDir  = "/usr/share/led-curves/cues"
)

// CurveLibrary holds all loaded fades and cues with their metadata
type CurveLibrary struct {
	Fades map[int]*Fade
	Cues  map[int]*Cue

	mu     sync.RWMutex
	logger *logger.Logger
}

// NewCurveLibrary creates a new empty curve library
func NewCurveLibrary(log *logger.Logger) *CurveLibrary {
	return &CurveLibrary{
		Fades:  make(map[int]*Fade),
		Cues:   make(map[int]*Cue),
		logger: log,
	}
}

// Load loads all fades and cues from the default directories
func (lib *CurveLibrary) Load() error {
	return lib.LoadFromDirs(DefaultFadesDir, DefaultCuesDir)
}

// LoadFromDirs loads all fades and cues from the specified directories
func (lib *CurveLibrary) LoadFromDirs(fadesDir, cuesDir string) error {
	lib.mu.Lock()
	defer lib.mu.Unlock()

	// Load fades first
	if err := lib.loadFades(fadesDir); err != nil {
		return fmt.Errorf("failed to load fades: %w", err)
	}

	// Load cues and calculate their durations
	if err := lib.loadCues(cuesDir); err != nil {
		return fmt.Errorf("failed to load cues: %w", err)
	}

	return nil
}

func (lib *CurveLibrary) loadFades(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			lib.logger.Warnf("Fades directory does not exist: %s", dir)
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		fade, err := LoadFade(path)
		if err != nil {
			lib.logger.Warnf("Failed to load fade %s: %v", entry.Name(), err)
			continue
		}

		if fade.Index >= 0 {
			lib.Fades[fade.Index] = fade
			lib.logger.Infof("Loaded fade %d (%s): duration=%v, zeroPoints=%d, endsAtZero=%v",
				fade.Index, fade.Name, fade.Duration, len(fade.ZeroPoints), fade.EndsAtZero)
		}
	}

	lib.logger.Infof("Loaded %d fades from %s", len(lib.Fades), dir)
	return nil
}

func (lib *CurveLibrary) loadCues(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			lib.logger.Warnf("Cues directory does not exist: %s", dir)
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		cue, err := LoadCue(path)
		if err != nil {
			lib.logger.Warnf("Failed to load cue %s: %v", entry.Name(), err)
			continue
		}

		if cue.Index >= 0 {
			// Calculate duration based on referenced fades
			cue.CalculateDuration(lib.Fades)
			lib.Cues[cue.Index] = cue
			lib.logger.Debugf("Loaded cue %d (%s): duration=%v, actions=%d",
				cue.Index, cue.Name, cue.Duration, len(cue.Actions))
		}
	}

	lib.logger.Infof("Loaded %d cues from %s", len(lib.Cues), dir)
	return nil
}

// GetFade returns a fade by index
func (lib *CurveLibrary) GetFade(index int) *Fade {
	lib.mu.RLock()
	defer lib.mu.RUnlock()
	return lib.Fades[index]
}

// GetCue returns a cue by index
func (lib *CurveLibrary) GetCue(index int) *Cue {
	lib.mu.RLock()
	defer lib.mu.RUnlock()
	return lib.Cues[index]
}

// GetCueDuration returns the duration of a cue, or 0 if not found
func (lib *CurveLibrary) GetCueDuration(cueIndex int) time.Duration {
	lib.mu.RLock()
	defer lib.mu.RUnlock()

	if cue, ok := lib.Cues[cueIndex]; ok {
		return cue.Duration
	}
	return 0
}

// GetCueNextZero returns the next time a cue's fades will be at zero
// after the given elapsed time since the cue started.
// Returns -1 if no zero point is found.
func (lib *CurveLibrary) GetCueNextZero(cueIndex int, elapsed time.Duration) time.Duration {
	lib.mu.RLock()
	defer lib.mu.RUnlock()

	cue, ok := lib.Cues[cueIndex]
	if !ok {
		return -1
	}

	// Find the earliest next zero point across all fades in the cue
	nextZero := time.Duration(-1)

	for _, action := range cue.Actions {
		if action.ActionType == ActionTypeFade {
			fade, ok := lib.Fades[action.FadeIndex]
			if !ok {
				continue
			}

			zp := fade.NextZeroAfter(elapsed)
			if zp >= 0 {
				if nextZero < 0 || zp < nextZero {
					nextZero = zp
				}
			}
		}
	}

	return nextZero
}

// IsCueAtZero returns true if all fades in a cue are at zero at the given elapsed time
func (lib *CurveLibrary) IsCueAtZero(cueIndex int, elapsed time.Duration) bool {
	lib.mu.RLock()
	defer lib.mu.RUnlock()

	cue, ok := lib.Cues[cueIndex]
	if !ok {
		return true // Unknown cue, assume safe to stop
	}

	for _, action := range cue.Actions {
		if action.ActionType == ActionTypeFade {
			fade, ok := lib.Fades[action.FadeIndex]
			if !ok {
				continue
			}

			if !fade.IsZeroAt(elapsed) {
				return false
			}
		}
	}

	return true
}

// WaitForCueZeroOrEnd calculates how long to wait for the cue to reach
// a zero point or end, whichever comes first.
// Returns 0 if already at zero or past duration.
func (lib *CurveLibrary) WaitForCueZeroOrEnd(cueIndex int, elapsed time.Duration) time.Duration {
	lib.mu.RLock()
	defer lib.mu.RUnlock()

	cue, ok := lib.Cues[cueIndex]
	if !ok {
		return 0 // Unknown cue, don't wait
	}

	// If already past duration, no wait needed
	if elapsed >= cue.Duration {
		return 0
	}

	// Check if already at zero
	atZero := true
	for _, action := range cue.Actions {
		if action.ActionType == ActionTypeFade {
			fade, ok := lib.Fades[action.FadeIndex]
			if ok && !fade.IsZeroAt(elapsed) {
				atZero = false
				break
			}
		}
	}
	if atZero {
		return 0
	}

	// Find the next zero point
	nextZero := lib.GetCueNextZero(cueIndex, elapsed)
	if nextZero >= 0 && nextZero <= cue.Duration {
		return nextZero - elapsed
	}

	// No zero point found, wait until cue ends
	return cue.Duration - elapsed
}
