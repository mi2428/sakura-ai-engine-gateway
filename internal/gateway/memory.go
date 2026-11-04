package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	memoryPollInterval = 250 * time.Millisecond
	requestMemoryBase  = 2 << 20
)

var (
	errQueueFull    = errors.New("memory admission queue is full")
	errMemoryClosed = errors.New("memory admission is closed")
)

type memorySnapshot struct {
	used, limit, reserved int64
	active, queued        int
	paused, available     bool
}

// Reservations deliberately overlap measured usage: they cover body-buffer
// growth, JSON inspection and allocations not yet visible in memory.current.
type memoryGate struct {
	mu sync.Mutex
	memorySnapshot
	readUsage func() (int64, error)
	maxQueued int
	changed   chan struct{}
	closed    bool
}

func newMemoryGate(config Config) (*memoryGate, error) {
	limit, err := readMemoryValue(filepath.Join(config.MemoryCgroupPath, "memory.max"))
	if err != nil || limit <= 0 {
		return nil, errors.New("memory admission requires a readable, finite, positive cgroup v2 memory.max")
	}
	gate := &memoryGate{
		limit: limit,
		readUsage: func() (int64, error) {
			return readMemoryValue(filepath.Join(config.MemoryCgroupPath, "memory.current"))
		},
		maxQueued: config.MaxQueuedRequests,
		changed:   make(chan struct{}),
	}
	if requestMemoryBase+4*config.MaxRequestBytes > gate.resumeAt() {
		return nil, errors.New("cgroup memory.max is too small for GATEWAY_MAX_REQUEST_BYTES")
	}
	if err := gate.sample(); err != nil {
		return nil, fmt.Errorf("read cgroup memory.current: %w", err)
	}
	return gate, nil
}

func readMemoryValue(path string) (int64, error) {
	// #nosec G304 -- the directory is operator configuration, never HTTP input.
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || value < 0 {
		return 0, errors.New("invalid cgroup memory value")
	}
	return value, nil
}

func (gate *memoryGate) pauseAt() int64  { return gate.limit / 4 * 3 }
func (gate *memoryGate) resumeAt() int64 { return gate.limit / 3 * 2 }

// sample is called under mu after construction. A failed sample never admits
// work using a stale usage value.
func (gate *memoryGate) sample() error {
	used, err := gate.readUsage()
	gate.available = err == nil
	if err != nil {
		return err
	}
	gate.used = used
	if used >= gate.pauseAt() {
		gate.paused = true
	} else if used <= gate.resumeAt() {
		gate.paused = false
	}
	return nil
}

func requestMemoryCost(request *http.Request, maximum int64) int64 {
	size := request.ContentLength
	if size < 0 {
		size = maximum
	}
	return requestMemoryBase + 4*size
}

func (gate *memoryGate) acquire(ctx context.Context, cost int64) error {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	queued := false
	var ticker *time.Ticker
	defer func() {
		if queued {
			gate.queued--
			ticker.Stop()
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if gate.closed {
			return errMemoryClosed
		}
		if err := gate.sample(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !gate.paused && cost <= gate.pauseAt()-gate.used-gate.reserved {
			gate.reserved += cost
			gate.active++
			return nil
		}
		if !queued {
			if gate.queued >= gate.maxQueued {
				return errQueueFull
			}
			gate.queued++
			queued = true
			ticker = time.NewTicker(memoryPollInterval)
		}
		changed := gate.changed
		gate.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		case <-ticker.C:
		}
		gate.mu.Lock()
	}
}

func (gate *memoryGate) release(cost int64) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	gate.reserved -= cost
	gate.active--
	close(gate.changed)
	gate.changed = make(chan struct{})
}

func (gate *memoryGate) snapshot() memorySnapshot {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	_ = gate.sample() // available explicitly reports a failed sample.
	return gate.memorySnapshot
}

func (gate *memoryGate) close() {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if !gate.closed {
		gate.closed = true
		close(gate.changed)
		gate.changed = make(chan struct{})
	}
}
