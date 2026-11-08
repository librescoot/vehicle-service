package messaging

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Boot identity and a short expiry prevent an unconsumed request honking on a
// later boot or after a stopped service finally resumes its queue.
func validateHornChirp(command, bootID string, now time.Time) error {
	parts := strings.Split(command, ":")
	if len(parts) != 3 || parts[0] != "chirp" || bootID == "" || parts[1] != bootID {
		return fmt.Errorf("invalid horn chirp boot identity")
	}
	expires, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || expires <= now.Unix() || expires > now.Unix()+5 {
		return fmt.Errorf("horn chirp is expired or outside its five-second window")
	}
	return nil
}

func (r *RedisClient) advertiseHornChirp() error {
	if r.callbacks.HornChirpCallback == nil {
		return nil
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	r.chirpBootID = strings.TrimSpace(string(boot))
	if r.chirpBootID == "" {
		return fmt.Errorf("boot identity is empty")
	}
	return r.vehiclePub.Set("horn:chirp-capability", "v1:"+r.chirpBootID)
}
