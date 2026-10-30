package platform

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	mysqlerr "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type HTTPError struct {
	Status  int
	Code    string
	Message string
	Details any
}

func (e *HTTPError) Error() string    { return e.Code + ": " + e.Message }
func E(s int, code, msg string) error { return &HTTPError{Status: s, Code: code, Message: msg} }
func WrapDB(e error) error {
	if e == nil {
		return nil
	}
	var he *HTTPError
	if errors.As(e, &he) {
		return e
	}
	if errors.Is(e, gorm.ErrRecordNotFound) {
		return E(404, "not_found", "资源不存在或已被移除，请刷新列表后重试。")
	}
	var m *mysqlerr.MySQLError
	if errors.As(e, &m) {
		switch m.Number {
		case 1062:
			return E(409, "configuration_conflict", "资源配置冲突或已有操作正在进行，请刷新状态后再试。")
		case 1451, 1452, 3819, 1048:
			return E(422, "validation_failed", "请求不符合资源约束，请检查配置；如配置无误，请联系管理员。")
		case 1205, 1213:
			return E(409, "instance_busy", "资源正在被其他操作修改，请刷新状态后重试。")
		}
	}
	return E(500, "internal_error", "服务暂时无法完成请求，请稍后重试；如持续失败，请携带请求编号联系管理员。")
}
func Reply(c *gin.Context, status int, data any) {
	if status == 204 {
		c.Status(204)
		c.Writer.WriteHeaderNow()
		return
	}
	c.JSON(status, gin.H{"data": data, "request_id": c.GetString("request_id")})
}
func Fail(c *gin.Context, e error) {
	var he *HTTPError
	if !errors.As(e, &he) {
		he = &HTTPError{Status: 500, Code: "internal_error", Message: "服务暂时无法完成请求，请稍后重试；如持续失败，请携带请求编号联系管理员。"}
		LogError("request", e)
	}
	c.Set("request_failed", true)
	c.AbortWithStatusJSON(he.Status, gin.H{"error": PublicError(he, false), "request_id": c.GetString("request_id")})
}

type Endpoint func(*gin.Context) (any, error)

