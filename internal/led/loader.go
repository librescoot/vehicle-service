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
	DefaultFadesDir = "/usr/share/led-curves/fades"
	DefaultCuesDir  = "/usr/share/led-curves/cues"
)

type CurveLibrary struct {
	Fades map[int]*Fade
	Cues  map[int]*Cue

	mu     sync.RWMutex
	logger *logger.Logger
}

func NewCurveLibrary(log *logger.Logger) *CurveLibrary {
	return &CurveLibrary{
		Fades:  make(map[int]*Fade),
		Cues:   make(map[int]*Cue),
		logger: log,
	}
}

func (lib *CurveLibrary) Load() error {
	return lib.LoadFromDirs(DefaultFadesDir, DefaultCuesDir)
}

func (lib *CurveLibrary) LoadFromDirs(fadesDir, cuesDir string) error {
	lib.mu.Lock()
	defer lib.mu.Unlock()

	if err := lib.loadFades(fadesDir); err != nil {
		return fmt.Errorf("failed to load fades: %w", err)
	}

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
			lib.logger.Debugf("Loaded fade %d (%s): duration=%v, zeroPoints=%d, endsAtZero=%v",
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
			cue.CalculateDuration(lib.Fades)
			lib.Cues[cue.Index] = cue
			lib.logger.Debugf("Loaded cue %d (%s): duration=%v, actions=%d",
				cue.Index, cue.Name, cue.Duration, len(cue.Actions))
		}
	}

	lib.logger.Infof("Loaded %d cues from %s", len(lib.Cues), dir)
	return nil
}

func (lib *CurveLibrary) GetFade(index int) *Fade {
	lib.mu.RLock()
	defer lib.mu.RUnlock()
	return lib.Fades[index]
}

func (lib *CurveLibrary) GetCue(index int) *Cue {
	lib.mu.RLock()
	defer lib.mu.RUnlock()
	return lib.Cues[index]
}

func (lib *CurveLibrary) GetCueDuration(cueIndex int) time.Duration {
	lib.mu.RLock()
	defer lib.mu.RUnlock()

	if cue, ok := lib.Cues[cueIndex]; ok {
		return cue.Duration
	}
	return 0
}

// GetCueNextZero returns the earliest next off point among a cue's fade actions.
func (lib *CurveLibrary) GetCueNextZero(cueIndex int, elapsed time.Duration) time.Duration {
	lib.mu.RLock()
	defer lib.mu.RUnlock()

	cue, ok := lib.Cues[cueIndex]
	if !ok {
		return -1
	}

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

// IsCueAtZero reports whether every fade is off; an unknown cue is safe to stop.
func (lib *CurveLibrary) IsCueAtZero(cueIndex int, elapsed time.Duration) bool {
	lib.mu.RLock()
	defer lib.mu.RUnlock()

	cue, ok := lib.Cues[cueIndex]
	if !ok {
		return true
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

// WaitForCueZeroOrEnd waits for an off point or completion; unknown cues do not delay shutdown.
func (lib *CurveLibrary) WaitForCueZeroOrEnd(cueIndex int, elapsed time.Duration) time.Duration {
	lib.mu.RLock()
	defer lib.mu.RUnlock()

	cue, ok := lib.Cues[cueIndex]
	if !ok {
		return 0
	}

	if elapsed >= cue.Duration {
		return 0
	}

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

	nextZero := lib.GetCueNextZero(cueIndex, elapsed)
	if nextZero >= 0 && nextZero <= cue.Duration {
		return nextZero - elapsed
	}

	return cue.Duration - elapsed
}
