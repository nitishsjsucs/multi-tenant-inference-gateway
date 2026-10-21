// Package budget enforces per-tenant token budgets in Redis.
//
// Each request reserves its worst-case cost (estimated prompt tokens plus
// max_tokens) before it is forwarded, atomically checking two limits:
//
//   - tokens per minute, over a sliding window approximated from two fixed
//     one-minute buckets: used = prev * (1 - elapsed/60s) + current
//   - tokens per UTC day, in a fixed daily bucket
//
// When the response finishes, Reconcile adjusts the same buckets by
// (actual - reserved), refunding unused tokens or charging an overrun.
package budget

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

//go:embed reserve.lua
var reserveSrc string

//go:embed reconcile.lua
var reconcileSrc string

var (
	reserveScript   = redis.NewScript(reserveSrc)
	reconcileScript = redis.NewScript(reconcileSrc)
)

// ErrUnavailable is returned by Reserve when Redis cannot be reached and the
// limiter fails closed.
var ErrUnavailable = errors.New("token budget store unavailable")

const window = time.Minute

// Limits are a tenant's budgets; zero disables a limit.
type Limits struct {
	TokensPerMinute int64
	TokensPerDay    int64
}

// Reason says which limit rejected a request.
type Reason string

// Rejection reasons.
const (
	ReasonMinute Reason = "tokens_per_minute"
	ReasonDay    Reason = "tokens_per_day"
)

// Decision is the outcome of Reserve.
type Decision struct {
	Allowed bool
	Reason  Reason // set when !Allowed
	// TooLarge means the request can never fit the limit, even when idle.
	TooLarge bool
	// RetryAfter is how long until the request would fit, assuming no other
	// traffic (set when !Allowed and !TooLarge).
	RetryAfter time.Duration
	// Degraded means Redis failed and the request was admitted unaccounted.
	Degraded bool

	Limits          Limits
	MinuteRemaining int64
	DayRemaining    int64
	// MinuteReset is when the current minute bucket rolls over.
	MinuteReset time.Duration

	// Reservation must be passed to Reconcile; nil when nothing was reserved.
	Reservation *Reservation
}

// Reservation identifies reserved tokens so they can be reconciled.
type Reservation struct {
	Tokens    int64
	minuteKey string
	dayKey    string
}

// Limiter reserves and reconciles token budgets.
type Limiter struct {
	rdb       redis.Scripter
	prefix    string
	failOpen  bool
	timeout   time.Duration
	Now       func() time.Time
	OnFailure func(error) // observes Redis errors (metrics/logging)
}

// NewLimiter returns a limiter storing keys under prefix.
func NewLimiter(rdb redis.Scripter, prefix string, failOpen bool, timeout time.Duration) *Limiter {
	return &Limiter{rdb: rdb, prefix: prefix, failOpen: failOpen, timeout: timeout, Now: time.Now}
}

func (l *Limiter) keys(tenant string, now time.Time) (cur, prev, day string) {
	minute := now.Unix() / int64(window/time.Second)
	// The {tenant} hash tag keeps all of a tenant's keys in one Redis Cluster slot.
	base := fmt.Sprintf("%s:{%s}", l.prefix, tenant)
	return fmt.Sprintf("%s:tpm:%d", base, minute),
		fmt.Sprintf("%s:tpm:%d", base, minute-1),
		fmt.Sprintf("%s:tpd:%s", base, now.UTC().Format("20060102"))
}

// Reserve atomically checks both limits and reserves tokens if they fit.
func (l *Limiter) Reserve(ctx context.Context, tenant string, limits Limits, tokens int64) (Decision, error) {
	d := Decision{Limits: limits}
	if limits.TokensPerMinute == 0 && limits.TokensPerDay == 0 {
		d.Allowed = true
		return d, nil
	}
	now := l.Now()
	elapsed := time.Duration(now.UnixNano() % int64(window))
	weight := 1 - float64(elapsed)/float64(window)
	d.MinuteReset = window - elapsed
	curKey, prevKey, dayKey := l.keys(tenant, now)

	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	res, err := reserveScript.Run(ctx, l.rdb, []string{curKey, prevKey, dayKey},
		tokens, limits.TokensPerMinute, limits.TokensPerDay,
		strconv.FormatFloat(weight, 'f', 6, 64),
		(2*window + 5*time.Second).Milliseconds(), int64((25 * time.Hour).Seconds()),
	).Int64Slice()
	if err != nil {
		if l.OnFailure != nil {
			l.OnFailure(err)
		}
		if l.failOpen {
			d.Allowed, d.Degraded = true, true
			return d, nil
		}
		return d, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	allowed, reason, cur, prev, day := res[0] == 1, res[1], res[2], res[3], res[4]
	used := int64(math.Floor(float64(prev)*weight)) + cur
	d.MinuteRemaining = remaining(limits.TokensPerMinute, used)
	d.DayRemaining = remaining(limits.TokensPerDay, day)

	if allowed {
		d.Allowed = true
		d.Reservation = &Reservation{Tokens: tokens, minuteKey: curKey, dayKey: dayKey}
		return d, nil
	}
	switch reason {
	case 1:
		d.Reason = ReasonMinute
		d.TooLarge = tokens > limits.TokensPerMinute
		d.RetryAfter = minuteRetryAfter(limits.TokensPerMinute, tokens, cur, prev, elapsed)
	case 2:
		d.Reason = ReasonDay
		d.TooLarge = tokens > limits.TokensPerDay
		y, m, dd := now.UTC().Date()
		d.RetryAfter = time.Date(y, m, dd+1, 0, 0, 0, 0, time.UTC).Sub(now)
	}
	return d, nil
}

// minuteRetryAfter solves for when the sliding-window usage leaves room for
// cost tokens, assuming no further traffic.
func minuteRetryAfter(limit, cost, cur, prev int64, elapsed time.Duration) time.Duration {
	if cost > limit || (cur+cost <= limit && prev == 0) {
		return 0
	}
	w := float64(window)
	if cur+cost <= limit {
		// Wait for the previous bucket to decay: prev*(1-(elapsed+t)/W) <= limit-cur-cost.
		t := w*(1-float64(limit-cur-cost)/float64(prev)) - float64(elapsed)
		return time.Duration(max(t, 0))
	}
	// The current bucket alone is too full: wait for it to become the previous
	// bucket, then for it to decay: cur*(1-t2/W) <= limit-cost.
	t2 := w * (1 - float64(limit-cost)/float64(cur))
	return window - elapsed + time.Duration(t2)
}

func remaining(limit, used int64) int64 {
	if limit == 0 {
		return -1
	}
	return max(limit-used, 0)
}

// Reconcile corrects a reservation once the actual token usage is known.
func (l *Limiter) Reconcile(ctx context.Context, r *Reservation, actual int64) error {
	if r == nil || actual == r.Tokens {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	err := reconcileScript.Run(ctx, l.rdb, []string{r.minuteKey, r.dayKey}, actual-r.Tokens).Err()
	if err != nil && l.OnFailure != nil {
		l.OnFailure(err)
	}
	return err
}
