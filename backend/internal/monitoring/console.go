package monitoring

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"101cloud/backend/internal/auth"
	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const consoleFrameLimit = 64 << 10

// consoleSession deliberately never serializes upstream credentials.
type consoleSession struct {
	ID                string
	UserID            uint64
	AuthSessionID     uint64
	InstanceID        uint64
	NodeID            uint64
	InstanceOwnerID   uint64
	IsAdmin           bool
	Kind              string
	Status            string
	Cols              int
	Rows              int
	OperationPath     string
	SecretsCiphertext []byte
	PrepareTaskID     *uint64
	PrepareStartedAt  *time.Time
	ConnectedAt       *time.Time
	ClosedAt          *time.Time
	Revision          uint64
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type consoleFrame struct {
	data []byte
	err  error
}
type consoleStop struct {
	code    int
	message string
}
type consoleResult struct {
	operation map[string]any
	err       error
}
type consoleBridge struct {
	data    *consoleWire
	control *consoleWire
	frames  chan consoleFrame
	result  chan consoleResult
	done    chan struct{}
	once    sync.Once
	stop    consoleStop
	cancel  context.CancelFunc
}

func (b *consoleBridge) close(code int, message string) {
	b.once.Do(func() {
		b.stop = consoleStop{code, message}
		close(b.done)
		b.cancel()
		// Incus terminates an exec command on control-channel disconnection.
		_ = b.control.Close()
		_ = b.data.Close()
	})
}

type consoleService struct {
	a       *platform.App
	mu      sync.Mutex
	bridges map[string]*consoleBridge
}

// RegisterConsole owns the durable preparation task and the browser-facing bridge.
// Prepared sockets must remain on their backend process; multi-replica deployments
// must route a session's stream to the process that prepared it.
func RegisterConsole(a *platform.App, user, admin *gin.RouterGroup) {
	s := &consoleService{a: a, bridges: make(map[string]*consoleBridge)}
	for _, group := range []*gin.RouterGroup{user, admin} {
		group.POST("/instances/:id/console-sessions", platform.Handle(s.create))
		group.GET("/console-sessions/:session_id", platform.Handle(s.status))
		group.DELETE("/console-sessions/:session_id", platform.Handle(s.remove))
		group.GET("/console-sessions/:session_id/stream", s.stream)
	}
	a.RegisterJob("monitoring.console.prepare", s.prepare)
	a.AddPoller("monitoring.console.revoke", 2*time.Second, s.revoke)
	a.AddShutdownHook(s.shutdown)
}

func (s *consoleService) create(c *gin.Context) (any, error) {
	var req struct {
		Kind string `json:"kind"`
		Cols int    `json:"cols"`
		Rows int    `json:"rows"`
	}
	if err := platform.Decode(c, &req); err != nil {
		return nil, err
	}
	admin := c.GetBool("admin")
	if req.Kind != "terminal" && req.Kind != "serial" && req.Kind != "vga" {
		return nil, platform.E(400, "invalid_kind", "无效控制台类型")
	}
	if req.Kind == "terminal" {
		if req.Cols < 1 || req.Cols > 1000 || req.Rows < 1 || req.Rows > 1000 {
			return nil, platform.E(400, "invalid_size", "cols和rows必须为1–1000")
		}
	} else if req.Cols != 0 || req.Rows != 0 {
		return nil, platform.E(400, "invalid_size", "仅terminal接受cols和rows")
	}
	if req.Kind != "terminal" {
		req.Cols = 80
		req.Rows = 24
	}
	id, err := platform.ID(c, "id")
	if err != nil {
		return nil, err
	}
	u := platform.Current(c)
	db := platform.DB(c, s.a)
	// Serialize quota admission across all instances and sessions belonging to this actor.
	if _, err = platform.Get(db, "users", u.ID, true); err != nil {
		return nil, err
	}
	inst, err := platform.Get(db, "instances", id, true)
	if err != nil {
		return nil, err
	}
	if inst["creation_failed_at"] != nil {
		return nil, platform.E(404, "not_found", "实例不存在")
	}
	if !admin && platform.Uint(inst["user_id"]) != u.ID {
		return nil, platform.E(404, "not_found", "实例不存在")
	}
	if !admin && !consoleAllowed(inst) {
		return nil, platform.E(403, "action_not_allowed", "管理员未授权此实例的控制台操作")
	}
	if platform.Text(inst["observed_state"]) != "running" || inst["deleted_at"] != nil || platform.Text(inst["provisioning_state"]) != "ready" {
		return nil, platform.E(409, "instance_unavailable", "实例未运行或不可运维")
	}
	if (req.Kind == "terminal") != (platform.Text(inst["image_type"]) == "container") {
		return nil, platform.E(409, "console_unsupported", "此实例不支持该控制台类型")
	}
	var count int64
	if err = db.Table("console_sessions").Where("user_id = ? AND status IN ?", u.ID, []string{"preparing", "ready", "active"}).Count(&count).Error; err != nil {
		return nil, platform.WrapDB(err)
	}
	if count >= int64(s.a.Config.ConsoleMaxConcurrent) {
		return nil, platform.E(429, "console_limit", "控制台会话已达到并发限制")
	}
	ticket := make([]byte, 32)
	if _, err = rand.Read(ticket); err != nil {
		return nil, err
	}
	now := s.a.Time()
	row := consoleSession{ID: base64.RawURLEncoding.EncodeToString(ticket), UserID: u.ID, AuthSessionID: u.SessionID, InstanceID: id, NodeID: platform.Uint(inst["node_id"]), InstanceOwnerID: platform.Uint(inst["user_id"]), IsAdmin: admin, Kind: req.Kind, Status: "preparing", Cols: req.Cols, Rows: req.Rows, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err = db.Table("console_sessions").Create(&row).Error; err != nil {
		return nil, platform.WrapDB(err)
	}
	task, err := platform.Enqueue(db, "monitoring.console.prepare", row.InstanceOwnerID, row.NodeID, id, false, map[string]any{"session_id": row.ID})
	if err != nil {
		return nil, err
	}
	taskID := platform.Uint(task["task_id"])
	if err = db.Table("console_sessions").Where("id = ?", row.ID).Update("prepare_task_id", taskID).Error; err != nil {
		return nil, platform.WrapDB(err)
	}
	row.PrepareTaskID = &taskID
	if admin {
		if err = platform.Audit(c.Request.Context(), db, u.ID, taskID, id, "console.create", map[string]any{"session_id": row.ID, "kind": req.Kind}); err != nil {
			return nil, err
		}
	}
	c.Header("ETag", fmt.Sprintf("\"rev-%d\"", row.Revision))
	platform.Reply(c, 201, s.public(c, row))
	return nil, nil
}

func (s *consoleService) public(c *gin.Context, row consoleSession) any {
	prefix := "/api/v1"
	if c.GetBool("admin") {
		prefix += "/admin"
	}
	protocol := "101cloud.terminal.v1"
	if row.Kind == "vga" {
		protocol = "101cloud.vnc.v1"
	}
	return map[string]any{"session_id": row.ID, "status": row.Status, "kind": row.Kind, "websocket_path": prefix + "/console-sessions/" + row.ID + "/stream", "protocol": protocol, "revision": strconv.FormatUint(row.Revision, 10)}
}

func consoleTicket(v string) bool {
	b, err := base64.RawURLEncoding.Strict().DecodeString(v)
	return err == nil && len(b) == 32 && len(v) == 43
}

func (s *consoleService) session(c *gin.Context, lock bool) (consoleSession, error) {
	var row consoleSession
	id := c.Param("session_id")
	if !consoleTicket(id) {
		return row, platform.E(404, "not_found", "控制台会话不存在")
	}
	db := platform.DB(c, s.a).Table("console_sessions")
	if lock {
		db = db.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	err := db.Where("id = ?", id).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return row, platform.E(404, "not_found", "控制台会话不存在")
	}
	if err != nil {
		return row, platform.WrapDB(err)
	}
	u := platform.Current(c)
	if row.UserID != u.ID || row.AuthSessionID != u.SessionID {
		return row, platform.E(404, "not_found", "控制台会话不存在")
	}
	return row, nil
}

func (s *consoleService) authorized(ctx context.Context, row consoleSession, live bool) error {
	if err := auth.ValidateSession(ctx, s.a, row.AuthSessionID, row.UserID); err != nil {
		return err
	}
	var inst platform.Row
	if err := s.a.DB.WithContext(ctx).Table("instances").Select("user_id,node_id,observed_state,provisioning_state,deleted_at,creation_failed_at,product_type,allowed_user_actions").Where("id = ?", row.InstanceID).Take(&inst).Error; err != nil {
		return err
	}
	if inst["creation_failed_at"] != nil {
		return platform.E(404, "not_found", "实例不存在")
	}
	if platform.Uint(inst["user_id"]) != row.InstanceOwnerID || platform.Uint(inst["node_id"]) != row.NodeID || (!row.IsAdmin && platform.Uint(inst["user_id"]) != row.UserID) {
		return platform.E(403, "console_revoked", "实例权限已改变")
	}
	if row.IsAdmin {
		var actor struct{ Role string }
		if err := s.a.DB.WithContext(ctx).Table("users").Select("role").Where("id=?", row.UserID).Take(&actor).Error; err != nil {
			return err
		}
		if actor.Role != "admin" {
			return platform.E(403, "console_revoked", "管理员权限已撤销")
		}
	} else if !consoleAllowed(inst) {
		return platform.E(403, "console_revoked", "实例控制台授权已撤销")
	}
	if live && (inst["deleted_at"] != nil || platform.Text(inst["observed_state"]) != "running" || platform.Text(inst["provisioning_state"]) != "ready") {
		return platform.E(403, "console_revoked", "实例状态已改变")
	}
	return nil
}

func (s *consoleService) status(c *gin.Context) (any, error) {
	row, err := s.session(c, false)
	if err != nil {
		return nil, err
	}
	if err = s.authorized(c.Request.Context(), row, false); err != nil {
		return nil, err
	}
	c.Header("ETag", fmt.Sprintf("\"rev-%d\"", row.Revision))
	c.Header("Cache-Control", "no-store")
	return s.public(c, row), nil
}

func (s *consoleService) remove(c *gin.Context) (any, error) {
	row, err := s.session(c, true)
	if err != nil {
		return nil, err
	}
	if err = s.authorized(c.Request.Context(), row, false); err != nil {
		return nil, err
	}
	if err = platform.Revision(c, platform.Row{"revision": row.Revision}); err != nil {
		return nil, err
	}
	if err = platform.DB(c, s.a).Table("console_sessions").Where("id = ? AND revision = ?", row.ID, row.Revision).Updates(map[string]any{"status": "closed", "closed_at": s.a.Time(), "revision": gorm.Expr("revision + 1")}).Error; err != nil {
		return nil, platform.WrapDB(err)
	}
	// The committed row, not an uncommitted mutation, drives upstream revocation.
	c.Status(204)
	return nil, nil
}

func consoleOperation(v map[string]any) map[string]any {
	if nested, ok := v["metadata"].(map[string]any); ok {
		if _, exists := nested["status_code"]; exists {
			return nested
		}
	}
	return v
}

func consoleOperationPath(v string) (string, error) {
	u, err := url.Parse(v)
	if err != nil || u.IsAbs() || u.Host != "" || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || !strings.HasPrefix(u.Path, "/1.0/operations/") {
		return "", errors.New("invalid upstream operation")
	}
	id := strings.TrimPrefix(u.Path, "/1.0/operations/")
	if len(id) != 36 {
		return "", errors.New("invalid upstream operation")
	}
	for i, ch := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if ch != '-' {
				return "", errors.New("invalid upstream operation")
			}
		} else if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return "", errors.New("invalid upstream operation")
		}
	}
	return u.Path, nil
}

