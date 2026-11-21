package platform

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"
)

const nodeResourceReuse = time.Second

// ErrNodeObservationDeferred means no probe was attempted. Callers must not
// treat a background cooldown or an old connection as a new health failure.
var ErrNodeObservationDeferred = errors.New("background node observation deferred")

type NodeResourceObservation struct {
	// Data is immutable and shared by the background consumers.
	Data      Row
	SampledAt time.Time
}

type NodeInventoryObservation struct {
	HealthSampledAt time.Time
	Online          bool
	Resource        NodeResourceObservation
}

type nodeConnectionKey struct {
	endpoint, token, pin string
	keyVersion           uint64
}

func observationConnection(node Row) nodeConnectionKey {
	return nodeConnectionKey{Text(node["agent_endpoint"]), Text(node["token_ciphertext"]), Text(node["server_cert_sha256"]), Uint(node["key_version"])}
}

func SameNodeConnection(a, b Row) bool {
	return observationConnection(a) == observationConnection(b)
}

type nodeObservationFlight struct {
	done      chan struct{}
	inventory bool
	result    NodeInventoryObservation
	err       error
}

type nodeObservationState struct {
	connection nodeConnectionKey
	revision   uint64
	flight     *nodeObservationFlight
	cached     NodeResourceObservation
	failures   int
	retryAt    time.Time
}

type nodeObservations struct {
	mu    sync.Mutex
	nodes map[uint64]*nodeObservationState
}

// Only background health and resource reads use this gate. Explicit recovery
// probes and native task requests keep their existing independent dispatch.
func (a *App) ObserveNodeInventory(ctx context.Context, node Row) (NodeInventoryObservation, error) {
	return a.observeNode(ctx, node, true)
}

func (a *App) ObserveNodeResources(ctx context.Context, node Row) (NodeResourceObservation, error) {
	result, err := a.observeNode(ctx, node, false)
	return result.Resource, err
}

func (a *App) observeNode(ctx context.Context, node Row, inventory bool) (NodeInventoryObservation, error) {
	// Pin the request to the exact connection used for cache lookup and the
	// consumer's write fence, rather than reloading it half way through a probe.
	var client *http.Client
	var origin string
	return a.nodeObservations.observe(ctx, node, inventory, a.Time, jitterObservationBackoff, func(ctx context.Context, path string) (Row, error) {
		if client == nil {
			cfg, err := a.agentConnection(node)
			if err != nil {
				return nil, err
			}
			client, origin, err = a.agentClient(cfg)
			if err != nil {
				return nil, err
			}
		}
		out, _, err := requestAgent(ctx, client, origin, http.MethodGet, path, nil, "")
		return out, err
	})
}

