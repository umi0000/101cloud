package gpu

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	ErrUnsupported = errors.New("native GPU monitoring unsupported on this platform")
	ErrUnavailable = errors.New("NVML library or required ABI unavailable")
	ErrBusy        = errors.New("GPU native collection still in progress")
	ErrClosed      = errors.New("GPU reader closed")
)

// Serialize all native work, including initialization and shutdown, across all
// Readers. A timed-out driver call retains its slot until it actually returns.
var nativeSlot = make(chan struct{}, 1)

type backend interface {
	collect(uint64) (Sample, error)
	close() error
}

// Reader bounds blocked native work independently of request lifetimes. It must
// not be copied. NewReader and its zero value do not load or initialize NVML.
type Reader struct {
	mu      sync.Mutex
	closed  bool
	running bool
	backend backend
	open    func() (backend, error)
}

func NewReader() *Reader { return &Reader{} }

type queryResult struct {
	sample Sample
	err    error
}

// Query enumerates every physical GPU, not only configured attachments. The
// cursor is an NVML timestamp from a previous successful utilization sample (or
// zero for the driver's retained history). Partial device failures are encoded
// in Device.Error; a top-level error invalidates host inventory completeness.
// Cancellation abandons the result, not the native call. Concurrent or later
// requests return ErrBusy rather than queueing more native calls/goroutines.
func (r *Reader) Query(ctx context.Context, sinceMicros uint64) (Sample, error) {
	if err := ctx.Err(); err != nil {
		return Sample{CollectedAt: time.Now()}, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return Sample{CollectedAt: time.Now()}, ErrClosed
	}
	if r.running {
		r.mu.Unlock()
		return Sample{CollectedAt: time.Now()}, ErrBusy
	}
	select {
	case nativeSlot <- struct{}{}:
	default:
		r.mu.Unlock()
		return Sample{CollectedAt: time.Now()}, ErrBusy
	}
	r.running = true
	r.mu.Unlock()
	result := make(chan queryResult, 1)
	go r.query(sinceMicros, result)
	select {
	case out := <-result:
		return out.sample, out.err
	case <-ctx.Done():
		return Sample{CollectedAt: time.Now()}, ctx.Err()
	}
}

func (r *Reader) query(since uint64, result chan<- queryResult) {
	if r.backend == nil {
		open := r.open
		if open == nil {
			open = openBackend
		}
		var err error
		r.backend, err = open()
		if err != nil {
			r.finish()
			result <- queryResult{sample: Sample{CollectedAt: time.Now()}, err: err}
			return
		}
	}
	sample, err := r.backend.collect(since)
	r.finish()
	result <- queryResult{sample: sample, err: err}
}

// finish holds the native slot through any deferred shutdown. Never hold mu
// across a driver call: even nvmlShutdown can block.
func (r *Reader) finish() {
	r.mu.Lock()
	if r.closed && r.backend != nil {
		b := r.backend
		r.backend = nil
		r.mu.Unlock()
		_ = b.close()
		r.mu.Lock()
	}
	r.running = false
	<-nativeSlot
	r.mu.Unlock()
}

// Close is idempotent and nonblocking. An active call owns its library until it
// returns, then shuts it down. If a driver never returns, its one native slot
// and library intentionally remain alive instead of being unsafely unloaded.
func (r *Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if r.running || r.backend == nil {
		return nil
	}
	r.running = true
	go func() {
		nativeSlot <- struct{}{}
		r.finish()
	}()
	return nil
}