func (s *consoleService) prepare(ctx context.Context, a *platform.App, job platform.Job) error {
	var args struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(job.Parameters, &args); err != nil {
		return err
	}
	var row consoleSession
	if err := a.DB.WithContext(ctx).Table("console_sessions").Where("id = ?", args.SessionID).Take(&row).Error; err != nil {
		return err
	}
	if row.Status != "preparing" {
		return nil
	}
	if err := s.authorized(ctx, row, true); err != nil {
		s.finish(row.ID, "closed")
		return err
	}
	now := a.Time()
	result := a.DB.WithContext(ctx).Table("console_sessions").Where("id = ? AND status = 'preparing' AND prepare_started_at IS NULL", row.ID).Updates(map[string]any{"prepare_started_at": now, "revision": gorm.Expr("revision + 1")})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		s.finish(row.ID, "failed")
		return platform.Unknown(errors.New("console preparation was interrupted; upstream operation must not be replayed"))
	}
	fail := func(err error) error { s.finish(row.ID, "failed"); return err }
	if err := a.Step(ctx, job, "prepare_console", "", "running"); err != nil {
		return fail(err)
	}
	inst, err := platform.Get(a.DB.WithContext(ctx), "instances", row.InstanceID, false)
	if err != nil {
		return fail(err)
	}
	if inst["creation_failed_at"] != nil {
		return fail(platform.E(404, "not_found", "实例不存在"))
	}
	path := "/1.0/instances/" + url.PathEscape(platform.Text(inst["incus_name"]))
	body := map[string]any{}
	if row.Kind == "terminal" {
		path += "/exec"
		body = map[string]any{"command": []string{"/bin/sh", "-l"}, "interactive": true, "wait-for-websocket": true, "environment": map[string]string{"TERM": "xterm-256color"}, "width": row.Cols, "height": row.Rows}
	} else {
		path += "/console"
		kind := "console"
		if row.Kind == "vga" {
			kind = "vga"
		}
		body = map[string]any{"type": kind}
	}
	project := platform.Text(inst["project_name"])
	path += "?project=" + url.QueryEscape(project)
	callCtx, cancelCall := context.WithTimeout(ctx, 15*time.Second)
	response, err := a.Agent(callCtx, row.NodeID, http.MethodPost, path, body)
	cancelCall()
	if err != nil {
		return fail(platform.Unknown(errors.New("upstream console creation outcome is unknown")))
	}
	opPath, err := consoleOperationPath(platform.Text(response["operation"]))
	if err != nil {
		return fail(platform.Unknown(err))
	}
	op := consoleOperation(response)
	metadata, _ := op["metadata"].(map[string]any)
	fds, _ := metadata["fds"].(map[string]any)
	dataSecret, controlSecret := platform.Text(fds["0"]), platform.Text(fds["control"])
	if len(dataSecret) == 0 || len(dataSecret) > 512 || len(controlSecret) == 0 || len(controlSecret) > 512 {
		return fail(platform.Unknown(errors.New("upstream console response lacks required channels")))
	}
	secrets, err := json.Marshal(map[string]string{"0": dataSecret, "control": controlSecret})
	if err != nil {
		return fail(err)
	}
	encrypted, err := a.Encrypt(secrets)
	if err != nil {
		return fail(err)
	}
	if err = a.DB.WithContext(ctx).Table("console_sessions").Where("id = ?", row.ID).Updates(map[string]any{"operation_path": opPath, "secrets_ciphertext": encrypted, "revision": gorm.Expr("revision + 1")}).Error; err != nil {
		return fail(platform.Unknown(err))
	}
	// The task lease ends on success; live channels have no fixed lifetime.
	// Shutdown closes only bridges owned by this process.
	liveCtx, cancelLive := context.WithCancel(context.Background())
	control, err := s.connect(liveCtx, row.NodeID, opPath, project, controlSecret)
	if err != nil {
		cancelLive()
		return fail(err)
	}
	data, err := s.connect(liveCtx, row.NodeID, opPath, project, dataSecret)
	if err != nil {
		_ = control.Close()
		cancelLive()
		return fail(err)
	}
	bridge := &consoleBridge{data: data, control: control, frames: make(chan consoleFrame, 4), result: make(chan consoleResult, 1), done: make(chan struct{}), cancel: cancelLive}
	// Observe completion while the operation is live, not after browser output
	// has drained: Incus removes completed operations after a short retention.
	go func() {
		op, err := a.WaitOperation(liveCtx, row.NodeID, opPath+"?project="+url.QueryEscape(project))
		bridge.result <- consoleResult{operation: op, err: err}
	}()
	s.mu.Lock()
	s.bridges[row.ID] = bridge
	s.mu.Unlock()
	go s.readData(bridge)
	go func() {
		for {
			if _, _, err := control.read(); err != nil {
				return
			}
		}
	}()
	result = a.DB.WithContext(ctx).Table("console_sessions").Where("id = ? AND status = 'preparing'", row.ID).Updates(map[string]any{"status": "ready", "revision": gorm.Expr("revision + 1")})
	if result.Error != nil || result.RowsAffected != 1 {
		bridge.close(1008, "会话已撤销")
		s.forget(row.ID, bridge)
		if result.Error != nil {
			return fail(result.Error)
		}
		return nil
	}
	row.OperationPath = opPath
	go s.guard(liveCtx, row, bridge)
	if err = a.Step(ctx, job, "prepare_console", opPath, "succeeded"); err != nil {
		bridge.close(1011, "控制台准备失败")
		return fail(err)
	}
	if err = a.TaskResult(ctx, job, map[string]any{"session_id": row.ID, "status": "ready"}); err != nil {
		bridge.close(1011, "控制台准备失败")
		return fail(err)
	}
	return nil
}

