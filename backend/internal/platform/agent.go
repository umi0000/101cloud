package platform

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	agentclient "101cloud/hostagent/client"
)

func (a *App) CheckEndpoint(endpoint string) error {
	u, e := url.Parse(endpoint)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return E(422, "validation_failed", "node endpoint must be an HTTPS origin")
	}
	host := u.Hostname()
	var addresses []netip.Addr
	if ip, e := netip.ParseAddr(host); e == nil {
		addresses = []netip.Addr{ip.Unmap()}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		addresses, e = net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if e != nil || len(addresses) == 0 {
			return E(422, "validation_failed", "cannot resolve node address")
		}
	}
	for _, ip := range addresses {
		allowed := false
		for _, prefix := range a.Config.NodeAllowCIDRs {
			if prefix.Contains(ip.Unmap()) {
				allowed = true
				break
			}
		}
		if !allowed {
			return E(403, "forbidden", "node address is outside configured management CIDRs")
		}
	}
	return nil
}
func (a *App) AgentConnection(ctx context.Context, nodeID uint64) (agentclient.ConnectionConfig, error) {
	r, e := Get(a.DB.WithContext(ctx), "nodes", nodeID, false)
	if e != nil {
		return agentclient.ConnectionConfig{}, e
	}
	endpoint := strings.TrimRight(Text(r["agent_endpoint"]), "/")
	if e = a.CheckEndpoint(endpoint); e != nil {
		return agentclient.ConnectionConfig{}, e
	}
	token, e := a.Decrypt([]byte(Text(r["token_ciphertext"])))
	if e != nil {
		return agentclient.ConnectionConfig{}, E(503, "node_unavailable", "cannot decrypt node credentials")
	}
	return agentclient.ConnectionConfig{Version: 1, Endpoint: endpoint, Token: string(token), ServerCertSHA256: hex.EncodeToString([]byte(Text(r["server_cert_sha256"])))}, nil
}
func (a *App) AgentClient(ctx context.Context, nodeID uint64) (*http.Client, string, error) {
	cfg, e := a.AgentConnection(ctx, nodeID)
	if e != nil {
		return nil, "", e
	}
	key := cfg.Endpoint + ":" + cfg.ServerCertSHA256 + ":" + hex.EncodeToString(a.sign([]byte(cfg.Token)))
	if cached, ok := a.agentClients.Load(key); ok {
		return cached.(*http.Client), cfg.Endpoint, nil
	}
	client, e := agentclient.NewHTTPClient(cfg)
	if e != nil {
		return nil, "", E(503, "node_unavailable", "invalid pinned node credentials")
	}
	old, loaded := a.agentClients.LoadOrStore(key, client)
	if loaded {
		client.CloseIdleConnections()
		client = old.(*http.Client)
	}
	return client, cfg.Endpoint, nil
}
func (a *App) Agent(ctx context.Context, nodeID uint64, method, path string, body any) (map[string]any, error) {
	v, _, e := a.AgentConditional(ctx, nodeID, method, path, body, "")
	return v, e
}
func (a *App) AgentConditional(ctx context.Context, nodeID uint64, method, path string, body any, etag string) (map[string]any, string, error) {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "\r\n\\") {
		return nil, "", errors.New("invalid node API path")
	}
	u, e := url.ParseRequestURI(path)
	if e != nil || u.IsAbs() {
		return nil, "", errors.New("invalid node API target")
	}
	client, origin, e := a.AgentClient(ctx, nodeID)
	if e != nil {
		return nil, "", e
	}
	var reader io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return nil, "", e
		}
		reader = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, origin+path, reader)
	if e != nil {
		return nil, "", e
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	resp, e := client.Do(req)
	if e != nil {
		return nil, "", E(503, "node_unavailable", "node request failed")
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if e != nil || len(b) > 16<<20 {
		return nil, "", E(503, "node_unavailable", "invalid node response size")
	}
	var out map[string]any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if d.Decode(&out) != nil {
		return nil, "", E(503, "node_unavailable", "invalid node JSON response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code := "node_unavailable"
		status := 503
		if resp.StatusCode == 409 || resp.StatusCode == 412 {
			code = "configuration_conflict"
			status = 409
		}
		return out, resp.Header.Get("ETag"), E(status, code, "node rejected operation")
	}
	return out, resp.Header.Get("ETag"), nil
}
func (a *App) WaitOperation(ctx context.Context, nodeID uint64, operation string) (map[string]any, error) {
	if operation == "" {
		return nil, errors.New("missing operation reference")
	}
	u, e := url.Parse(operation)
	if e != nil || u.IsAbs() || !strings.HasPrefix(u.Path, "/1.0/operations/") || strings.Contains(strings.TrimPrefix(u.Path, "/1.0/operations/"), "/") {
		return nil, errors.New("invalid operation reference")
	}
	q := u.Query()
	q.Set("timeout", "30")
	path := u.Path + "/wait?" + q.Encode()
	for {
		result, e := a.Agent(ctx, nodeID, http.MethodGet, path, nil)
		if e != nil {
			return nil, Unknown(e)
		}
		m, ok := result["metadata"].(map[string]any)
		if !ok {
			return nil, Unknown(errors.New("missing operation state"))
		}
		status := Uint(m["status_code"])
		if status >= 200 {
			if status == 200 {
				return result, nil
			}
			return result, E(409, "operation_failed", "node operation failed")
		}
		select {
		case <-ctx.Done():
			return nil, Unknown(ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}
