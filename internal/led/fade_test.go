package led

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadFade(t *testing.T) {
	// Create a temporary fade file with known content
	tmpDir := t.TempDir()
	fadePath := filepath.Join(tmpDir, "fade0_test")

	// Create a fade with 5 samples (20ms total at 250Hz):
	// 0, 6000, 12000, 6000, 0
	samples := []byte{
		0x00, 0x00, // 0
		0x70, 0x17, // 6000
		0xE0, 0x2E, // 12000
		0x70, 0x17, // 6000
		0x00, 0x00, // 0
	}
	if err := os.WriteFile(fadePath, samples, 0644); err != nil {
		t.Fatalf("Failed to write test fade: %v", err)
	}

	fade, err := LoadFade(fadePath)
	if err != nil {
		t.Fatalf("LoadFade failed: %v", err)
	}

	// Check basic properties
	if fade.Index != 0 {
		t.Errorf("Expected index 0, got %d", fade.Index)
	}

	if len(fade.Samples) != 5 {
		t.Errorf("Expected 5 samples, got %d", len(fade.Samples))
	}

	// Duration should be 5 samples * 4ms = 20ms
	expectedDuration := 20 * time.Millisecond
	if fade.Duration != expectedDuration {
		t.Errorf("Expected duration %v, got %v", expectedDuration, fade.Duration)
	}

	// Should start at zero
	if !fade.StartsAtZero {
		t.Error("Expected StartsAtZero to be true")
	}

	// Should end at zero
	if !fade.EndsAtZero {
		t.Error("Expected EndsAtZero to be true")
	}

	// Should have 2 zero points (start and end)
	if len(fade.ZeroPoints) != 2 {
		t.Errorf("Expected 2 zero points, got %d", len(fade.ZeroPoints))
	}

	// First zero at 0ms
	if fade.FirstZero != 0 {
		t.Errorf("Expected FirstZero at 0, got %v", fade.FirstZero)
	}

	// Last zero at 16ms (sample index 4)
	expectedLastZero := 16 * time.Millisecond
	if fade.LastZero != expectedLastZero {
		t.Errorf("Expected LastZero at %v, got %v", expectedLastZero, fade.LastZero)
	}
}

func TestFadeNextZeroAfter(t *testing.T) {
	fade := &Fade{
		ZeroPoints: []time.Duration{
			0,
			400 * time.Millisecond,
			800 * time.Millisecond,
		},
	}

	// Before first zero
	if zp := fade.NextZeroAfter(-1 * time.Millisecond); zp != 0 {
		t.Errorf("Expected 0, got %v", zp)
	}

	// At first zero
	if zp := fade.NextZeroAfter(0); zp != 0 {
		t.Errorf("Expected 0, got %v", zp)
	}

	// Between first and second
	if zp := fade.NextZeroAfter(200 * time.Millisecond); zp != 400*time.Millisecond {
		t.Errorf("Expected 400ms, got %v", zp)
	}

	// After last zero
	if zp := fade.NextZeroAfter(900 * time.Millisecond); zp != -1 {
		t.Errorf("Expected -1, got %v", zp)
	}
}

func TestExtractFadeIndex(t *testing.T) {
	tests := []struct {
		filename string
		expected int
	}{
		{"fade0", 0},
		{"fade10", 10},
		{"fade0_smooth-on", 0},
		{"fade12_blink", 12},
		{"notafade", -1},
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			if got := extractFadeIndex(tt.filename); got != tt.expected {
				t.Errorf("extractFadeIndex(%q) = %d, want %d", tt.filename, got, tt.expected)
			}
		})
	}
}