func (s *consoleService) finish(id, status string) {
	_ = s.a.DB.Table("console_sessions").Where("id = ? AND status IN ?", id, []string{"preparing", "ready", "active"}).Updates(map[string]any{"status": status, "closed_at": s.a.Time(), "secrets_ciphertext": nil, "revision": gorm.Expr("revision + 1")}).Error
}
func (s *consoleService) forget(id string, b *consoleBridge) {
	s.mu.Lock()
	if s.bridges[id] == b {
		delete(s.bridges, id)
	}
	s.mu.Unlock()
}
func (s *consoleService) readData(b *consoleBridge) {
	defer close(b.frames)
	for {
		kind, data, err := b.data.read()
		if err == nil && kind == websocket.TextMessage && len(data) == 0 {
			// Incus sends an empty text barrier when stdout ends. Its exec
			// finisher waits for stdin's barrier before publishing metadata.return;
			// waiting for the operation without acknowledging this deadlocks.
			err = b.data.write(websocket.TextMessage, nil)
			if err == nil {
				err = io.EOF
			}
		}
		if err != nil {
			select {
			case b.frames <- consoleFrame{err: err}:
			case <-b.done:
			}
			return
		}
		if kind != websocket.BinaryMessage {
			select {
			case b.frames <- consoleFrame{err: errors.New("unexpected upstream data frame")}:
			case <-b.done:
			}
			return
		}
		select {
		case b.frames <- consoleFrame{data: data}:
		case <-b.done:
			return
		}
	}
}
func (s *consoleService) guard(ctx context.Context, row consoleSession, b *consoleBridge) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	defer s.forget(row.ID, b)
	for {
		select {
		case <-ctx.Done():
			b.close(1000, "会话已结束")
			s.finish(row.ID, "closed")
			return
		case <-b.done:
			s.finish(row.ID, "closed")
			return
		case <-ticker.C:
			checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			var current consoleSession
			err := s.a.DB.WithContext(checkCtx).Table("console_sessions").Where("id = ?", row.ID).Take(&current).Error
			if err == nil && current.Status != "ready" && current.Status != "active" {
				err = errors.New("console revoked")
			}
			if err == nil {
				err = s.authorized(checkCtx, current, true)
			}
			cancel()
			if err != nil {
				b.close(1008, "会话权限已撤销")
				s.finish(row.ID, "closed")
				return
			}
		}
	}
}

