// Package node implements trusted-backend-only node extensions over the local
// Incus Unix socket. It performs no authentication; its HTTP handler must be
// mounted behind the host agent's backend authentication and authorization.
//
// GET /agent/v1/resources returns complete server_resources, every GPU in gpus,
// and average_load_percent (null when any GPU load is unknown or no GPUs exist).
// network_interfaces, system_dns and network_errors describe local address,
// default-route and DNS configuration without inferring an allocatable IP pool.
// GET /agent/v1/health returns {"status":"ok","incus":true,"server_name":"..."}
// after a live Incus GetServer call, or HTTP 503 when Incus is unavailable.
// POST /agent/v1/network/ip-probe accepts one interface/address IPv4 probe.
// Observations do not reserve the address or guarantee DHCP safety.
// PUT /agent/v1/instances/{name}/gpu?project=default accepts
// {"gpu_uuids": ["GPU-..."], "allow_live": false}. An empty UUID array removes
// only local agent-gpu-N devices. Other GPU devices and inherited GPU profiles
// are conflicts, never implicitly removed. Only node-advertised NVIDIA UUIDs
// are supported, not PCI selectors, MIG identifiers or GPU resource quotas.
//
// GPU mutation responses include requested_gpu_uuids, operation_completed,
// configuration_state (observed or unknown), actual (the observed api.Instance),
// etag, resource_release (always not_verified) and warnings. Failed or uncertain
// operations also include error/message and, if a fresh read fails,
// observation_error. Observations are point-in-time configuration, not evidence
// that old device handles, memory or compute contexts have been reclaimed.
package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	incus "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
)

const (
	defaultSocket  = "/var/lib/incus/unix.socket"
	maxRequestBody = 64 << 10
	readTimeout    = 10 * time.Second
	updateTimeout  = 2 * time.Minute
)

// Service exposes node-local Incus resources, GPU configuration and IPv4 conflict probing.
// The caller must authenticate and authorize the trusted backend before invoking it.
type Service struct {
	socketPath       string
	gpuCollector     func(context.Context, *api.Resources) ([]GPUInfo, *float64)
	networkCollector func(context.Context) NetworkInfo
	ipProber         func(context.Context, IPProbeRequest) (IPProbeResult, error)
	ipProbeSlots     chan struct{}
	metricsMu        sync.RWMutex
	metrics          *metricsSampler
	imageCatalog     *remoteImageCatalog
	mu               sync.Mutex
	locks            map[instanceKey]*instanceLock
}

type instanceKey struct {
	project string
	name    string
}

type instanceLock struct {
	token chan struct{}
	users int
}

// New prepares an Incus Unix-socket client. It does not require the daemon to be
// online at startup; /agent/v1/health performs a live connectivity check.
func New(socketPath string) (*Service, error) {
	if socketPath == "" {
		socketPath = defaultSocket
	}
	if !filepath.IsAbs(socketPath) {
		return nil, fmt.Errorf("Incus socket path must be absolute")
	}
	return &Service{
		socketPath:       socketPath,
		gpuCollector:     collectGPUs,
		networkCollector: collectNetwork,
		ipProber:         probeIPv4,
		ipProbeSlots:     make(chan struct{}, maxConcurrentIPProbes),
		imageCatalog:     newRemoteImageCatalog(),
		locks:            make(map[instanceKey]*instanceLock),
	}, nil
}

func (s *Service) connect(ctx context.Context, project string) (incus.InstanceServer, error) {
	// WaitContext in SDK v6.0.5 mutates Transport.ResponseHeaderTimeout.
	// Give every request a separate transport, including observation reads.
	client, err := incus.ConnectIncusUnixWithContext(ctx, s.socketPath, &incus.ConnectionArgs{
		SkipGetServer: true,
		SkipGetEvents: true,
		UserAgent:     "101cloud-hostagent",
	})
	if err != nil {
		return nil, err
	}
	// The connection already carries ctx; UseProject clones that connection.
	return client.UseProject(project), nil
}

func closeIncus(client incus.InstanceServer) {
	if httpClient, err := client.GetHTTPClient(); err == nil {
		httpClient.CloseIdleConnections()
	}
	client.Disconnect()
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/agent/v1/image-sources" || strings.HasPrefix(r.URL.Path, "/agent/v1/image-sources/") {
		s.imageSourcesAPI(w, r)
		return
	}
	switch r.URL.Path {
	case "/agent/v1/instances/metrics":
		s.metricsAPI(w, r, false)
		return
	case "/agent/v1/instances/metrics/samples":
		s.metricsAPI(w, r, true)
		return
	case "/agent/v1/health":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		s.health(w, r)
		return
	case "/agent/v1/resources":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		s.resources(w, r)
		return
	case "/agent/v1/network/ip-probe":
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		s.probeIP(w, r)
		return
	}
	const prefix = "/agent/v1/instances/"
	if strings.HasPrefix(r.URL.Path, prefix) && strings.HasSuffix(r.URL.Path, "/gpu") {
		if !requireMethod(w, r, http.MethodPut) {
			return
		}
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), "/gpu")
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
			writeError(w, http.StatusBadRequest, "invalid_instance", "a single instance name is required")
			return
		}
		s.configureGPU(w, r, name)
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "unknown node endpoint")
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method must be "+method)
	return false
}