func Handle(fn Endpoint) gin.HandlerFunc {
	return func(c *gin.Context) {
		v, e := fn(c)
		if e != nil {
			Fail(c, WrapDB(e))
			return
		}
		if !c.Writer.Written() {
			Reply(c, 200, v)
		}
	}
}
func ValidateKeys(m map[string]any, keys ...string) error {
	allowed := make(map[string]bool, len(keys))
	for _, k := range keys {
		allowed[k] = true
	}
	for k := range m {
		if !allowed[k] {
			return requestFieldError(k, "请求包含不支持的字段，请按接口要求调整后重试。")
		}
	}
	return nil
}
func Decode(c *gin.Context, v any) error {
	b, e := io.ReadAll(io.LimitReader(c.Request.Body, (1<<20)+1))
	if e != nil || len(b) > 1<<20 {
		return EDetails(400, "invalid_json", "请求内容不是有效的 JSON，或超过 1 MiB 大小限制。", Row{"action": "请检查 JSON 格式并缩小请求内容后重试。"})
	}
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		b = []byte("{}")
	}
	if b[0] != '{' {
		return requestFieldError("", "请求正文必须是 JSON 对象。")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	if e = uniqueJSON(d); e != nil {
		return EDetails(400, "invalid_json", "请求 JSON 格式错误或包含重复字段。", Row{"action": "请修正 JSON 语法，确保每个字段只出现一次。"})
	}
	if _, e = d.Token(); e != io.EOF {
		return EDetails(400, "invalid_json", "请求包含多个 JSON 值或多余内容。", Row{"action": "请只提交一个 JSON 对象。"})
	}
	d = json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(v); e != nil {
		var typeError *json.UnmarshalTypeError
		if errors.As(e, &typeError) {
			return requestFieldError(typeError.Field, "请求字段的类型或数值范围不正确。")
		}
		const unknown = "json: unknown field "
		if strings.HasPrefix(e.Error(), unknown) {
			field, _ := strconv.Unquote(strings.TrimPrefix(e.Error(), unknown))
			return requestFieldError(field, "请求包含不支持的字段，请按接口要求调整后重试。")
		}
		return requestFieldError("", "请求字段格式不正确，请检查字段类型及必填项。")
	}
	return nil
}
func uniqueJSON(d *json.Decoder) error {
	t, e := d.Token()
	if e != nil {
		return e
	}
	if delim, ok := t.(json.Delim); ok {
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return e
				}
				s, ok := k.(string)
				if !ok || seen[s] {
					return errors.New("duplicate key")
				}
				seen[s] = true
				if e = uniqueJSON(d); e != nil {
					return e
				}
			}
		case '[':
			for d.More() {
				if e = uniqueJSON(d); e != nil {
					return e
				}
			}
		default:
			return errors.New("unexpected delimiter")
		}
		_, e = d.Token()
		return e
	}
	return nil
}
func IDString(s string) (uint64, error) {
	if s == "" || strings.HasPrefix(s, "+") || strings.HasPrefix(s, "-") {
		return 0, E(422, "validation_failed", "invalid ID")
	}
	u, e := strconv.ParseUint(s, 10, 64)
	if e != nil || u == 0 {
		return 0, E(422, "validation_failed", "invalid ID")
	}
	return u, nil
}
func ID(c *gin.Context, param string) (uint64, error) { return IDString(c.Param(param)) }
func Text(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	case json.Number:
		return x.String()
	case time.Time:
		return x.In(time.FixedZone("UTC+8", 28800)).Format(time.RFC3339Nano)
	default:
		return fmt.Sprint(v)
	}
}
func Uint(v any) uint64 {
	switch x := v.(type) {
	case uint64:
		return x
	case uint:
		return uint64(x)
	case int64:
		if x >= 0 {
			return uint64(x)
		}
	case int:
		if x >= 0 {
			return uint64(x)
		}
	case float64:
		if x >= 0 {
			return uint64(x)
		}
	case uint8:
		return uint64(x)
	case bool:
		if x {
			return 1
		}
	}
	u, _ := strconv.ParseUint(Text(v), 10, 64)
	return u
}
func Bool(v any) bool { return v == true || Text(v) == "true" || Uint(v) == 1 }
func Stamp(v any) time.Time {
	if t, ok := v.(time.Time); ok {
		return t.In(time.FixedZone("UTC+8", 28800))
	}
	for _, f := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999", "2006-01-02 15:04:05"} {
		t, e := time.ParseInLocation(f, Text(v), time.FixedZone("UTC+8", 28800))
		if e == nil {
			return t
		}
	}
	return time.Time{}
}
func Get(db *gorm.DB, table string, id uint64, lock bool) (Row, error) {
	var r Row
	q := db.Table(table).Where("id=?", id)
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	e := q.Take(&r).Error
	if e != nil {
		return nil, WrapDB(e)
	}
	return r, nil
}
func Owned(c *gin.Context, a *App, table string, id uint64) (Row, error) {
	r, e := Get(DB(c, a), table, id, c.Request.Method != "GET")
	if e != nil {
		return nil, e
	}
	if !c.GetBool("admin") && Uint(r["user_id"]) != Current(c).ID {
		return nil, E(404, "not_found", "resource not found")
	}
	return r, nil
}
func Revision(c *gin.Context, r Row) error {
	h := c.GetHeader("If-Match")
	if h == "" {
		return E(428, "precondition_required", "If-Match required")
	}
	if h != fmt.Sprintf("\"rev-%d\"", Uint(r["revision"])) {
		return E(412, "revision_conflict", "resource changed; refresh")
	}
	return nil
}
func SetETag(c *gin.Context, r Row) { c.Header("ETag", fmt.Sprintf("\"rev-%d\"", Uint(r["revision"]))) }
func Save(c *gin.Context, a *App, table string, r Row, changes Row) (Row, error) {
	if e := Revision(c, r); e != nil {
		return nil, e
	}
	changes["revision"] = Uint(r["revision"]) + 1
	res := DB(c, a).Table(table).Where("id=? AND revision=?", r["id"], r["revision"]).Updates(changes)
	if res.Error != nil {
		return nil, WrapDB(res.Error)
	}
	if res.RowsAffected != 1 {
		return nil, E(412, "revision_conflict", "resource changed")
	}
	next, e := Get(DB(c, a), table, Uint(r["id"]), false)
	if e == nil {
		SetETag(c, next)
	}
	return next, e
}
func Public(r Row, fields ...string) Row {
	v := Row{}
	if len(fields) == 0 {
		for k, x := range r {
			v[k] = publicValue(k, x)
		}
		return v
	}
	for _, k := range fields {
		if x, ok := r[k]; ok {
			v[k] = publicValue(k, x)
		}
	}
	return v
}
func publicValue(k string, x any) any {
	if x == nil {
		return nil
	}
	if t, ok := x.(time.Time); ok {
		return t.In(time.FixedZone("UTC+8", 28800)).Format(time.RFC3339Nano)
	}
	if k == "id" || k == "revision" || strings.HasSuffix(k, "_id") || strings.HasSuffix(k, "_bytes") || strings.HasSuffix(k, "_ns") {
		return Text(x)
	}
	switch v := x.(type) {
	case []byte:
		s := string(v)
		if strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[") {
			var out any
			d := json.NewDecoder(strings.NewReader(s))
			d.UseNumber()
			if d.Decode(&out) == nil {
				return out
			}
		}
		return s
	}
	return x
}
func Limit(c *gin.Context, def, max int) (int, error) {
	s := c.Query("limit")
	if s == "" {
		return def, nil
	}
	n, e := strconv.Atoi(s)
	if e != nil || n < 1 || n > max {
		return 0, E(422, "validation_failed", "invalid limit")
	}
	return n, nil
}