func (o *nodeObservations) observe(ctx context.Context, node Row, inventory bool, now func() time.Time, jitter func(time.Duration) time.Duration, request func(context.Context, string) (Row, error)) (NodeInventoryObservation, error) {
	id, revision, connection := Uint(node["id"]), Uint(node["revision"]), observationConnection(node)
	var joinedAt time.Time
	for {
		if err := ctx.Err(); err != nil {
			return NodeInventoryObservation{}, err
		}
		o.mu.Lock()
		state := o.nodes[id]
		if state == nil {
			if o.nodes == nil {
				o.nodes = make(map[uint64]*nodeObservationState)
			}
			state = &nodeObservationState{connection: connection, revision: revision}
			o.nodes[id] = state
		} else if state.connection != connection {
			// A changed trust document cannot consume the old flight or inherit
			// its cooldown. Do not start a parallel background probe, either.
			if state.flight != nil || revision < state.revision {
				o.mu.Unlock()
				return NodeInventoryObservation{}, ErrNodeObservationDeferred
			}
			*state = nodeObservationState{connection: connection, revision: revision}
		}
		state.revision = max(state.revision, revision)
		if flight := state.flight; flight != nil {
			o.mu.Unlock()
			select {
			case <-ctx.Done():
				return NodeInventoryObservation{}, ctx.Err()
			case <-flight.done:
				if !inventory && flight.inventory && flight.err == nil {
					// The inventory flight may have joined a prior resource
					// request before this caller arrived. Only its own waiters
					// may consume that result beyond the brief reuse window.
					at := flight.result.Resource.SampledAt
					if at.Before(flight.result.HealthSampledAt) && !ObservationFresh(now(), at, nodeResourceReuse) {
						continue
					}
				}
				if !inventory || flight.inventory {
					return flight.result, flight.err
				}
				// Read health separately, but keep the resources observation we
				// already waited for even if the brief cache window has passed.
				joinedAt = flight.result.Resource.SampledAt
				continue
			}
		}
		at := now()
		if at.Before(state.retryAt) {
			o.mu.Unlock()
			return NodeInventoryObservation{}, ErrNodeObservationDeferred
		}
		reuseFor := nodeResourceReuse
		if !joinedAt.IsZero() && joinedAt.Equal(state.cached.SampledAt) {
			reuseFor = NodeInventoryMaxAge
		}
		if !ObservationFresh(at, state.cached.SampledAt, reuseFor) {
			state.cached = NodeResourceObservation{}
		}
		cached := state.cached
		if !inventory && cached.Data != nil {
			o.mu.Unlock()
			return NodeInventoryObservation{Resource: cached}, nil
		}
		flight := &nodeObservationFlight{done: make(chan struct{}), inventory: inventory}
		state.flight = flight
		o.mu.Unlock()

		call, cancel := context.WithTimeout(ctx, 10*time.Second)
		flight.result, flight.err = collectNodeObservation(call, inventory, cached, reuseFor, now, request)
		cancel()
		o.mu.Lock()
		if flight.err == nil {
			state.cached = flight.result.Resource
			state.failures, state.retryAt = 0, time.Time{}
		} else if ctx.Err() == nil {
			state.cached = NodeResourceObservation{}
			state.failures = min(state.failures+1, 5)
			state.retryAt = now().Add(jitter(min(30*time.Second, time.Second<<state.failures)))
		}
		state.flight = nil
		close(flight.done)
		o.mu.Unlock()
		return flight.result, flight.err
	}
}

func jitterObservationBackoff(base time.Duration) time.Duration {
	// Spread retries over the last 10% of each interval and never exceed 30s.
	return base - rand.N(base/10)
}

func collectNodeObservation(ctx context.Context, inventory bool, cached NodeResourceObservation, reuseFor time.Duration, now func() time.Time, request func(context.Context, string) (Row, error)) (NodeInventoryObservation, error) {
	var out NodeInventoryObservation
	if inventory {
		out.HealthSampledAt = now()
		health, err := request(ctx, "/agent/v1/health")
		if err != nil {
			return out, err
		}
		if Text(health["status"]) != "ok" || !Bool(health["incus"]) {
			return out, E(503, "node_unavailable", "Agent or Incus health check failed")
		}
		out.Online = true
	}
	at := now()
	if ObservationFresh(at, cached.SampledAt, reuseFor) {
		out.Resource = cached
		return out, nil
	}
	data, err := request(ctx, "/agent/v1/resources")
	if err != nil {
		return out, err
	}
	if _, ok := data["server_resources"].(map[string]any); !ok {
		return out, E(503, "observation_incomplete", "Agent hardware inventory is incomplete")
	}
	gpus, ok := data["gpus"].([]any)
	if !ok {
		return out, E(503, "observation_incomplete", "Agent GPU inventory is incomplete")
	}
	for _, gpu := range gpus {
		if _, ok := gpu.(map[string]any); !ok {
			return out, E(503, "observation_incomplete", "Agent GPU inventory is malformed")
		}
	}
	out.Resource = NodeResourceObservation{Data: data, SampledAt: at}
	return out, nil
}