// Revoke abandoned rows only when their bound login is no longer authorized.
// Process-local absence alone is not evidence that another replica's bridge died.
func (s *consoleService) revoke(ctx context.Context, a *platform.App) error {
	return a.DB.WithContext(ctx).Exec("UPDATE console_sessions c LEFT JOIN user_sessions s ON s.id=c.auth_session_id AND s.user_id=c.user_id LEFT JOIN users u ON u.id=c.user_id SET c.status='closed', c.closed_at=?, c.secrets_ciphertext=NULL, c.revision=c.revision+1 WHERE c.status IN ('preparing','ready','active') AND (s.id IS NULL OR s.revoked_at IS NOT NULL OR s.expires_at<=? OR u.id IS NULL OR u.status<>'active')", a.Time(), a.Time()).Error
}

func (s *consoleService) shutdown() {
	s.mu.Lock()
	bridges := s.bridges
	s.bridges = make(map[string]*consoleBridge)
	s.mu.Unlock()
	for id, bridge := range bridges {
		bridge.close(1001, "服务已停止")
		s.finish(id, "closed")
	}
}

func (s *consoleService) stream(c *gin.Context) {
	fail := func(err error) { platform.Fail(c, err) }
	if origins := c.Request.Header.Values("Origin"); len(origins) != 1 || origins[0] != s.a.Config.Origin {
		fail(platform.E(403, "invalid_origin", "WebSocket Origin无效"))
		return
	}
	if c.Request.URL.RawQuery != "" {
		fail(platform.E(400, "invalid_query", "控制台流不接受查询参数"))
		return
	}
	row, err := s.session(c, false)
	if err != nil {
		fail(err)
		return
	}
	if err = s.authorized(c.Request.Context(), row, true); err != nil {
		fail(err)
		return
	}
	protocol := "101cloud.terminal.v1"
	if row.Kind == "vga" {
		protocol = "101cloud.vnc.v1"
	}
	requested := websocket.Subprotocols(c.Request)
	if len(requested) != 1 || requested[0] != protocol || !websocket.IsWebSocketUpgrade(c.Request) {
		fail(platform.E(400, "invalid_protocol", "必须使用指定的WebSocket子协议"))
		return
	}
	s.mu.Lock()
	bridge := s.bridges[row.ID]
	s.mu.Unlock()
	if row.Status != "ready" {
		fail(platform.E(409, "console_not_ready", "控制台未准备好或已被使用"))
		return
	}
	if bridge == nil {
		fail(platform.E(409, "console_process_mismatch", "控制台流必须连接准备该会话的后端进程"))
		return
	}
	result := platform.DB(c, s.a).WithContext(c.Request.Context()).Table("console_sessions").Where("id = ? AND status = 'ready' AND connected_at IS NULL", row.ID).Updates(map[string]any{"status": "active", "connected_at": s.a.Time(), "secrets_ciphertext": nil, "revision": gorm.Expr("revision + 1")})
	if result.Error != nil {
		fail(platform.WrapDB(result.Error))
		return
	}
	if result.RowsAffected != 1 {
		fail(platform.E(409, "console_used", "控制台会话已被使用"))
		return
	}
	if err = platform.Audit(c.Request.Context(), platform.DB(c, s.a), row.UserID, 0, row.InstanceID, "console.connect", map[string]any{"session_id": row.ID, "kind": row.Kind}); err != nil {
		bridge.close(1011, "审计写入失败")
		fail(err)
		return
	}
	upgrader := websocket.Upgrader{ReadBufferSize: 4096, WriteBufferSize: 4096, HandshakeTimeout: 10 * time.Second, Subprotocols: []string{protocol}, CheckOrigin: func(r *http.Request) bool {
		return len(r.Header.Values("Origin")) == 1 && r.Header.Get("Origin") == s.a.Config.Origin
	}}
	browser, err := upgrader.Upgrade(c.Writer, c.Request, http.Header{"Cache-Control": []string{"no-store"}})
	if err != nil {
		bridge.close(1008, "连接已结束")
		return
	}
	defer browser.Close()
	defer bridge.close(1000, "会话已结束")
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = platform.Audit(ctx, s.a.DB, row.UserID, 0, row.InstanceID, "console.disconnect", map[string]any{"session_id": row.ID, "kind": row.Kind})
	}()
	browser.SetReadLimit(consoleFrameLimit)
	_ = browser.SetReadDeadline(time.Now().Add(60 * time.Second))
	browser.SetPongHandler(func(string) error { return browser.SetReadDeadline(time.Now().Add(60 * time.Second)) })
	type input struct {
		kind int
		data []byte
		err  error
	}
	inputs := make(chan input, 1)
	readerDone := make(chan struct{})
	defer close(readerDone)
	go func() {
		for {
			kind, data, err := browser.ReadMessage()
			select {
			case inputs <- input{kind, data, err}:
			case <-readerDone:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	write := func(kind int, data []byte) error {
		_ = browser.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return browser.WriteMessage(kind, data)
	}
	closeBrowser := func(code int, message string) {
		if row.Kind != "vga" && code != 1000 {
			data, _ := json.Marshal(map[string]any{"type": "error", "code": "console_closed", "message": message})
			_ = write(websocket.TextMessage, data)
		}
		_ = browser.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, message), time.Now().Add(2*time.Second))
	}
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-bridge.done:
			closeBrowser(bridge.stop.code, bridge.stop.message)
			return
		case <-ping.C:
			if err = browser.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
				return
			}
		case in := <-inputs:
			if in.err != nil {
				return
			}
			if in.kind == websocket.BinaryMessage {
				if err = bridge.data.write(websocket.BinaryMessage, in.data); err != nil {
					closeBrowser(1011, "上游连接失败")
					return
				}
				continue
			}
			if row.Kind == "vga" {
				closeBrowser(1008, "VGA仅接受二进制帧")
				return
			}
			cols, rows, parseErr := consoleResize(in.data)
			if in.kind != websocket.TextMessage || parseErr != nil {
				closeBrowser(1008, "无效控制消息")
				return
			}
			if row.Kind == "serial" {
				data, _ := json.Marshal(map[string]any{"type": "error", "code": "resize_unsupported", "message": "serial控制台不支持resize"})
				if write(websocket.TextMessage, data) != nil {
					return
				}
				continue
			}
			command, _ := json.Marshal(map[string]any{"command": "window-resize", "args": map[string]string{"width": strconv.Itoa(cols), "height": strconv.Itoa(rows)}})
			if err = bridge.control.write(websocket.TextMessage, command); err != nil {
				closeBrowser(1011, "上游控制通道失败")
				return
			}
		case frame, ok := <-bridge.frames:
			if !ok || frame.err != nil {
				waitCtx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
				var result consoleResult
				select {
				case result = <-bridge.result:
				case <-bridge.done:
					cancel()
					closeBrowser(bridge.stop.code, bridge.stop.message)
					return
				case <-waitCtx.Done():
					result.err = waitCtx.Err()
				}
				op, waitErr := result.operation, result.err
				cancel()
				if waitErr != nil {
					closeBrowser(1011, "上游会话失败")
					return
				}
				if row.Kind != "vga" {
					metadata, _ := consoleOperation(op)["metadata"].(map[string]any)
					exit, exists := metadata["return"]
					if !exists && row.Kind == "terminal" {
						closeBrowser(1011, "上游缺少退出状态")
						return
					}
					code := 0
					if exists {
						var parseErr error
						code, parseErr = strconv.Atoi(platform.Text(exit))
						if parseErr != nil {
							closeBrowser(1011, "上游退出状态无效")
							return
						}
					}
					data, _ := json.Marshal(map[string]any{"type": "exit", "exit_code": code})
					if write(websocket.TextMessage, data) != nil {
						return
					}
				}
				closeBrowser(1000, "")
				return
			}
			if err = write(websocket.BinaryMessage, frame.data); err != nil {
				return
			}
		}
	}
}

