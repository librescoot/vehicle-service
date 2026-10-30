package messaging

import (
	"context"
	"time"

	ipc "github.com/librescoot/redis-ipc"
	"vehicle-service/internal/types"
)

func (r *RedisClient) ReadDriveStartInputs(ctx context.Context) (types.DriveStartInputs, error) {
	ctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	pipe := r.client.Raw().TxPipeline()
	front := pipe.HMGet(ctx, "battery:0", "present", "state")
	rear := pipe.HMGet(ctx, "battery:1", "present", "state")
	if _, err := pipe.Exec(ctx); err != nil {
		return types.DriveStartInputs{}, err
	}
	active := func(fields []interface{}) bool {
		return len(fields) == 2 && fields[0] == "true" && fields[1] == "active"
	}
	return types.DriveStartInputs{
		ActiveBattery: active(front.Val()) || active(rear.Val()),
	}, nil
}

func (r *RedisClient) SetDriveStartStatus(status string) error {
	return r.vehiclePub.Set("drive-start-status", status, ipc.Sync())
}
