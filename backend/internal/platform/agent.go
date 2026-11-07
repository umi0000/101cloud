package platform

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	agentclient "101cloud/hostagent/client"
	"gorm.io/gorm"
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
	dispatch, associated := ctx.Value(taskDispatchKey{}).(taskDispatch)
	mutation := method != http.MethodGet && method != http.MethodHead
	if execution, ok := ctx.Value(taskExecutionKey{}).(*taskExecution); ok && mutation && !associated {
		dispatch = taskDispatch{job: execution.job, key: fmt.Sprintf("native.dispatch.%d", execution.dispatch.Add(1))}
		associated = true
		// Older handlers may omit WithTaskDispatch. Their mutations still need
		// durable intent and the same execution lease as explicitly named steps.
		if err := a.Step(ctx, dispatch.job, dispatch.key, "", "running"); err != nil {
			return nil, "", Unknown(err)
		}
	}
	if associated && mutation {
		var result map[string]any
		var resultETag string
		var requestErr error
		requestCtx, cancelRequest := context.WithCancel(ctx)
		defer cancelRequest()
		submitted, completed := make(chan struct{}), make(chan struct{})
		var submission sync.Once
		finishSubmission := func() { submission.Do(func() { close(submitted) }) }
		requestCtx = httptrace.WithClientTrace(requestCtx, &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { finishSubmission() }})
		started := false
		// Hold the durable fence through actual HTTP submission, not response
		// waiting. Force-stop can then commit before cancelling any execution;
		// a rejected/rolled-back request never interrupts the worker.
		err := a.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var task Row
			if err := tx.Raw("SELECT id,instance_id,task_type,parameters_redacted FROM tasks WHERE id=? AND lease_generation=? AND status='running' AND lease_until>? FOR UPDATE", dispatch.job.ID, dispatch.job.LeaseGeneration, a.Time()).Scan(&task).Error; err != nil {
				return err
			}
			if Uint(task["id"]) != dispatch.job.ID || nodeID != dispatch.job.NodeID {
				return errors.New("lost task lease before native dispatch")
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if instanceID := Uint(task["instance_id"]); instanceID != 0 {
				var locked uint64
				if err := tx.Raw("SELECT id FROM instances WHERE id=? FOR UPDATE", instanceID).Scan(&locked).Error; err != nil {
					return err
				}
				if locked != instanceID {
					return errors.New("task instance identity is missing")
				}
				if !TaskCanResolveMutationBarrier(task) {
					if err := TaskMutationBarrier(tx, instanceID); err != nil {
						return err
					}
				}
			}
			// Independently commit intent before bytes can leave this process.
			intent := a.DB.WithContext(ctx).Table("task_steps").Where("task_id=? AND step_key=? AND attempt=?", dispatch.job.ID, dispatch.key, dispatch.job.LeaseGeneration).Update("result_redacted", gorm.Expr("JSON_SET(COALESCE(result_redacted, JSON_OBJECT()), '$.native_dispatch_started', true)"))
			if intent.Error != nil {
				return intent.Error
			}
			if intent.RowsAffected != 1 {
				return errors.New("native dispatch omitted durable step intent")
			}
			started = true
			go func() {
				defer close(completed)
				defer finishSubmission() // also releases failed/custom transports
				result, resultETag, requestErr = a.agentConditional(requestCtx, nodeID, method, path, body, etag)
			}()
			<-submitted
			return nil
		})
		if err != nil {
			cancelRequest()
			if started {
				<-completed
			}
			return result, resultETag, Unknown(err)
		}
		<-completed
		if operation := Text(result["operation"]); operation != "" {
			ref, err := url.Parse(operation)
			if err != nil || ref.IsAbs() || !strings.HasPrefix(ref.Path, "/1.0/operations/") {
				return result, resultETag, Unknown(errors.New("invalid native operation reference"))
			}
			if target, err := url.ParseRequestURI(path); err == nil && ref.Query().Get("project") == "" {
				q := ref.Query()
				q.Set("project", target.Query().Get("project"))
				ref.RawQuery = q.Encode()
			}
			journalCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer stop()
			if err := a.recordTaskOperation(journalCtx, dispatch, nodeID, ref); err != nil {
				return result, resultETag, Unknown(err)
			}
		}
		return result, resultETag, requestErr
	}
	return a.agentConditional(ctx, nodeID, method, path, body, etag)
}

// WithTaskDispatch selects a durable step for the actual HTTP submission.
// Executions automatically fence unnamed mutations; late operation identities
// append to the original journal without restoring revoked execution authority.
func WithTaskDispatch(ctx context.Context, job Job, key string) context.Context {
	return context.WithValue(ctx, taskDispatchKey{}, taskDispatch{job, key})
}

type taskDispatchKey struct{}
type taskDispatch struct {
	job Job
	key string
}

func (a *App) agentConditional(ctx context.Context, nodeID uint64, method, path string, body any, etag string) (map[string]any, string, error) {
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
		return nil, "", E(503, "node_unavailable", "无法连接节点或等待节点响应超时，请联系管理员检查节点连接；操作已提交时请勿重复执行。")
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if e != nil || len(b) > 16<<20 {
		return nil, "", E(503, "node_unavailable", "节点响应不完整或超出大小限制，请联系管理员检查节点状态。")
	}
	var out map[string]any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if d.Decode(&out) != nil {
		return nil, "", E(503, "node_unavailable", "节点返回了无法识别的响应，请联系管理员检查节点状态。")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusNotFound && Text(out["type"]) == "error" && Uint(out["error_code"]) == http.StatusNotFound {
			return out, resp.Header.Get("ETag"), E(404, "native_not_found", "Native resource is absent")
		}
		code := "node_unavailable"
		status := 503
		if resp.StatusCode == 409 || resp.StatusCode == 412 {
			code = "configuration_conflict"
			status = 409
		}
		return out, resp.Header.Get("ETag"), E(status, code, "节点拒绝了本次操作，请刷新实例状态并联系管理员检查节点配置。")
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
			return result, E(409, "operation_failed", "节点执行操作失败，请联系管理员检查节点任务及实例状态后再决定下一步。")
		}
		select {
		case <-ctx.Done():
			return nil, Unknown(ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}