func consoleResize(data []byte) (int, int, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return 0, 0, errors.New("invalid resize")
	}
	seen := map[string]bool{}
	kind := ""
	cols, rows := 0, 0
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return 0, 0, err
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return 0, 0, errors.New("duplicate resize field")
		}
		seen[name] = true
		switch name {
		case "type":
			err = dec.Decode(&kind)
		case "cols":
			err = dec.Decode(&cols)
		case "rows":
			err = dec.Decode(&rows)
		default:
			return 0, 0, errors.New("unknown resize field")
		}
		if err != nil {
			return 0, 0, err
		}
	}
	if token, err = dec.Token(); err != nil || token != json.Delim('}') {
		return 0, 0, errors.New("invalid resize")
	}
	if _, err = dec.Token(); err != io.EOF || len(seen) != 3 || kind != "resize" || cols < 1 || cols > 1000 || rows < 1 || rows > 1000 {
		return 0, 0, errors.New("invalid resize")
	}
	return cols, rows, nil
}

// consoleWire performs RFC6455 framing over the HTTP client's upgraded body.
// Using that body is essential: AgentClient's opaque RoundTripper owns both the
// pinned TLS verification and bearer header, neither of which may be bypassed.
type consoleWire struct {
	body      io.ReadWriteCloser
	mu        sync.Mutex
	closeOnce sync.Once
}

