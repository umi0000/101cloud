package platform

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type responseBuffer struct {
	gin.ResponseWriter
	body    bytes.Buffer
	headers http.Header
	status  int
	written bool
}

func (w *responseBuffer) Header() http.Header { return w.headers }
func (w *responseBuffer) WriteHeader(s int) {
	if !w.written {
		w.status = s
		w.written = true
	}
}
func (w *responseBuffer) WriteHeaderNow() {
	if !w.written {
		w.WriteHeader(200)
	}
}
func (w *responseBuffer) Write(b []byte) (int, error)       { w.WriteHeaderNow(); return w.body.Write(b) }
func (w *responseBuffer) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *responseBuffer) Status() int {
	if w.status == 0 {
		return 200
	}
	return w.status
}
func (w *responseBuffer) Size() int {
	if !w.written {
		return -1
	}
	return w.body.Len()
}
func (w *responseBuffer) Written() bool { return w.written }
func (w *responseBuffer) Flush()        { w.WriteHeaderNow() }

// Mutation requests commit their domain writes, idempotency record, and audit
// together. A response is never released before the SQL transaction commits.
func (a *App) Mutations() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead || c.Request.Method == http.MethodOptions {
			c.Next()
			return
		}
		raw, e := io.ReadAll(io.LimitReader(c.Request.Body, (1<<20)+1))
		if e != nil || len(raw) > 1<<20 {
			Fail(c, E(400, "invalid_json", "invalid request size"))
			return
		}
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			raw = []byte("{}")
		}
		if raw[0] != '{' {
			Fail(c, E(422, "validation_failed", "JSON object required"))
			return
		}
		check := json.NewDecoder(bytes.NewReader(raw))
		if uniqueJSON(check) != nil {
			Fail(c, E(400, "invalid_json", "duplicate or malformed JSON"))
			return
		}
		if _, e = check.Token(); e != io.EOF {
			Fail(c, E(400, "invalid_json", "trailing JSON"))
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(raw))
		var canonical any
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		if d.Decode(&canonical) != nil {
			Fail(c, E(400, "invalid_json", "invalid JSON"))
			return
		}
		body, _ := json.Marshal(canonical)
		digest := a.sign(body)
		needKey := c.Request.Method == http.MethodPost || c.Request.Method == http.MethodDelete
		key := c.GetHeader("Idempotency-Key")
		if needKey {
			if len(key) < 1 || len(key) > 128 || strings.IndexFunc(key, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
				Fail(c, E(422, "validation_failed", "Idempotency-Key required (1-128 printable ASCII characters)"))
				return
			}
		}
		scope := hex.EncodeToString(a.sign([]byte(Text(Current(c).ID) + ":" + c.Request.Method + ":" + c.Request.URL.EscapedPath())))
		original := c.Writer
		buffer := &responseBuffer{ResponseWriter: original, headers: original.Header().Clone()}
		c.Writer = buffer
		defer func() { c.Writer = original }()
		rollback := errors.New("request rejected")
		mutationCtx := context.WithValue(c.Request.Context(), initiatingUserKey{}, Current(c).ID)
		e = a.DB.WithContext(mutationCtx).Transaction(func(tx *gorm.DB) error {
			c.Set("transaction", tx)
			defer c.Set("transaction", a.DB.WithContext(c.Request.Context()))
			if needKey {
				r := Row{"scope": scope, "request_key": key, "request_digest": digest, "status_code": 0}
				if e := tx.Exec("INSERT INTO backend_requests(scope,request_key,request_digest,status_code) VALUES(?,?,?,0) ON DUPLICATE KEY UPDATE scope=scope", scope, key, digest).Error; e != nil {
					return e
				}
				if e := tx.Raw("SELECT * FROM backend_requests WHERE scope=? AND request_key=? FOR UPDATE", scope, key).Scan(&r).Error; e != nil {
					return e
				}
				if !bytes.Equal([]byte(Text(r["request_digest"])), digest) {
					Fail(c, E(409, "idempotency_conflict", "idempotency key was used for another request"))
					return rollback
				}
				if Uint(r["status_code"]) > 0 {
					var saved Row
					if json.Unmarshal([]byte(Text(r["response"])), &saved) != nil {
						return errors.New("invalid idempotency result")
					}
					saved["request_id"] = c.GetString("request_id")
					if s := Text(r["etag"]); s != "" {
						c.Header("ETag", s)
					}
					if s := Text(r["location"]); s != "" {
						c.Header("Location", s)
					}
					if Uint(r["status_code"]) == 204 {
						c.Status(204)
					} else {
						c.JSON(int(Uint(r["status_code"])), saved)
					}
					c.Abort()
					return nil
				}
			}
			c.Next()
			if c.GetBool("request_failed") || buffer.Status() >= 400 {
				return rollback
			}
			if !buffer.Written() {
				c.Status(204)
			}
			if needKey {
				saved := buffer.body.Bytes()
				if replacement, ok := c.Get("idempotency_replay"); ok {
					saved, _ = json.Marshal(Row{"data": replacement})
				}
				if len(saved) == 0 {
					saved = []byte("{}")
				}
				if e := tx.Table("backend_requests").Where("scope=? AND request_key=?", scope, key).Updates(Row{"status_code": buffer.Status(), "response": string(saved), "etag": c.Writer.Header().Get("ETag"), "location": c.Writer.Header().Get("Location")}).Error; e != nil {
					return e
				}
			}
			var instanceID uint64
			if strings.Contains(c.FullPath(), "/instances/:id") {
				instanceID = Uint(c.Param("id"))
			}
			return Audit(c.Request.Context(), tx, Current(c).ID, 0, instanceID, "api.mutation", Row{"request_id": c.GetString("request_id"), "method": c.Request.Method, "route": c.FullPath()})
		})
		c.Writer = original
		if e != nil && !errors.Is(e, rollback) {
			Fail(c, WrapDB(e))
			return
		}
		if e == nil {
			if value, ok := c.Get("after_commit"); ok {
				for _, hook := range value.([]func()) {
					hook()
				}
			}
		}
		for k, v := range buffer.Header() {
			if e != nil && (strings.EqualFold(k, "Set-Cookie") || strings.EqualFold(k, "ETag") || strings.EqualFold(k, "Location")) {
				continue
			}
			original.Header()[k] = v
		}
		original.WriteHeader(buffer.Status())
		if buffer.body.Len() > 0 {
			_, _ = original.Write(buffer.body.Bytes())
		}
	}
}
