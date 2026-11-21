package platform

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var testNodeResources = Row{"server_resources": Row{}, "gpus": []any{}}

func noObservationJitter(d time.Duration) time.Duration { return d }

// Signal that the caller is actually selecting on a pending result, not merely
// that its goroutine has started.
type observationWaitContext struct {
	context.Context
	once    sync.Once
	waiting chan struct{}
}

func (c *observationWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestNodeObservationSharesInFlightAndOriginalSample(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var broker nodeObservations
	start := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	at := start
	now := func() time.Time { return at }
	node := Row{"id": 1, "revision": 1, "agent_endpoint": "https://first"}
	replacement := Row{"id": 1, "revision": 2, "agent_endpoint": "https://second"}
	entered, release := make(chan struct{}, 2), make(chan struct{})
	var healthCalls, resourceCalls atomic.Int32
	request := func(ctx context.Context, path string) (Row, error) {
		if path == "/agent/v1/health" {
			healthCalls.Add(1)
			return Row{"status": "ok", "incus": true}, nil
		}
		resourceCalls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
			return testNodeResources, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	type observationResult struct {
		out NodeInventoryObservation
		err error
	}
	observe := func(ctx context.Context, inventory bool) <-chan observationResult {
		result := make(chan observationResult, 1)
		go func() {
			out, err := broker.observe(ctx, node, inventory, now, noObservationJitter, request)
			result <- observationResult{out, err}
		}()
		return result
	}
	inventory := observe(ctx, true)
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("inventory never started its resources request")
	}
	waiter := &observationWaitContext{Context: ctx, waiting: make(chan struct{})}
	load := observe(waiter, false)
	select {
	case <-waiter.waiting:
	case <-entered:
		t.Fatal("load started another resources request during inventory")
	case <-ctx.Done():
		t.Fatal("load never joined inventory's resources request")
	}
	if _, err := broker.observe(ctx, replacement, false, now, noObservationJitter, request); !errors.Is(err, ErrNodeObservationDeferred) {
		t.Fatalf("replacement joined an old connection's flight: %v", err)
	}
	close(release)
	for _, result := range []<-chan observationResult{inventory, load} {
		got := <-result
		if got.err != nil || !got.out.Online || !got.out.HealthSampledAt.Equal(start) || !got.out.Resource.SampledAt.Equal(start) {
			t.Fatalf("coalesced observation = %+v, %v", got.out, got.err)
		}
	}
	at = start.Add(nodeResourceReuse / 2)
	gpu, err := broker.observe(ctx, node, false, now, noObservationJitter, request)
	if err != nil || !gpu.Resource.SampledAt.Equal(start) || gpu.Resource.Data == nil {
		t.Fatalf("fresh reuse = %+v, %v", gpu, err)
	}
	repeated, err := broker.observe(ctx, node, true, now, noObservationJitter, request)
	if err != nil || !repeated.HealthSampledAt.Equal(at) || !repeated.Resource.SampledAt.Equal(start) || resourceCalls.Load() != 1 || healthCalls.Load() != 2 {
		t.Fatalf("fresh health must preserve the shared resource time: %+v, %v, resources=%d health=%d", repeated, err, resourceCalls.Load(), healthCalls.Load())
	}
	at = start.Add(nodeResourceReuse + time.Microsecond)
	fresh, err := broker.observe(ctx, node, false, now, noObservationJitter, request)
	if err != nil || !fresh.Resource.SampledAt.Equal(at) || resourceCalls.Load() != 2 {
		t.Fatalf("expired cache was not refreshed: %+v, %v", fresh, err)
	}
	at = at.Add(time.Microsecond)
	fresh, err = broker.observe(ctx, replacement, false, now, noObservationJitter, request)
	if err != nil || !fresh.Resource.SampledAt.Equal(at) || resourceCalls.Load() != 3 {
		t.Fatalf("replacement reused the former connection's data: %+v, %v", fresh, err)
	}
}

func TestNodeInventoryChecksHealthAfterJoiningResources(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var broker nodeObservations
	start := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	var elapsed atomic.Int64
	now := func() time.Time { return start.Add(time.Duration(elapsed.Load())) }
	node := Row{"id": 1}
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var resourceCalls atomic.Int32
	request := func(ctx context.Context, path string) (Row, error) {
		if path == "/agent/v1/health" {
			return Row{"status": "ok", "incus": true}, nil
		}
		resourceCalls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
			return testNodeResources, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	load := make(chan error, 1)
	go func() {
		_, err := broker.observe(ctx, node, false, now, noObservationJitter, request)
		load <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("resources request never started")
	}
	waiter := &observationWaitContext{Context: ctx, waiting: make(chan struct{})}
	inventory := make(chan error, 1)
	go func() {
		out, err := broker.observe(waiter, node, true, now, noObservationJitter, request)
		if err == nil && (!out.Online || !out.HealthSampledAt.Equal(now()) || !out.Resource.SampledAt.Equal(start)) {
			err = errors.New("inventory did not check health independently of the original resource sample")
		}
		inventory <- err
	}()
	select {
	case <-waiter.waiting:
	case <-ctx.Done():
		t.Fatal("inventory never joined the resources request")
	}
	elapsed.Store(int64(2 * nodeResourceReuse))
	close(release)
	if err := <-load; err != nil {
		t.Fatal(err)
	}
	if err := <-inventory; err != nil {
		t.Fatal(err)
	}
	if resourceCalls.Load() != 1 {
		t.Fatalf("concurrent observations made %d resources requests", resourceCalls.Load())
	}
}

func TestNodeObservationCooldownIncludesHealthAndResources(t *testing.T) {
	var broker nodeObservations
	at := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { return at }
	node := Row{"id": 1}
	var requests int
	failHealth, failResources := false, false
	request := func(_ context.Context, path string) (Row, error) {
		requests++
		if path == "/agent/v1/health" {
			return Row{"status": "ok", "incus": !failHealth}, nil
		}
		if failResources {
			return Row{"server_resources": Row{}}, nil // malformed success must back off
		}
		return testNodeResources, nil
	}
	observe := func(inventory bool) (NodeInventoryObservation, error) {
		return broker.observe(t.Context(), node, inventory, now, noObservationJitter, request)
	}
	if out, err := observe(false); err != nil || !out.Resource.SampledAt.Equal(at) {
		t.Fatalf("seeding a fresh sample: %+v, %v", out, err)
	}
	for i, delay := range []time.Duration{2, 4, 8, 16, 30, 30, 2} {
		failHealth = i%2 == 0 && i < 6
		failResources = !failHealth
		inventory := failHealth || i == 6
		out, err := observe(inventory)
		if err == nil || errors.Is(err, ErrNodeObservationDeferred) || out.Online != (i == 6) || out.Resource.Data != nil || (inventory && !out.HealthSampledAt.Equal(at)) {
			t.Fatalf("attempt %d did not record the right failure: %+v, %v", i, out, err)
		}
		before := requests
		at = at.Add(delay*time.Second - time.Microsecond)
		for _, inventory := range []bool{false, true} {
			deferred, err := observe(inventory)
			if !errors.Is(err, ErrNodeObservationDeferred) || !deferred.HealthSampledAt.IsZero() || deferred.Resource.Data != nil || requests != before {
				t.Fatalf("attempt %d cooldown did not defer both consumers: %+v, %v, requests=%d", i, deferred, err, requests)
			}
		}
		at = at.Add(time.Microsecond)
		if i >= 5 {
			failHealth, failResources = false, false
			if out, err = observe(true); err != nil || !out.Online || !out.Resource.SampledAt.Equal(at) {
				t.Fatalf("recovery at the cooldown boundary: %+v, %v", out, err)
			}
			// Expire the successful sample before asking the Agent for resources again.
			at = at.Add(nodeResourceReuse + time.Microsecond)
		}
	}
}
