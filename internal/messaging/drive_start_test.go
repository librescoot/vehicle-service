package messaging

import (
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	ipc "github.com/librescoot/redis-ipc"
	"vehicle-service/internal/logger"
)

func driveStartRedis(t *testing.T) *RedisClient {
	t.Helper()
	server, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server is required")
	}
	socket := filepath.Join(t.TempDir(), "redis.sock")
	cmd := exec.Command(server, "--port", "0", "--unixsocket", socket, "--unixsocketperm", "700", "--save", "", "--appendonly", "no")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test Redis did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	client, err := ipc.New(ipc.WithURL("unix://"+socket), ipc.WithCodec(ipc.StringCodec{}), ipc.WithMaxRetries(1))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &RedisClient{client: client, ctx: ctx, cancel: cancel,
		logger:     logger.NewLogger(log.New(io.Discard, "", 0), logger.LogLevelError),
		vehiclePub: client.NewHashPublisher("vehicle")}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestReadDriveStartInputs(t *testing.T) {
	r := driveStartRedis(t)
	for _, tc := range []struct {
		name, present, state, fault string
		slot                        int
		active, healthy             bool
	}{
		{"missing", "", "", "", 0, false, false},
		{"idle", "true", "idle", "0", 0, false, true},
		{"front active", "true", "active", "0", 0, true, true},
		{"rear active", "true", "active", "0", 1, true, true},
		{"absent active cache", "false", "active", "0", 0, false, true},
		{"unknown presence", "", "active", "0", 0, false, true},
		{"fault", "true", "active", "20", 0, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := r.client.Raw()
			if err := raw.Del(r.ctx, "battery:0", "battery:1", "engine-ecu").Err(); err != nil {
				t.Fatal(err)
			}
			key := "battery:0"
			if tc.slot == 1 {
				key = "battery:1"
			}
			if tc.present != "" {
				if err := raw.HSet(r.ctx, key, "present", tc.present, "state", tc.state).Err(); err != nil {
					t.Fatal(err)
				}
			}
			if tc.fault != "" {
				if err := raw.HSet(r.ctx, "engine-ecu", "fault:code", tc.fault).Err(); err != nil {
					t.Fatal(err)
				}
			}
			got, err := r.ReadDriveStartInputs(r.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got.ActiveBattery != tc.active || got.ECUHealthy != tc.healthy {
				t.Fatalf("inputs=%+v", got)
			}
		})
	}
	ctx, cancel := context.WithCancel(r.ctx)
	cancel()
	if got, err := r.ReadDriveStartInputs(ctx); err == nil || got.ActiveBattery || got.ECUHealthy {
		t.Fatalf("cancelled read = %+v, %v", got, err)
	}
	if err := r.SetDriveStartStatus("waiting-battery"); err != nil {
		t.Fatal(err)
	}
	if got := r.client.Raw().HGet(r.ctx, "vehicle", "drive-start-status").Val(); got != "waiting-battery" {
		t.Fatalf("status=%q", got)
	}
}

func TestDriveMotionSubscriptionInvalidatesReconnect(t *testing.T) {
	r := driveStartRedis(t)
	messages := make(chan string, 20)
	r.callbacks.DriveMotionCallback = func(payload string) { messages <- payload }
	r.wg.Add(1)
	go r.runDriveMotionSubscriber()
	await := func(want string) {
		t.Helper()
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		for {
			select {
			case got := <-messages:
				if got == want {
					return
				}
			case <-deadline.C:
				t.Fatalf("did not receive %q", want)
			}
		}
	}
	await("")
	if err := r.client.Raw().Publish(r.ctx, "engine-ecu:motion", "first").Err(); err != nil {
		t.Fatal(err)
	}
	await("first")
	if err := r.client.Raw().Do(r.ctx, "CLIENT", "KILL", "TYPE", "pubsub").Err(); err != nil {
		t.Fatal(err)
	}
	await("")
	deadline := time.Now().Add(3 * time.Second)
	for {
		count, err := r.client.Raw().PubSubNumSub(r.ctx, "engine-ecu:motion").Result()
		if err == nil && count["engine-ecu:motion"] == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("subscriber did not reconnect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := r.client.Raw().Publish(r.ctx, "engine-ecu:motion", "second").Err(); err != nil {
		t.Fatal(err)
	}
	await("second")
}