func (s *Service) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	client, err := s.connect(ctx, "default")
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "incus_unavailable", err.Error())
		return
	}
	defer closeIncus(client)
	server, _, err := client.GetServer()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "incus_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Status     string `json:"status"`
		Incus      bool   `json:"incus"`
		ServerName string `json:"server_name"`
	}{"ok", true, server.Environment.ServerName})
}

func (s *Service) resources(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	client, err := s.connect(ctx, "default")
	if err != nil {
		writeError(w, http.StatusBadGateway, "incus_resources_failed", err.Error())
		return
	}
	defer closeIncus(client)
	resources, err := client.GetServerResources()
	if err != nil {
		writeError(w, http.StatusBadGateway, "incus_resources_failed", err.Error())
		return
	}
	var gpus []GPUInfo
	var averageLoad *float64
	var gpuDone sync.WaitGroup
	gpuDone.Add(1)
	go func() {
		defer gpuDone.Done()
		gpus, averageLoad = s.gpuCollector(ctx, resources)
	}()
	networkCtx, networkCancel := context.WithTimeout(ctx, 5*time.Second)
	network := s.networkCollector(networkCtx)
	networkCancel()
	gpuDone.Wait()
	writeJSON(w, http.StatusOK, struct {
		ServerResources    *api.Resources     `json:"server_resources"`
		GPUs               []GPUInfo          `json:"gpus"`
		AverageLoadPercent *float64           `json:"average_load_percent"`
		NetworkInterfaces  []NetworkInterface `json:"network_interfaces"`
		SystemDNS          SystemDNS          `json:"system_dns"`
		NetworkErrors      []string           `json:"network_errors"`
	}{resources, gpus, averageLoad, network.Interfaces, network.SystemDNS, network.Errors})
}

func requestProject(r *http.Request) (string, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", fmt.Errorf("invalid query: %w", err)
	}
	for key := range values {
		if key != "project" {
			return "", fmt.Errorf("unknown query parameter %q", key)
		}
	}
	projects, ok := values["project"]
	if !ok {
		return "default", nil
	}
	if len(projects) != 1 || projects[0] == "" || strings.TrimSpace(projects[0]) != projects[0] || strings.ContainsAny(projects[0], "/\\\x00") {
		return "", errors.New("project must be one non-empty project name")
	}
	return projects[0], nil
}

func decodeGPURequest(w http.ResponseWriter, r *http.Request) (gpuRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	// Decode the object manually to reject duplicate keys as well as unknown
	// keys. encoding/json otherwise silently accepts the last duplicate value.
	decoder := json.NewDecoder(r.Body)
	token, err := decoder.Token()
	if err != nil {
		return gpuRequest{}, err
	}
	if token != json.Delim('{') {
		return gpuRequest{}, errors.New("request body must be a JSON object")
	}
	var request gpuRequest
	seen := make(map[string]bool)
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return gpuRequest{}, err
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return gpuRequest{}, fmt.Errorf("duplicate or invalid JSON field %q", key)
		}
		seen[key] = true
		switch key {
		case "gpu_uuids":
			if err := decoder.Decode(&request.GPUUUIDs); err != nil {
				return gpuRequest{}, err
			}
			if request.GPUUUIDs == nil {
				return gpuRequest{}, errors.New("gpu_uuids must be an array, not null")
			}
		case "allow_live":
			var value *bool
			if err := decoder.Decode(&value); err != nil {
				return gpuRequest{}, err
			}
			if value == nil {
				return gpuRequest{}, errors.New("allow_live must be a boolean")
			}
			request.AllowLive = *value
		default:
			return gpuRequest{}, fmt.Errorf("unknown JSON field %q", key)
		}
	}
	if _, err := decoder.Token(); err != nil {
		return gpuRequest{}, err
	}
	if !seen["gpu_uuids"] {
		return gpuRequest{}, errors.New("gpu_uuids is required (use [] to remove managed configuration)")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return gpuRequest{}, errors.New("only one JSON object is permitted")
		}
		return gpuRequest{}, err
	}
	return request, nil
}

func (s *Service) acquire(ctx context.Context, key instanceKey) (func(), error) {
	s.mu.Lock()
	lock := s.locks[key]
	if lock == nil {
		lock = &instanceLock{token: make(chan struct{}, 1)}
		lock.token <- struct{}{}
		s.locks[key] = lock
	}
	lock.users++
	s.mu.Unlock()
	drop := func() {
		s.mu.Lock()
		lock.users--
		if lock.users == 0 {
			delete(s.locks, key)
		}
		s.mu.Unlock()
	}
	select {
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	case <-lock.token:
		return func() {
			lock.token <- struct{}{}
			drop()
		}, nil
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}{code, message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