func (w *consoleWire) Close() error {
	var err error
	w.closeOnce.Do(func() { err = w.body.Close() })
	return err
}

func (s *consoleService) connect(ctx context.Context, nodeID uint64, operation, project, secret string) (*consoleWire, error) {
	client, origin, err := s.a.AgentClient(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	path, err := consoleOperationPath(operation)
	if err != nil {
		return nil, err
	}
	target := strings.TrimSuffix(origin, "/") + path + "/websocket?" + url.Values{"secret": []string{secret}, "project": []string{project}}.Encode()
	nonce := make([]byte, 16)
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(nonce)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", key)
	// net/http recognizes websocket Upgrade requests and requires HTTP/1.1,
	// including when the pinned transport otherwise enables HTTP/2.
	// A separate handshake cancellation must not outlive successful upgrade.
	handshakeCtx, cancelHandshake := context.WithCancel(ctx)
	req = req.WithContext(handshakeCtx)
	timer := time.AfterFunc(10*time.Second, cancelHandshake)
	resp, err := client.Do(req)
	timer.Stop()
	if err != nil {
		cancelHandshake()
		return nil, errors.New("upstream websocket handshake failed")
	}
	digest := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if resp.StatusCode != 101 || !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") || !consoleHeaderToken(resp.Header.Get("Connection"), "upgrade") || resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(digest[:]) || resp.Header.Get("Sec-WebSocket-Extensions") != "" || resp.Header.Get("Sec-WebSocket-Protocol") != "" {
		_ = resp.Body.Close()
		cancelHandshake()
		return nil, errors.New("invalid upstream websocket handshake")
	}
	body, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		_ = resp.Body.Close()
		cancelHandshake()
		return nil, errors.New("upstream transport does not support upgraded streams")
	}
	return &consoleWire{body: &consoleBody{ReadWriteCloser: body, cancel: cancelHandshake}}, nil
}

