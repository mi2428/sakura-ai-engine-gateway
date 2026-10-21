package gateway

import (
	"context"
	"sync"
	"time"
)

type tokenSlot struct {
	value         string
	cooldownUntil time.Time
	rateLimitRun  int
}

type tokenLease struct {
	index int
	token string
}

type tokenSlotMetricSnapshot struct {
	cooldown        time.Duration
	rateLimitStreak int
}

// tokenPool rotates through every configured token and tracks provider-driven
// cooldowns within one process. It does not serialize requests per token.
type tokenPool struct {
	mu      sync.Mutex
	slots   []tokenSlot
	next    int
	changed chan struct{}
	config  Config
}

func newTokenPool(config Config) *tokenPool {
	slots := make([]tokenSlot, len(config.AccountTokens))
	for index, token := range config.AccountTokens {
		slots[index].value = token
	}
	return &tokenPool{
		slots:   slots,
		changed: make(chan struct{}),
		config:  config,
	}
}

func (pool *tokenPool) acquire(ctx context.Context) (tokenLease, time.Duration, error) {
	started := time.Now()
	for {
		pool.mu.Lock()
		now := time.Now()
		for offset := range len(pool.slots) {
			index := (pool.next + offset) % len(pool.slots)
			slot := &pool.slots[index]
			if !slot.cooldownUntil.After(now) {
				pool.next = (index + 1) % len(pool.slots)
				pool.mu.Unlock()
				return tokenLease{index: index, token: slot.value}, time.Since(started), nil
			}
		}

		changed := pool.changed
		wait := time.Duration(0)
		for index := range pool.slots {
			slot := &pool.slots[index]
			remaining := max(0, slot.cooldownUntil.Sub(now))
			if index == 0 || remaining < wait {
				wait = remaining
			}
		}
		pool.mu.Unlock()

		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return tokenLease{}, time.Since(started), ctx.Err()
			case <-changed:
				timer.Stop()
			case <-timer.C:
			}
			continue
		}

		select {
		case <-ctx.Done():
			return tokenLease{}, time.Since(started), ctx.Err()
		case <-changed:
		}
	}
}

// recordSuccess resets 429 backoff only after the cooldown has passed. With
// concurrent leases, an older success must not undo a newer provider 429.
func (pool *tokenPool) recordSuccess(lease tokenLease) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	slot := &pool.slots[lease.index]
	if !slot.cooldownUntil.After(time.Now()) {
		slot.rateLimitRun = 0
	}
}

func (pool *tokenPool) rateLimited(lease tokenLease, providerDelay time.Duration) (time.Duration, time.Duration) {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	slot := &pool.slots[lease.index]
	delay := retryDelay(pool.config, slot.rateLimitRun, providerDelay)
	slot.rateLimitRun++
	if until := time.Now().Add(delay); until.After(slot.cooldownUntil) {
		slot.cooldownUntil = until
	}
	pool.notifyLocked()
	return delay, pool.nextAvailableWaitLocked(time.Now())
}

func (pool *tokenPool) nextAvailableWaitLocked(now time.Time) time.Duration {
	var wait time.Duration
	found := false
	for index := range pool.slots {
		slot := &pool.slots[index]
		remaining := max(0, slot.cooldownUntil.Sub(now))
		if !found || remaining < wait {
			wait = remaining
			found = true
		}
	}
	if !found {
		return 0
	}
	return wait
}

func (pool *tokenPool) notifyLocked() {
	close(pool.changed)
	pool.changed = make(chan struct{})
}

func (pool *tokenPool) metricsSnapshot() []tokenSlotMetricSnapshot {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	now := time.Now()
	snapshot := make([]tokenSlotMetricSnapshot, len(pool.slots))
	for index, slot := range pool.slots {
		snapshot[index] = tokenSlotMetricSnapshot{
			cooldown:        max(0, slot.cooldownUntil.Sub(now)),
			rateLimitStreak: slot.rateLimitRun,
		}
	}
	return snapshot
}
