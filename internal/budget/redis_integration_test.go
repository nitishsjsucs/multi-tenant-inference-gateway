package budget

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// TestRealRedis runs the Lua scripts on a real Redis server, since the unit
// tests use miniredis's Lua interpreter. Set MTIG_TEST_REDIS_ADDR to enable.
func TestRealRedis(t *testing.T) {
	addr := os.Getenv("MTIG_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set MTIG_TEST_REDIS_ADDR=host:port to run against a real Redis")
	}
	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	defer rdb.Close()
	prefix := fmt.Sprintf("mtig-it-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		keys, _ := rdb.Keys(ctx, prefix+":*").Result()
		if len(keys) > 0 {
			rdb.Del(ctx, keys...)
		}
	})
	l := NewLimiter(rdb, prefix, false, time.Second)
	now := t0
	l.Now = func() time.Time { return now }
	lim := Limits{TokensPerMinute: 1000, TokensPerDay: 1500}

	d, err := l.Reserve(ctx, "acme", lim, 800)
	if err != nil || !d.Allowed {
		t.Fatalf("reserve: %+v %v", d, err)
	}
	if err := l.Reconcile(ctx, d.Reservation, 600); err != nil {
		t.Fatal(err)
	}
	if v, _ := rdb.Get(ctx, d.Reservation.minuteKey).Int(); v != 600 {
		t.Errorf("minute bucket = %d, want 600", v)
	}
	if ttl, _ := rdb.PTTL(ctx, d.Reservation.minuteKey).Result(); ttl <= time.Minute {
		t.Errorf("minute bucket TTL = %v", ttl)
	}

	// Halfway into the next minute, the 600 from the previous minute weighs 300;
	// 701 more fits once it decays below 299, i.e. after 100ms.
	now = t0.Truncate(time.Minute).Add(90 * time.Second)
	if d, _ := l.Reserve(ctx, "acme", lim, 701); d.Allowed || d.Reason != ReasonMinute || !near(d.RetryAfter, 100*time.Millisecond) {
		t.Errorf("sliding window: %+v", d)
	}
	if d, _ := l.Reserve(ctx, "acme", lim, 700); !d.Allowed || d.MinuteRemaining != 0 || d.DayRemaining != 200 {
		t.Errorf("exact fit: %+v", d)
	}
	if d, _ := l.Reserve(ctx, "acme", Limits{TokensPerDay: 1500}, 201); d.Allowed || d.Reason != ReasonDay {
		t.Errorf("day limit: %+v", d)
	}
}
