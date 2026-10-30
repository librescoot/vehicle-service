package messaging

import (
	"context"
	"io"
	"log"
	"net"
	"os/exec"
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
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cmd := exec.Command(server, "--bind", "127.0.0.1", "--port", port, "--dir", dir, "--save", "", "--appendonly", "no")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test Redis did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	client, err := ipc.New(ipc.WithURL(address), ipc.WithCodec(ipc.StringCodec{}), ipc.WithMaxRetries(1))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &RedisClient{client: client, ctx: ctx, cancel: cancel,
		logger:     logger.NewLogger(log.New(io.Discard, "", 0), logger.LogLevelError),
		vehiclePub: client.NewHashPublisher("vehicle")}
	t.Cleanup(func() { _ = r.Close() })
	config, err := client.Raw().ConfigGet(ctx, "dir").Result()
	if err != nil || config["dir"] != dir || client.Raw().Options().Addr != address {
		t.Fatalf("test Redis isolation failed: address=%s dir=%v err=%v", client.Raw().Options().Addr, config, err)
	}
	return r
}

func TestReadDriveStartInputs(t *testing.T) {
	r := driveStartRedis(t)
	for _, tc := range []struct {
		name, present, state string
		slot                 int
		active               bool
	}{
		{"missing", "", "", 0, false},
		{"idle", "true", "idle", 0, false},
		{"asleep", "true", "asleep", 0, false},
		{"front active", "true", "active", 0, true},
		{"rear active", "true", "active", 1, true},
		{"absent active cache", "false", "active", 0, false},
		{"unknown presence", "", "active", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := r.client.Raw()
			if err := raw.Del(r.ctx, "battery:0", "battery:1").Err(); err != nil {
				t.Fatal(err)
			}
			key := "battery:0"
			if tc.slot == 1 {
				key = "battery:1"
			}
			if tc.state != "" {
				if err := raw.HSet(r.ctx, key, "state", tc.state).Err(); err != nil {
					t.Fatal(err)
				}
			}
			if tc.present != "" {
				if err := raw.HSet(r.ctx, key, "present", tc.present).Err(); err != nil {
					t.Fatal(err)
				}
			}
			got, err := r.ReadDriveStartInputs(r.ctx)
			if err != nil || got.ActiveBattery != tc.active {
				t.Fatalf("inputs=%+v, err=%v", got, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(r.ctx)
	cancel()
	if got, err := r.ReadDriveStartInputs(ctx); err == nil || got.ActiveBattery {
		t.Fatalf("cancelled read = %+v, %v", got, err)
	}
	if err := r.SetDriveStartStatus("waiting-battery"); err != nil {
		t.Fatal(err)
	}
	if got := r.client.Raw().HGet(r.ctx, "vehicle", "drive-start-status").Val(); got != "waiting-battery" {
		t.Fatalf("status=%q", got)
	}
}

func TestReadDriveStartInputsIgnoresECU(t *testing.T) {
	r := driveStartRedis(t)
	raw := r.client.Raw()
	if err := raw.HSet(r.ctx, "battery:0", "present", "true", "state", "active").Err(); err != nil {
		t.Fatal(err)
	}
	if err := raw.HSet(r.ctx, "engine-ecu", "rpm", "250", "speed:raw", "20", "throttle", "on", "fault:code", "20").Err(); err != nil {
		t.Fatal(err)
	}
	for _, malformed := range []bool{false, true} {
		if malformed {
			if err := raw.Set(r.ctx, "engine-ecu", "wrong type", 0).Err(); err != nil {
				t.Fatal(err)
			}
		}
		got, err := r.ReadDriveStartInputs(r.ctx)
		if err != nil || !got.ActiveBattery {
			t.Fatalf("ECU state blocked active battery: %+v, %v", got, err)
		}
	}
}

func TestReadDriveStartInputsFailsClosedOnBatteryReadError(t *testing.T) {
	r := driveStartRedis(t)
	if err := r.client.Raw().Set(r.ctx, "battery:0", "wrong type", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if got, err := r.ReadDriveStartInputs(r.ctx); err == nil || got.ActiveBattery {
		t.Fatalf("invalid battery read = %+v, %v", got, err)
	}
}
