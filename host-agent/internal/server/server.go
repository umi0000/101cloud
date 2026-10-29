package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"101cloud/hostagent/internal/config"
	"101cloud/hostagent/internal/gateway"
	"101cloud/hostagent/internal/node"
)

// Run serves token-authenticated HTTPS. Cancellation closes streams,
// including hijacked WebSockets, without stopping the managed instances.
func Run(ctx context.Context, c config.Config, log *slog.Logger) error {
	tlsConfig, err := gateway.TLSConfig(c.TLS.Cert, c.TLS.Key)
	if err != nil {
		return err
	}
	token, err := gateway.LoadToken(c.TokenFile)
	if err != nil {
		return err
	}
	proxy, err := gateway.NewProxy(c.IncusSocket)
	if err != nil {
		return err
	}
	nodes, err := node.New(c.IncusSocket)
	if err != nil {
		return err
	}
	// Dispatch without ServeMux's path-cleaning redirects: the gateway rejects
	// ambiguous paths rather than silently changing a privileged request.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent/v1" || len(r.URL.Path) >= len("/agent/v1/") && r.URL.Path[:len("/agent/v1/")] == "/agent/v1/" {
			nodes.ServeHTTP(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	})
	srv := &http.Server{
		Handler: audit(gateway.TokenAuth(token, handler), log), TLSConfig: tlsConfig,
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second,
		MaxHeaderBytes: 64 * 1024,
		// No whole-request read/write timeout: exec, events and backups stream.
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	base, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	defer base.Close()
	settings, err := c.MetricsSettings()
	if err != nil {
		return err
	}
	metricsCtx, metricsCancel := context.WithCancel(ctx)
	defer metricsCancel()
	if err := nodes.StartMetrics(metricsCtx, time.Duration(settings.IntervalSeconds)*time.Second, time.Duration(settings.TimeoutSeconds)*time.Second); err != nil {
		return err
	}
	listener := &trackedListener{Listener: base, conns: make(map[*trackedConn]struct{})}
	defer listener.closeConnections()
	done := make(chan error, 1)
	go func() { done <- srv.ServeTLS(listener, "", "") }()
	log.Info("host agent listening", "address", base.Addr().String())
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		stop, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(stop); err != nil {
			_ = srv.Close()
		}
		listener.closeConnections()
		err := <-done
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func audit(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Defense in depth if this handler is ever reused without ServeTLS.
		if r.TLS == nil {
			http.Error(w, "HTTPS required", http.StatusUnauthorized)
			return
		}
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			http.Error(w, "request ID unavailable", 500)
			return
		}
		requestID := hex.EncodeToString(id[:])
		w.Header().Set("X-Request-ID", requestID)
		r.Header.Set("X-Request-ID", requestID)
		identity := r.RemoteAddr
		started := time.Now()
		recorder := &responseWriter{ResponseWriter: w}
		defer func() {
			status := recorder.status
			if status == 0 {
				status = http.StatusOK
			}
			// Never log queries: Incus WebSocket queries contain access secrets.
			log.Info("request", "request_id", requestID, "client", identity,
				"method", r.Method, "path", r.URL.EscapedPath(), "status", status,
				"duration", time.Since(started))
		}()
		next.ServeHTTP(recorder, r)
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *responseWriter) WriteHeader(status int) {
	if w.status == 0 {
		if status >= 200 || status == 101 {
			w.status = status
		}
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *responseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(p)
}
func (w *responseWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, b, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.status = http.StatusSwitchingProtocols
	}
	return c, b, err
}

type trackedListener struct {
	net.Listener
	mu    sync.Mutex
	conns map[*trackedConn]struct{}
}
type trackedConn struct {
	net.Conn
	owner *trackedListener
}

func (l *trackedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	t := &trackedConn{Conn: c, owner: l}
	l.mu.Lock()
	l.conns[t] = struct{}{}
	l.mu.Unlock()
	return t, nil
}
func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.owner.mu.Lock()
	delete(c.owner.conns, c)
	c.owner.mu.Unlock()
	return err
}
func (l *trackedListener) closeConnections() {
	l.mu.Lock()
	conns := make([]*trackedConn, 0, len(l.conns))
	for c := range l.conns {
		conns = append(conns, c)
	}
	l.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}
