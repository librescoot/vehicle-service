package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type fakePWMFileInfo struct {
	name string
	mode os.FileMode
}

func (f fakePWMFileInfo) Name() string       { return f.name }
func (f fakePWMFileInfo) Size() int64        { return 0 }
func (f fakePWMFileInfo) Mode() os.FileMode  { return f.mode }
func (f fakePWMFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakePWMFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakePWMFileInfo) Sys() any           { return nil }

type fakePWMEnvironment struct {
	modulePresent bool
	nodes         map[int]os.FileMode
	commands      []string
	commandErrors map[string]error
	sleeps        []time.Duration
	node0Stats    int
	node0ReadyAt  int
}

func healthyPWMEnvironment() *fakePWMEnvironment {
	nodes := make(map[int]os.FileMode, pwmLEDDeviceCount)
	for index := 0; index < pwmLEDDeviceCount; index++ {
		nodes[index] = os.ModeCharDevice
	}
	return &fakePWMEnvironment{
		modulePresent: true,
		nodes:         nodes,
		commandErrors: make(map[string]error),
	}
}

func (f *fakePWMEnvironment) ops() pwmDriverOps {
	const moduleDir = "/sys/module/imx_pwm_led"
	return pwmDriverOps{
		moduleDir: moduleDir,
		devicePath: func(index int) string {
			return fmt.Sprintf("/dev/pwm_led%d", index)
		},
		stat: func(path string) (os.FileInfo, error) {
			if path == moduleDir {
				if !f.modulePresent {
					return nil, os.ErrNotExist
				}
				return fakePWMFileInfo{name: "imx_pwm_led", mode: os.ModeDir}, nil
			}
			var index int
			if _, err := fmt.Sscanf(path, "/dev/pwm_led%d", &index); err != nil {
				return nil, os.ErrNotExist
			}
			if index == 0 && f.node0ReadyAt != 0 {
				f.node0Stats++
				if f.node0Stats < f.node0ReadyAt {
					return nil, os.ErrNotExist
				}
			}
			mode, ok := f.nodes[index]
			if !ok {
				return nil, os.ErrNotExist
			}
			return fakePWMFileInfo{name: filepath.Base(path), mode: mode}, nil
		},
		run: func(name string, args ...string) error {
			command := name
			if len(args) != 0 {
				command += " " + args[0]
			}
			f.commands = append(f.commands, command)
			return f.commandErrors[name]
		},
		sleep: func(duration time.Duration) {
			f.sleeps = append(f.sleeps, duration)
		},
	}
}

func TestPreparePWMDriverReusesVerifiedExistingDriver(t *testing.T) {
	env := healthyPWMEnvironment()
	reused, reason, removeErr, err := preparePWMDriver(env.ops())
	if err != nil || removeErr != nil || !reused || reason != "" {
		t.Fatalf("prepare = reused=%v reason=%q removeErr=%v err=%v", reused, reason, removeErr, err)
	}
	if len(env.commands) != 0 {
		t.Fatalf("verified driver triggered commands: %v", env.commands)
	}
}

func TestPreparePWMDriverFallsBackForIncompleteOrMismatchedState(t *testing.T) {
	tests := []struct {
		name       string
		breakState func(*fakePWMEnvironment)
	}{
		{"missing module", func(env *fakePWMEnvironment) { env.modulePresent = false }},
		{"missing node", func(env *fakePWMEnvironment) { delete(env.nodes, 7) }},
		{"non-character node", func(env *fakePWMEnvironment) { env.nodes[3] = 0 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := healthyPWMEnvironment()
			tc.breakState(env)
			reused, reason, _, err := preparePWMDriver(env.ops())
			if err != nil {
				t.Fatal(err)
			}
			if reused || reason == "" {
				t.Fatalf("prepare = reused=%v reason=%q", reused, reason)
			}
			want := []string{"rmmod imx_pwm_led", "modprobe imx_pwm_led"}
			if !reflect.DeepEqual(env.commands, want) {
				t.Fatalf("commands = %v, want %v", env.commands, want)
			}
		})
	}
}

func TestPreparePWMDriverRetainsReloadErrorBehavior(t *testing.T) {
	env := healthyPWMEnvironment()
	delete(env.nodes, 7)
	removeFailure := errors.New("module busy")
	env.commandErrors["rmmod"] = removeFailure

	reused, _, removeErr, err := preparePWMDriver(env.ops())
	if err != nil || reused || !errors.Is(removeErr, removeFailure) {
		t.Fatalf("nonfatal remove = reused=%v removeErr=%v err=%v", reused, removeErr, err)
	}

	env = healthyPWMEnvironment()
	delete(env.nodes, 7)
	loadFailure := errors.New("load failed")
	env.commandErrors["modprobe"] = loadFailure
	if _, _, _, err := preparePWMDriver(env.ops()); !errors.Is(err, loadFailure) {
		t.Fatalf("load error = %v, want %v", err, loadFailure)
	}
}

func TestPreparePWMDriverWaitsForPostReloadNode(t *testing.T) {
	env := healthyPWMEnvironment()
	env.node0ReadyAt = 4 // initial verification plus two failed post-load polls
	reused, _, _, err := preparePWMDriver(env.ops())
	if err != nil || reused {
		t.Fatalf("prepare = reused=%v err=%v", reused, err)
	}
	if len(env.sleeps) != 2 {
		t.Fatalf("sleeps = %v, want two waits", env.sleeps)
	}
	for _, duration := range env.sleeps {
		if duration != 100*time.Millisecond {
			t.Fatalf("sleep = %v, want 100ms", duration)
		}
	}
}
