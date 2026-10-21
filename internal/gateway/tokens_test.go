package gateway

import (
	"context"
	"testing"
	"time"
)

func TestTokenPoolRoundRobinAllowsConcurrentLeases(t *testing.T) {
	config := testConfig(nil)
	config.AccountTokens = []string{"account-a", "account-b"}
	pool := newTokenPool(config)
	first, _, err := pool.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := pool.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.token != "account-a" || second.token != "account-b" {
		t.Fatalf("round-robin tokens = (%q, %q)", first.token, second.token)
	}

	third, _, err := pool.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if third.token != "account-a" {
		t.Fatalf("round-robin token = %q, want account-a while its first lease is active", third.token)
	}
	// Every lease is active simultaneously; the pool does not serialize by token.
}

func TestTokenPoolRateLimitCooldownIsPerAccount(t *testing.T) {
	config := testConfig(nil)
	config.AccountTokens = []string{"account-a", "account-b"}
	config.BaseBackoff = 20 * time.Millisecond
	config.MaxBackoff = 100 * time.Millisecond
	config.Jitter = 0
	pool := newTokenPool(config)
	first, _, err := pool.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	delay, sharedWait := pool.rateLimited(first, 0)
	if delay != 20*time.Millisecond || sharedWait != 0 {
		t.Fatalf("first rate-limit delay = (%s, %s)", delay, sharedWait)
	}
	second, _, err := pool.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.token != "account-b" {
		t.Fatalf("cooldown should leave account-b available, got %q", second.token)
	}
	third, _, err := pool.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if third.token != "account-b" {
		t.Fatalf("cooldown should keep assigning the available account-b, got %q", third.token)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	time.Sleep(25 * time.Millisecond)
	pool.recordSuccess(second)
	pool.recordSuccess(third)
	recovered, _, err := pool.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.token != "account-a" {
		t.Fatalf("rate-limited account after cooldown = %q", recovered.token)
	}
	pool.recordSuccess(recovered)
	accountB, _, err := pool.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if accountB.token != "account-b" {
		t.Fatalf("account-b was not reusable: %q", accountB.token)
	}
	pool.recordSuccess(accountB)
}

func TestTokenPoolCooldownWaitHonorsCancellation(t *testing.T) {
	config := testConfig(nil)
	config.AccountTokens = []string{"provider-a"}
	config.BaseBackoff = time.Second
	config.MaxBackoff = time.Second
	config.Jitter = 0
	pool := newTokenPool(config)
	lease, _, err := pool.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pool.rateLimited(lease, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, _, err := pool.acquire(ctx); err == nil {
		t.Fatal("acquire succeeded before the token cooldown expired")
	}
}

func TestTokenPoolBackoffEscalatesAndResetsAfterSuccess(t *testing.T) {
	config := testConfig(nil)
	config.AccountTokens = []string{"provider-a"}
	config.BaseBackoff = 5 * time.Millisecond
	config.MaxBackoff = 20 * time.Millisecond
	config.Jitter = 0
	pool := newTokenPool(config)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, want := range []time.Duration{5 * time.Millisecond, 10 * time.Millisecond} {
		lease, _, err := pool.acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := pool.rateLimited(lease, 0)
		if got != want {
			t.Fatalf("cooldown = %s, want %s", got, want)
		}
	}
	lease, _, err := pool.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pool.recordSuccess(lease)
	lease, _, err = pool.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := pool.rateLimited(lease, 0); got != 5*time.Millisecond {
		t.Fatalf("cooldown after a successful response = %s, want 5ms", got)
	}
}

func TestTokenPoolOlderSuccessDoesNotEraseConcurrentRateLimit(t *testing.T) {
	config := testConfig(nil)
	config.AccountTokens = []string{"provider-a"}
	config.BaseBackoff = 25 * time.Millisecond
	config.MaxBackoff = 100 * time.Millisecond
	config.Jitter = 0
	pool := newTokenPool(config)
	limited, _, err := pool.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	olderSuccess, _, err := pool.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first, _ := pool.rateLimited(limited, 0); first != 25*time.Millisecond {
		t.Fatalf("initial backoff = %s", first)
	}
	pool.recordSuccess(olderSuccess)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	next, _, err := pool.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second, _ := pool.rateLimited(next, 0); second != 50*time.Millisecond {
		t.Fatalf("concurrent stale success reset the 429 backoff: got %s, want 50ms", second)
	}
}
