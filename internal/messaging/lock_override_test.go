package messaging

import (
	"errors"
	"fmt"
	ipc "github.com/librescoot/redis-ipc"
	"net"
	"os/exec"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestLockCallProtocol(t *testing.T) {
	future := time.Now().Add(time.Minute).UnixMilli()
	request := func(version int, command string, deadline int64) string {
		return fmt.Sprintf(`{"version":%d,"command":%q,"deadline":%d}`, version, command, deadline)
	}
	for _, tc := range []struct {
		payload, want, err string
		calls              int
	}{
		{request(1, "capabilities", future), `"lock:v1:ignore-seatbox"`, "", 0},
		{request(1, "ignore-seatbox", future), `"lock:v1:accepted"`, "", 1},
		{request(2, "ignore-seatbox", future), "", "unsupported", 0},
		{request(1, "ignore-seatbox", 1), "", "expired", 0},
		{request(1, "ignore-seatbox:force", future), "", "invalid", 0},
		{request(1, "force-lock", future), "", "invalid", 0},
		{`{}`, "", "unsupported", 0},
		{`null`, "", "unsupported", 0},
		{`"ignore-seatbox"`, "", "invalid", 0},
		{`{"version":1,"command":"ignore-seatbox","deadline":123,"extra":true}`, "", "invalid", 0},
		{request(1, "ignore-seatbox", future) + `{}`, "", "invalid", 0},
		{`{`, "", "invalid", 0},
	} {
		t.Run(tc.payload, func(t *testing.T) {
			calls := 0
			r := &RedisClient{callbacks: Callbacks{LockIgnoreSeatboxCallback: func(deadline time.Time) error {
				calls++
				if deadline.UnixMilli() != future {
					t.Fatal(deadline)
				}
				return nil
			}}}
			got, err := r.handleLockCall(tc.payload)
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			if got != tc.want || msg != tc.err || calls != tc.calls {
				t.Fatalf("got %q %q calls %d", got, msg, calls)
			}
		})
	}
	for _, message := range []string{"unsafe-state", "expired", "processing"} {
		r := &RedisClient{callbacks: Callbacks{LockIgnoreSeatboxCallback: func(time.Time) error { return fmt.Errorf("%s", message) }}}
		got, err := r.handleLockCall(request(1, "ignore-seatbox", future))
		if got != "" || err == nil || err.Error() != message {
			t.Fatalf("%q %v", got, err)
		}
	}
	r := &RedisClient{}
	if _, err := r.handleLockCall(request(1, "capabilities", future)); err == nil || err.Error() != "unsupported" {
		t.Fatal(err)
	}
}

// Uses an isolated, non-persistent loopback Redis, never a scooter/service.
// This exercises the actual StringCodec + Call envelope/reply boundary.
func TestLockCallRedisRoundTrip(t *testing.T) {
	binary, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server unavailable")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	process := exec.Command(binary, "--bind", "127.0.0.1", "--port", strconv.Itoa(port), "--save", "", "--appendonly", "no", "--dir", t.TempDir())
	if err = process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Process.Kill(); _ = process.Wait() })
	address := fmt.Sprintf("127.0.0.1:%d", port)
	for end := time.Now().Add(time.Second); ; {
		conn, e := net.DialTimeout("tcp", address, 20*time.Millisecond)
		if e == nil {
			conn.Close()
			break
		}
		if time.Now().After(end) {
			t.Fatal("test Redis did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	client, err := ipc.New(ipc.WithAddress("127.0.0.1"), ipc.WithPort(port), ipc.WithCodec(ipc.StringCodec{}))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var calls atomic.Int32
	r := &RedisClient{callbacks: Callbacks{LockIgnoreSeatboxCallback: func(time.Time) error { calls.Add(1); return nil }}}
	handler := ipc.HandleCalls(client, "scooter:lock", r.handleLockCall, ipc.WithCallConcurrency(1))
	defer handler.Stop()
	for _, command := range []string{"capabilities", "ignore-seatbox"} {
		request := fmt.Sprintf(`{"version":1,"command":%q,"deadline":%d}`, command, time.Now().Add(time.Second).UnixMilli())
		reply, err := ipc.Call[string, string](client, "scooter:lock", request, time.Second)
		want := `"lock:v1:ignore-seatbox"`
		if command == "ignore-seatbox" {
			want = `"lock:v1:accepted"`
		}
		if err != nil || reply != want {
			t.Fatalf("round trip: %q %v", reply, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("probe actuated: %d", calls.Load())
	}
	_, err = ipc.Call[string, string](client, "scooter:lock", `{"version":2}`, time.Second)
	if !ipc.IsCallError(err) || err.Error() != "unsupported" {
		t.Fatalf("version error: %v", err)
	}
	// An old vehicle has no listener on the new endpoint: no success, and the
	// expired message must not actuate if support starts after the timeout.
	handler.Stop()
	payload := fmt.Sprintf(`{"version":1,"command":"ignore-seatbox","deadline":%d}`, time.Now().Add(30*time.Millisecond).UnixMilli())
	_, err = ipc.Call[string, string](client, "scooter:lock", payload, 30*time.Millisecond)
	if !errors.Is(err, ipc.ErrCallTimeout) {
		t.Fatalf("old service: %v", err)
	}
	restarted := ipc.HandleCalls(client, "scooter:lock", r.handleLockCall)
	defer restarted.Stop()
	probe := fmt.Sprintf(`{"version":1,"command":"capabilities","deadline":%d}`, time.Now().Add(time.Second).UnixMilli())
	if _, err = ipc.Call[string, string](client, "scooter:lock", probe, time.Second); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("expired queued override actuated")
	}
	restarted.Stop()

	t.Run("caller-times-out-after-acceptance", func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		var completed atomic.Int32
		delayed := &RedisClient{callbacks: Callbacks{LockIgnoreSeatboxCallback: func(time.Time) error {
			// Model a callback already past the FSM's pre-exit acceptance
			// boundary. Its normal shutdown is delayed, not retried or vetoed.
			close(started)
			<-release
			completed.Add(1)
			return nil
		}}}
		h := ipc.HandleCalls(client, "scooter:lock", delayed.handleLockCall)
		defer h.Stop()
		result := make(chan error, 1)
		payload := fmt.Sprintf(`{"version":1,"command":"ignore-seatbox","deadline":%d}`, time.Now().Add(100*time.Millisecond).UnixMilli())
		go func() {
			_, err := ipc.Call[string, string](client, "scooter:lock", payload, 100*time.Millisecond)
			result <- err
		}()
		select {
		case <-started:
		case err := <-result:
			close(release)
			t.Fatalf("callback did not reach acceptance: %v", err)
		}
		err := <-result
		close(release)
		h.Stop() // Drain completion even though the caller already unsubscribed.
		if !errors.Is(err, ipc.ErrCallTimeout) || completed.Load() != 1 {
			t.Fatalf("caller outcome=%v completed shutdowns=%d", err, completed.Load())
		}
	})
}
