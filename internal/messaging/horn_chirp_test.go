package messaging

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestHornChirpValidation(t *testing.T) {
	now := time.Unix(100, 0)
	for _, test := range []struct {
		command string
		valid   bool
	}{
		{"chirp:boot:105", true},
		{"chirp:other:105", false},
		{"chirp:boot:100", false},
		{"chirp:boot:106", false},
		{"chirp:boot:bad", false},
		{"chirp:boot", false},
	} {
		t.Run(test.command, func(t *testing.T) {
			err := validateHornChirp(test.command, "boot", now)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v, error=%v", test.valid, err)
			}
		})
	}
}

func TestHornChirpDispatch(t *testing.T) {
	calls := 0
	r := &RedisClient{
		ctx:         context.Background(),
		chirpBootID: "boot",
		callbacks: Callbacks{HornChirpCallback: func(context.Context) error {
			calls++
			return nil
		}},
	}
	if err := r.handleHornCommand(fmt.Sprintf("chirp:boot:%d", time.Now().Unix()+5)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("chirp calls=%d", calls)
	}
	if err := r.handleHornCommand("chirp:boot:1"); err == nil || calls != 1 {
		t.Fatalf("expired chirp dispatched: calls=%d err=%v", calls, err)
	}
}
