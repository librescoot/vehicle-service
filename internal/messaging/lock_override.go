package messaging

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// scooter:lock is a bounded redis-ipc Call endpoint, not a fire-and-forget
// scooter:state command. StringCodec requires explicitly encoded JSON payloads.
func (r *RedisClient) handleLockCall(payload string) (string, error) {
	var req struct {
		Version  int    `json:"version"`
		Command  string `json:"command"`
		Deadline int64  `json:"deadline"`
	}
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return "", fmt.Errorf("invalid")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return "", fmt.Errorf("invalid")
	}
	if req.Version != 1 {
		return "", fmt.Errorf("unsupported")
	}
	deadline := time.UnixMilli(req.Deadline)
	if !time.Now().Before(deadline) {
		return "", fmt.Errorf("expired")
	}
	if r.callbacks.LockIgnoreSeatboxCallback == nil {
		return "", fmt.Errorf("unsupported")
	}
	switch req.Command {
	case "capabilities":
		return `"lock:v1:ignore-seatbox"`, nil
	case "ignore-seatbox":
		if err := r.callbacks.LockIgnoreSeatboxCallback(deadline); err != nil {
			return "", err
		}
		return `"lock:v1:accepted"`, nil
	default:
		return "", fmt.Errorf("invalid")
	}
}