type listCursor struct {
	At    string `json:"at"`
	ID    uint64 `json:"id"`
	Scope string `json:"scope"`
}

func (a *App) sign(p []byte) []byte {
	m := hmac.New(sha256.New, a.Config.MasterKey)
	m.Write(p)
	return m.Sum(nil)
}
func (a *App) cursorScope(c *gin.Context) string {
	q := url.Values{}
	for k, v := range c.Request.URL.Query() {
		if k != "limit" && k != "cursor" {
			q[k] = v
		}
	}
	return fmt.Sprintf("%d:%s:%s", Current(c).ID, c.Request.URL.Path, q.Encode())
}
func List(c *gin.Context, a *App, q *gorm.DB, filters []string) (any, error) {
	return ListMap(c, a, q, filters, func(r Row) (Row, error) { return Public(r), nil })
}

// ListMap pages like List and projects every row through dto, so list items can
// reuse the exact detail representation of the resource.
func ListMap(c *gin.Context, a *App, q *gorm.DB, filters []string, dto func(Row) (Row, error)) (Row, error) {
	allow := map[string]bool{"limit": true, "cursor": true}
	for _, k := range filters {
		allow[k] = true
	}
	for k := range c.Request.URL.Query() {
		if !allow[k] {
			return nil, E(422, "validation_failed", "unknown query parameter: "+k)
		}
	}
	n, e := Limit(c, 20, 100)
	if e != nil {
		return nil, e
	}
	scope := a.cursorScope(c)
	if s := c.Query("cursor"); s != "" {
		raw, e := base64.RawURLEncoding.DecodeString(s)
		if e != nil || len(raw) < 32 {
			return nil, E(400, "invalid_cursor", "invalid cursor")
		}
		body, sig := raw[:len(raw)-32], raw[len(raw)-32:]
		var v listCursor
		if !hmac.Equal(sig, a.sign(body)) || json.Unmarshal(body, &v) != nil || v.Scope != scope {
			return nil, E(400, "invalid_cursor", "invalid cursor scope")
		}
		t, e := time.Parse(time.RFC3339Nano, v.At)
		if e != nil {
			return nil, E(400, "invalid_cursor", "invalid cursor time")
		}
		q = q.Where("(created_at < ? OR (created_at = ? AND id < ?))", t, t, v.ID)
	}
	var rows []Row
	if e = q.Order("created_at DESC, id DESC").Limit(n + 1).Find(&rows).Error; e != nil {
		return nil, WrapDB(e)
	}
	var next any
	if len(rows) > n {
		rows = rows[:n]
		r := rows[len(rows)-1]
		t := Stamp(r["created_at"])
		if t.IsZero() || Uint(r["id"]) == 0 {
			return nil, E(500, "internal_error", "list projection lacks cursor fields")
		}
		b, _ := json.Marshal(listCursor{At: t.Format(time.RFC3339Nano), ID: Uint(r["id"]), Scope: scope})
		next = base64.RawURLEncoding.EncodeToString(append(b, a.sign(b)...))
	}
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		item, e := dto(r)
		if e != nil {
			return nil, e
		}
		out = append(out, item)
	}
	return Row{"items": out, "next_cursor": next}, nil
}
func RequestMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("request_id", "req-"+RandomToken()[:16])
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Cache-Control", "no-store")
		c.Next()
	}
}
