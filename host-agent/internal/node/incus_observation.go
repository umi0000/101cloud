package node

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
)

const (
	maxConcurrentIncusObservations = 2
	incusObservationCooldown      = 5 * time.Second
)

// The gate covers only local Incus observation I/O, not mutations or host collectors.
// It never queues. After a failure, let older calls drain before admitting the
// sole recovery probe, so their results cannot change the probe's decision.
type incusObservationGate struct {
	mu      sync.Mutex
	now     func() time.Time
	active  int
	retryAt time.Time
}

func (g *incusObservationGate) begin(ctx context.Context) (bool, incusObservationFailure) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, classifyIncusObservationFailure("admission", err)
	}
	probe := !g.retryAt.IsZero()
	if probe && (g.active != 0 || g.now().Before(g.retryAt)) {
		return false, incusObservationFailure{phase: "admission", reason: "cooldown"}
	}
	if g.active >= maxConcurrentIncusObservations {
		return false, incusObservationFailure{phase: "admission", reason: "busy"}
	}
	g.active++
	return probe, incusObservationFailure{}
}

func (g *incusObservationGate) finish(probe bool, failure incusObservationFailure) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active--
	switch {
	case failure.reason == "canceled":
		// A caller that left tells us nothing about Incus. A canceled
		// recovery probe leaves the gate ready for another single probe.
	case probe && failure.reason == "":
		g.retryAt = time.Time{}
	case failure.reason != "" && (probe || g.retryAt.IsZero()):
		g.retryAt = g.now().Add(incusObservationCooldown)
	}
}

func (s *Service) observeIncus(ctx context.Context, observe func(incus.InstanceServer) error) (failure incusObservationFailure) {
	probe, failure := s.observations.begin(ctx)
	if failure.reason != "" {
		return failure
	}
	defer func() { s.observations.finish(probe, failure) }()

	client, err := s.connect(ctx, "default")
	if err != nil {
		return classifyIncusObservationFailure("connect", err)
	}
	defer closeIncus(client)

	// SkipGetServer only creates the client. The real dial occurs lazily on
	// the first request (and again for every request with SDK keepalive off).
	// The SDK's Unix dial ignores context. Bind this private transport's dial
	// to the observation deadline even when net/http detaches its dial context.
	httpClient, err := client.GetHTTPClient()
	if err != nil {
		return classifyIncusObservationFailure("connect", err)
	}
	var connecting atomic.Bool
	httpClient.Transport.(*http.Transport).DialContext = func(_ context.Context, _, _ string) (net.Conn, error) {
		connecting.Store(true)
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", s.socketPath)
		if err == nil {
			connecting.Store(false)
		}
		return conn, err
	}

	err = observe(client)
	if err == nil {
		err = ctx.Err()
	}
	// net/http may return context cancellation before it sees the dial error.
	if connecting.Load() {
		return classifyIncusObservationFailure("connect", err)
	}
	return classifyIncusObservationFailure("request", err)
}

// Only fixed phase and reason values cross the API boundary. In particular,
// neither a URL-bearing transport error nor an upstream error body is exposed.
type incusObservationFailure struct {
	phase  string
	reason string
}

func classifyIncusObservationFailure(phase string, err error) incusObservationFailure {
	if err == nil {
		return incusObservationFailure{}
	}
	var operation *net.OpError
	if errors.As(err, &operation) && operation.Op == "dial" {
		phase = "connect"
	}
	var timeout net.Error
	var reason string
	switch {
	case errors.Is(err, context.Canceled):
		reason = "canceled"
	case phase == "connect" && errors.Is(err, syscall.EAGAIN):
		// Unix socket backlog exhaustion satisfies net.Error.Timeout too.
		reason = "socket_backlog"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout(),
		api.StatusErrorCheck(err, http.StatusRequestTimeout, http.StatusGatewayTimeout):
		reason = "timeout"
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM),
		api.StatusErrorCheck(err, http.StatusUnauthorized, http.StatusForbidden):
		reason = "permission_denied"
	case phase == "connect":
		reason = "socket_unavailable"
	default:
		reason = "request_failed"
	}
	return incusObservationFailure{phase: phase, reason: reason}
}

func (failure incusObservationFailure) write(w http.ResponseWriter, status int, code string) {
	if failure.phase == "admission" {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, struct {
		Error   string `json:"error"`
		Message string `json:"message"`
		Phase   string `json:"phase"`
		Reason  string `json:"reason"`
	}{code, "local Incus observation unavailable", failure.phase, failure.reason})
}
