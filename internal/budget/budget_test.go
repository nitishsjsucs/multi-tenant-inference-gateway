package budget

import (
	"context"
	"errors"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// 12:00:15 UTC, a quarter of the way into a minute window.
var t0 = time.Date(2026, 10, 21, 12, 0, 15, 0, time.UTC)

type harness struct {
	*Limiter
	mr  *miniredis.Miniredis
	now time.Time
}

func newHarness(t *testing.T, failOpen bool) *harness {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1, DialerRetries: 1})
	t.Cleanup(func() { rdb.Close() })
	h := &harness{Limiter: NewLimiter(rdb, "test", failOpen, time.Second), mr: mr, now: t0}
	h.Now = func() time.Time { return h.now }
	return h
}

func (h *harness) reserve(t *testing.T, limits Limits, tokens int64) Decision {
	t.Helper()
	d, err := h.Reserve(context.Background(), "acme", limits, tokens)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func near(a, b time.Duration) bool { return math.Abs(float64(a-b)) < float64(10*time.Millisecond) }

func TestMinuteLimit(t *testing.T) {
	h := newHarness(t, true)
	lim := Limits{TokensPerMinute: 1000}

	d := h.reserve(t, lim, 300)
	if !d.Allowed || d.MinuteRemaining != 700 || d.Reservation == nil {
		t.Fatalf("first reserve: %+v", d)
	}
	if !near(d.MinuteReset, 45*time.Second) {
		t.Errorf("MinuteReset = %v, want 45s", d.MinuteReset)
	}
	if d = h.reserve(t, lim, 600); !d.Allowed || d.MinuteRemaining != 100 {
		t.Fatalf("second reserve: %+v", d)
	}
	d = h.reserve(t, lim, 200)
	if d.Allowed || d.Reason != ReasonMinute || d.TooLarge {
		t.Fatalf("third reserve should be rejected: %+v", d)
	}
	// 900 used now; the bucket must roll over (45s) and decay until
	// 900*(1-t/60s) + 200 <= 1000, i.e. t = 6.67s.
	if want := 45*time.Second + 6666666666*time.Nanosecond; !near(d.RetryAfter, want) {
		t.Errorf("RetryAfter = %v, want %v", d.RetryAfter, want)
	}
	if got := h.mr.TTL("test:{acme}:tpm:" + itoa(t0.Unix()/60)); got <= 60*time.Second {
		t.Errorf("minute bucket TTL = %v, want > 60s so it can serve as the previous bucket", got)
	}
}

func TestSlidingWindowDecaysPreviousMinute(t *testing.T) {
	h := newHarness(t, true)
	lim := Limits{TokensPerMinute: 1000}
	if d := h.reserve(t, lim, 800); !d.Allowed {
		t.Fatal("setup reserve rejected")
	}
	// Halfway through the next minute the previous bucket counts for 400.
	h.now = t0.Truncate(time.Minute).Add(90 * time.Second)
	d := h.reserve(t, lim, 700)
	if d.Allowed {
		t.Fatalf("400 + 700 > 1000 should be rejected: %+v", d)
	}
	// 800*(1-(30s+t)/60s) + 700 <= 1000  =>  t = 7.5s
	if !near(d.RetryAfter, 7500*time.Millisecond) {
		t.Errorf("RetryAfter = %v, want 7.5s", d.RetryAfter)
	}
	if d = h.reserve(t, lim, 600); !d.Allowed || d.MinuteRemaining != 0 {
		t.Fatalf("400 + 600 fits exactly: %+v", d)
	}
}

func TestDayLimit(t *testing.T) {
	h := newHarness(t, true)
	lim := Limits{TokensPerDay: 1000}
	if d := h.reserve(t, lim, 900); !d.Allowed || d.DayRemaining != 100 || d.MinuteRemaining != -1 {
		t.Fatalf("first reserve: %+v", d)
	}
	d := h.reserve(t, lim, 200)
	if d.Allowed || d.Reason != ReasonDay {
		t.Fatalf("should hit the day limit: %+v", d)
	}
	if want := 12*time.Hour - 15*time.Second; d.RetryAfter != want {
		t.Errorf("RetryAfter = %v, want %v (next UTC midnight)", d.RetryAfter, want)
	}
}

func TestTooLarge(t *testing.T) {
	h := newHarness(t, true)
	d := h.reserve(t, Limits{TokensPerMinute: 100}, 101)
	if d.Allowed || !d.TooLarge || d.RetryAfter != 0 {
		t.Fatalf("a request larger than the limit can never fit: %+v", d)
	}
}

func TestReconcile(t *testing.T) {
	h := newHarness(t, true)
	lim := Limits{TokensPerMinute: 1000, TokensPerDay: 5000}
	d := h.reserve(t, lim, 500)
	ctx := context.Background()

	if err := h.Reconcile(ctx, d.Reservation, 120); err != nil {
		t.Fatal(err)
	}
	minuteKey := "test:{acme}:tpm:" + itoa(t0.Unix()/60)
	if got, _ := h.mr.Get(minuteKey); got != "120" {
		t.Errorf("minute bucket after refund = %s, want 120", got)
	}
	if got, _ := h.mr.Get("test:{acme}:tpd:20261021"); got != "120" {
		t.Errorf("day bucket after refund = %s, want 120", got)
	}
	if d := h.reserve(t, lim, 880); !d.Allowed {
		t.Fatal("refunded tokens should be reusable")
	}

	// An overrun (estimate too low) is charged.
	small := &Reservation{Tokens: 10, minuteKey: minuteKey, dayKey: "test:{acme}:tpd:20261021"}
	if err := h.Reconcile(ctx, small, 60); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.mr.Get(minuteKey); got != "1050" {
		t.Errorf("minute bucket after overrun = %s, want 1050", got)
	}
}

func TestReconcileNeverGoesNegativeAndSkipsExpired(t *testing.T) {
	h := newHarness(t, true)
	d := h.reserve(t, Limits{TokensPerMinute: 1000}, 100)
	h.mr.Set(d.Reservation.minuteKey, "40") // e.g. concurrent refunds raced ahead
	h.mr.SetTTL(d.Reservation.minuteKey, 2*time.Minute)
	if err := h.Reconcile(context.Background(), d.Reservation, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.mr.Get(d.Reservation.minuteKey); got != "0" {
		t.Errorf("bucket = %s, want clamped to 0", got)
	}
	h.mr.FastForward(26 * time.Hour)
	if err := h.Reconcile(context.Background(), d.Reservation, 500); err != nil {
		t.Fatal(err)
	}
	if h.mr.Exists(d.Reservation.minuteKey) || h.mr.Exists(d.Reservation.dayKey) {
		t.Error("reconcile must not recreate expired buckets")
	}
}

func TestRedisOutage(t *testing.T) {
	lim := Limits{TokensPerMinute: 1000}
	for _, failOpen := range []bool{true, false} {
		h := newHarness(t, failOpen)
		var failures int
		h.OnFailure = func(error) { failures++ }
		h.mr.Close()

		d, err := h.Reserve(context.Background(), "acme", lim, 10)
		if failOpen {
			if err != nil || !d.Allowed || !d.Degraded || d.Reservation != nil {
				t.Errorf("fail-open: got %+v, %v", d, err)
			}
		} else if !errors.Is(err, ErrUnavailable) {
			t.Errorf("fail-closed: err = %v, want ErrUnavailable", err)
		}
		if failures != 1 {
			t.Errorf("OnFailure called %d times, want 1", failures)
		}
		// Unlimited tenants never touch Redis.
		if d, err := h.Reserve(context.Background(), "acme", Limits{}, 10); err != nil || !d.Allowed {
			t.Errorf("unlimited tenant: %+v, %v", d, err)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