type consoleBody struct {
	io.ReadWriteCloser
	cancel context.CancelFunc
}

func (b *consoleBody) Close() error { b.cancel(); return b.ReadWriteCloser.Close() }
func consoleHeaderToken(value, want string) bool {
	for _, v := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(v), want) {
			return true
		}
	}
	return false
}

func (w *consoleWire) write(opcode int, data []byte) error {
	if len(data) > consoleFrameLimit {
		return errors.New("websocket frame exceeds limit")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var header [14]byte
	header[0] = 0x80 | byte(opcode)
	size := 2
	switch {
	case len(data) < 126:
		header[1] = 0x80 | byte(len(data))
	case len(data) <= 65535:
		header[1] = 0x80 | 126
		binary.BigEndian.PutUint16(header[2:4], uint16(len(data)))
		size = 4
	default:
		header[1] = 0x80 | 127
		binary.BigEndian.PutUint64(header[2:10], uint64(len(data)))
		size = 10
	}
	if _, err := rand.Read(header[size : size+4]); err != nil {
		return err
	}
	masked := make([]byte, len(data))
	for i, v := range data {
		masked[i] = v ^ header[size+i%4]
	}
	// An upgraded response body exposes no net.Conn deadlines. Closing it on a
	// bounded timer interrupts blocked writes without weakening pinned transport.
	timer := time.AfterFunc(10*time.Second, func() { _ = w.Close() })
	defer timer.Stop()
	if err := consoleWriteAll(w.body, header[:size+4]); err != nil {
		return err
	}
	return consoleWriteAll(w.body, masked)
}
func consoleWriteAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

func (w *consoleWire) read() (int, []byte, error) {
	opcode := 0
	var message []byte
	for {
		var header [2]byte
		if _, err := io.ReadFull(w.body, header[:]); err != nil {
			return 0, nil, err
		}
		fin := header[0]&0x80 != 0
		kind := int(header[0] & 15)
		if header[0]&0x70 != 0 || header[1]&0x80 != 0 {
			return 0, nil, errors.New("invalid upstream websocket flags")
		}
		length := uint64(header[1] & 127)
		if length == 126 {
			var b [2]byte
			if _, err := io.ReadFull(w.body, b[:]); err != nil {
				return 0, nil, err
			}
			length = uint64(binary.BigEndian.Uint16(b[:]))
			if length < 126 {
				return 0, nil, errors.New("invalid frame length")
			}
		} else if length == 127 {
			var b [8]byte
			if _, err := io.ReadFull(w.body, b[:]); err != nil {
				return 0, nil, err
			}
			length = binary.BigEndian.Uint64(b[:])
			if length < 65536 {
				return 0, nil, errors.New("invalid frame length")
			}
		}
		if length > consoleFrameLimit || (kind >= 8 && (!fin || length > 125)) {
			return 0, nil, errors.New("upstream frame exceeds limit")
		}
		payload := make([]byte, int(length))
		if _, err := io.ReadFull(w.body, payload); err != nil {
			return 0, nil, err
		}
		switch kind {
		case websocket.CloseMessage:
			if len(payload) == 1 {
				return 0, nil, errors.New("invalid websocket close")
			}
			return 0, nil, io.EOF
		case websocket.PingMessage:
			if err := w.write(websocket.PongMessage, payload); err != nil {
				return 0, nil, err
			}
			continue
		case websocket.PongMessage:
			continue
		case websocket.TextMessage, websocket.BinaryMessage:
			if opcode != 0 {
				return 0, nil, errors.New("unexpected websocket data frame")
			}
			opcode = kind
		case 0:
			if opcode == 0 {
				return 0, nil, errors.New("unexpected continuation")
			}
		default:
			return 0, nil, errors.New("invalid websocket opcode")
		}
		if len(message)+len(payload) > consoleFrameLimit {
			return 0, nil, errors.New("upstream message exceeds limit")
		}
		if len(message) == 0 && fin {
			return opcode, payload, nil
		}
		message = append(message, payload...)
		if fin {
			return opcode, message, nil
		}
	}
}

func consoleAllowed(inst platform.Row) bool {
	var actions []string
	if json.Unmarshal([]byte(platform.Text(inst["allowed_user_actions"])), &actions) != nil {
		return false
	}
	for _, action := range actions {
		if action == "console" {
			return true
		}
	}
	return false
}
