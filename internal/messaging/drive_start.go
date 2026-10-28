package messaging

import (
	"context"
	"time"

	ipc "github.com/librescoot/redis-ipc"
	"github.com/redis/go-redis/v9"
	"vehicle-service/internal/types"
)

func (r *RedisClient) ReadDriveStartInputs(ctx context.Context) (types.DriveStartInputs, error) {
	ctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	pipe := r.client.Raw().TxPipeline()
	front := pipe.HMGet(ctx, "battery:0", "present", "state")
	rear := pipe.HMGet(ctx, "battery:1", "present", "state")
	fault := pipe.HMGet(ctx, "engine-ecu", "fault:code")
	if _, err := pipe.Exec(ctx); err != nil {
		return types.DriveStartInputs{}, err
	}
	active := func(fields []interface{}) bool {
		return len(fields) == 2 && fields[0] == "true" && fields[1] == "active"
	}
	return types.DriveStartInputs{
		ActiveBattery: active(front.Val()) || active(rear.Val()),
		ECUHealthy:    len(fault.Val()) == 1 && fault.Val()[0] == "0",
	}, nil
}

func (r *RedisClient) SetDriveStartStatus(status string) error {
	return r.vehiclePub.Set("drive-start-status", status, ipc.Sync())
}

// Receive preserves subscription notifications: a reconnect invalidates the
// previous motion sample, even when no subsequent CAN frame arrives.
func (r *RedisClient) runDriveMotionSubscriber() {
	defer r.wg.Done()
	notify := func(payload string) {
		if r.callbacks.DriveMotionCallback != nil {
			r.callbacks.DriveMotionCallback(payload)
		}
	}
	defer notify("")
	for r.ctx.Err() == nil {
		pubsub := r.client.Raw().Subscribe(r.ctx, "engine-ecu:motion")
		closeOnCancel := context.AfterFunc(r.ctx, func() { _ = pubsub.Close() })
		for r.ctx.Err() == nil {
			msg, err := pubsub.Receive(r.ctx)
			if err != nil {
				break
			}
			switch msg := msg.(type) {
			case *redis.Subscription:
				notify("")
			case *redis.Message:
				notify(msg.Payload)
			}
		}
		closeOnCancel()
		_ = pubsub.Close()
		notify("")
		timer := time.NewTimer(time.Second)
		select {
		case <-r.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
