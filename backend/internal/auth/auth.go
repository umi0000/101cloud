package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/argon2"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const sessionCookie = "__Host-101cloud_session"
const csrfCookie = "__Host-101cloud_csrf"
const csrfTTL = 30 * time.Minute

// Only token hashes are persisted. Session validity always includes current user state.
type identity struct {
	ID        uint64
	Username  string
	Role      string
	SessionID uint64
}

func Middleware(a *platform.App) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, err := c.Cookie(sessionCookie)
		if err != nil || !validToken(token) {
			platform.Fail(c, unauthenticated())
			c.Abort()
			return
		}
		hash := sha256.Sum256([]byte(token))
		var u identity
		result := a.DB.WithContext(c.Request.Context()).Table("user_sessions s").
			Select("u.id, u.username, u.role, s.id AS session_id").
			Joins("JOIN users u ON u.id = s.user_id").
			Where("s.token_hash = ? AND s.revoked_at IS NULL AND s.expires_at > ? AND u.status = ?", hash[:], a.Time(), "active").Take(&u)
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			clearCookie(c, sessionCookie)
			platform.Fail(c, unauthenticated())
			c.Abort()
			return
		}
		if result.Error != nil {
			platform.Fail(c, platform.WrapDB(result.Error))
			c.Abort()
			return
		}
		c.Set("user", platform.User{ID: u.ID, Username: u.Username, Role: u.Role, SessionID: u.SessionID})
		c.Header("Cache-Control", "no-store")
		c.Next()
	}
}

// ValidateSession is also used by long-lived websocket bridges after HTTP admission.
func ValidateSession(ctx context.Context, a *platform.App, sessionID, userID uint64) error {
	var n int64
	err := a.DB.WithContext(ctx).Table("user_sessions s").Joins("JOIN users u ON u.id = s.user_id").
		Where("s.id = ? AND s.user_id = ? AND s.revoked_at IS NULL AND s.expires_at > ? AND u.status = ?", sessionID, userID, a.Time(), "active").Count(&n).Error
	if err != nil {
		return platform.WrapDB(err)
	}
	if n != 1 {
		return unauthenticated()
	}
	return nil
}

// CSRF binds a signed, short-lived browser context to the opaque session cookie.
// A stolen form token from a pre-login context cannot authorize a logged-in request.
func CSRF(a *platform.App) gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			c.Next()
			return
		}
		if c.GetHeader("Origin") != a.Config.Origin || c.GetHeader("Sec-Fetch-Site") == "cross-site" {
			platform.Fail(c, platform.E(403, "csrf_failed", "Origin validation failed"))
			c.Abort()
			return
		}
		cookie, err := c.Cookie(csrfCookie)
		supplied := c.GetHeader("X-CSRF-Token")
		if err != nil || !validCSRFContext(a, cookie) || !hmac.Equal([]byte(supplied), []byte(csrfToken(a, c, cookie))) {
			platform.Fail(c, platform.E(403, "csrf_failed", "CSRF token is missing or expired"))
			c.Abort()
			return
		}
		c.Next()
	}
}

func Register(a *platform.App, api, user, admin *gin.RouterGroup) {
	api.GET("/auth/csrf", platform.Handle(func(c *gin.Context) (any, error) {
		if origin := c.GetHeader("Origin"); (origin != "" && origin != a.Config.Origin) || c.GetHeader("Sec-Fetch-Site") == "cross-site" {
			return nil, platform.E(403, "csrf_failed", "Origin validation failed")
		}
		c.Header("Cache-Control", "no-store")
		context, err := c.Cookie(csrfCookie)
		if err != nil || !validCSRFContext(a, context) {
			context, err = newCSRFContext(a)
			if err != nil {
				return nil, err
			}
			setCookie(c, csrfCookie, context, int(csrfTTL.Seconds()))
		}
		return gin.H{"csrf_token": csrfToken(a, c, context)}, nil
	}))
	api.POST("/auth/login", platform.Handle(func(c *gin.Context) (any, error) { return login(c, a) }))
	api.POST("/auth/logout", Middleware(a), platform.Handle(func(c *gin.Context) (any, error) {
		current := platform.Current(c)
		err := a.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
			if err := tx.Table("user_sessions").Where("id = ? AND user_id = ? AND revoked_at IS NULL", current.SessionID, current.ID).
				Updates(map[string]any{"revoked_at": a.Time(), "revision": gorm.Expr("revision + 1")}).Error; err != nil {
				return platform.WrapDB(err)
			}
			return platform.Audit(c.Request.Context(), tx, current.ID, 0, 0, "auth.logout", nil)
		})
		if err != nil {
			return nil, platform.WrapDB(err)
		}
		clearCookie(c, sessionCookie)
		clearCookie(c, csrfCookie)
		noContent(c)
		return nil, nil
	}))
	registerMe(a, user)
	registerAdmin(a, admin)
	a.AddPoller("auth-expired-rate-limits", time.Hour, func(ctx context.Context, a *platform.App) error {
		return a.DB.WithContext(ctx).Exec("DELETE FROM auth_rate_limits WHERE expires_at < ? LIMIT 10000", a.Time()).Error
	})
}

func login(c *gin.Context, a *platform.App) (any, error) {
	c.Header("Cache-Control", "no-store")
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := platform.Decode(c, &input); err != nil {
		return nil, err
	}
	var existing struct {
		ID           uint64
		PasswordHash []byte
		Status       string
	}
	if validUsername(input.Username) {
		err := a.DB.WithContext(c.Request.Context()).Table("users").Select("id,password_hash,status").Where("username = ?", input.Username).Take(&existing).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, platform.WrapDB(err)
		}
	}
	dimension := "name:" + strings.ToLower(input.Username)
	if existing.ID != 0 {
		dimension = "account:" + strconv.FormatUint(existing.ID, 10)
	}
	if err := loginLimit(c, a, dimension); err != nil {
		return nil, err
	}
	encoded := existing.PasswordHash
	if existing.ID == 0 {
		encoded = []byte(dummyPasswordHash)
	}
	passwordOK := verifyPassword(string(encoded), input.Password)
	if !passwordOK || existing.ID == 0 || existing.Status != "active" || !validUsername(input.Username) || utf8.RuneCountInString(input.Password) > 128 {
		return nil, invalidCredentials()
	}
	token, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	context, err := newCSRFContext(a)
	if err != nil {
		return nil, err
	}
	now := a.Time()
	ttl := a.Config.SessionTTL
	if ttl <= 0 {
		return nil, platform.E(500, "internal_error", "Session lifetime is not configured")
	}
	hash := sha256.Sum256([]byte(token))
	var user platform.Row
	// Recheck the password hash and status under lock: concurrent reset/disable wins.
	err = a.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		var e error
		user, e = platform.Get(tx, "users", existing.ID, true)
		if e != nil {
			return e
		}
		if platform.Text(user["status"]) != "active" || !hmac.Equal(bytesOf(user["password_hash"]), existing.PasswordHash) {
			return invalidCredentials()
		}
		if old, e := c.Cookie(sessionCookie); e == nil && validToken(old) {
			h := sha256.Sum256([]byte(old))
			if e = tx.Table("user_sessions").Where("token_hash = ? AND revoked_at IS NULL", h[:]).Updates(map[string]any{"revoked_at": now, "revision": gorm.Expr("revision + 1")}).Error; e != nil {
				return platform.WrapDB(e)
			}
		}
		ip := net.ParseIP(c.ClientIP())
		var packed []byte
		if ip != nil {
			if v4 := ip.To4(); v4 != nil {
				packed = v4
			} else {
				packed = ip.To16()
			}
		}
		ua := c.Request.UserAgent()
		if utf8.RuneCountInString(ua) > 1024 {
			ua = string([]rune(ua)[:1024])
		}
		row := map[string]any{"user_id": existing.ID, "token_hash": hash[:], "expires_at": now.Add(ttl), "client_ip": packed, "user_agent": ua, "created_at": now, "updated_at": now}
		if e = tx.Table("user_sessions").Create(row).Error; e != nil {
			return platform.WrapDB(e)
		}
		if e = tx.Table("users").Where("id = ?", existing.ID).Update("last_login_at", now).Error; e != nil {
			return platform.WrapDB(e)
		}
		return platform.Audit(c.Request.Context(), tx, existing.ID, 0, 0, "auth.login", nil)
	})
	if err != nil {
		return nil, err
	}
	setCookie(c, sessionCookie, token, int(ttl.Seconds()))
	setCookie(c, csrfCookie, context, int(csrfTTL.Seconds()))
	// csrfToken reads the old request cookie, so explicitly bind the rotated token.
	return gin.H{"user": userDTO(user), "csrf_token": csrfFor(a, context, token)}, nil
}

func loginLimit(c *gin.Context, a *platform.App, username string) error {
	now := a.Time()
	start := now.Truncate(15 * time.Minute)
	limited := false
	err := a.DB.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		for _, dimension := range []struct {
			key     string
			maximum uint64
		}{{"ip:" + c.ClientIP(), 50}, {"username:" + strings.ToLower(username), 10}} {
			digest := keyed(a, "login-rate:"+dimension.key)
			if err := tx.Exec("INSERT INTO auth_rate_limits (bucket_key,window_start,attempts,expires_at) VALUES (?,?,1,?) ON DUPLICATE KEY UPDATE attempts=IF(window_start < VALUES(window_start),1,attempts+1), window_start=VALUES(window_start), expires_at=VALUES(expires_at)", string(digest), start, start.Add(30*time.Minute)).Error; err != nil {
				return platform.WrapDB(err)
			}
			var attempts uint64
			if err := tx.Table("auth_rate_limits").Select("attempts").Where("bucket_key = ?", digest).Scan(&attempts).Error; err != nil {
				return platform.WrapDB(err)
			}
			if attempts > dimension.maximum {
				limited = true
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if limited {
		c.Header("Retry-After", strconv.FormatInt(int64(start.Add(15*time.Minute).Sub(now).Seconds())+1, 10))
		return platform.E(429, "rate_limited", "Too many login attempts")
	}
	return nil
}

func newCSRFContext(a *platform.App) (string, error) {
	token, err := randomToken(32)
	if err != nil {
		return "", err
	}
	body := token + "." + strconv.FormatInt(a.Time().Add(csrfTTL).Unix(), 10)
	return body + "." + base64.RawURLEncoding.EncodeToString(keyed(a, "csrf-context:"+body)), nil
}
func validCSRFContext(a *platform.App, value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 3 || !validToken(parts[0]) {
		return false
	}
	expiry, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || expiry <= a.Time().Unix() || expiry > a.Time().Add(csrfTTL+time.Minute).Unix() {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	return err == nil && hmac.Equal(sig, keyed(a, "csrf-context:"+parts[0]+"."+parts[1]))
}
func csrfToken(a *platform.App, c *gin.Context, context string) string {
	token, _ := c.Cookie(sessionCookie)
	return csrfFor(a, context, token)
}
func csrfFor(a *platform.App, context, token string) string {
	return base64.RawURLEncoding.EncodeToString(keyed(a, "csrf-token:"+context+":"+token))
}
func keyed(a *platform.App, text string) []byte {
	h := hmac.New(sha256.New, a.Config.MasterKey)
	h.Write([]byte(text))
	return h.Sum(nil)
}
func randomToken(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", platform.E(500, "internal_error", "Secure random generation failed")
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func validToken(token string) bool {
	if len(token) != 43 {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(b) == 32 && base64.RawURLEncoding.EncodeToString(b) == token
}
func setCookie(c *gin.Context, name, value string, age int) {
	http.SetCookie(c.Writer, &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: age, Expires: time.Now().Add(time.Duration(age) * time.Second)})
}
func clearCookie(c *gin.Context, name string) {
	http.SetCookie(c.Writer, &http.Cookie{Name: name, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1, Expires: time.Unix(1, 0)})
}
func noContent(c *gin.Context) {
	c.Status(http.StatusNoContent)
	c.Writer.WriteHeaderNow()
}
func unauthenticated() error { return platform.E(401, "unauthenticated", "Authentication required") }
func invalidCredentials() error {
	return platform.E(401, "unauthenticated", "Invalid username or password")
}
func validation(message string) error { return platform.E(422, "validation_failed", message) }
func bytesOf(v any) []byte {
	if b, ok := v.([]byte); ok {
		return b
	}
	return []byte(platform.Text(v))
}

const dummyPasswordHash = "$argon2id$v=19$m=65536,t=3,p=2$c2VjdXJlLWR1bW15LXNhbHQ$72mVHDVMjQmAWD5VdCAbzMeblo5hj+XBFQVGl8uqrdg"

func hashPassword(password string) (string, error) {
	if !validPassword(password) {
		return "", validation("Password must contain 12–128 characters")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", platform.E(500, "internal_error", "Secure random generation failed")
	}
	key := argon2.IDKey([]byte(password), salt, 3, 65536, 2, 32)
	return fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=2$%s$%s", base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}
func verifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != "m=65536,t=3,p=2" || len(password) > 512 {
		return false
	}
	salt, e1 := base64.RawStdEncoding.DecodeString(parts[4])
	expected, e2 := base64.RawStdEncoding.DecodeString(parts[5])
	if e1 != nil || e2 != nil || len(salt) < 16 || len(salt) > 64 || len(expected) != 32 {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, 3, 65536, 2, 32)
	return subtle.ConstantTimeCompare(expected, actual) == 1
}
func validPassword(password string) bool {
	n := utf8.RuneCountInString(password)
	return utf8.ValidString(password) && n >= 12 && n <= 128
}
func validUsername(value string) bool {
	n := utf8.RuneCountInString(value)
	if !utf8.ValidString(value) || n < 1 || n > 64 || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func validEmail(value string) bool {
	if utf8.RuneCountInString(value) > 254 || len(value) > 1016 {
		return false
	}
	parsed, err := mail.ParseAddress(value)
	return err == nil && parsed.Address == value && parsed.Name == ""
}
func userDTO(row platform.Row) platform.Row {
	return platform.Public(row, "id", "username", "email", "role", "status", "revision", "created_at")
}
func zeroLimits(userID uint64) map[string]any {
	return map[string]any{"user_id": userID, "max_instances": 0, "max_vcpus": 0, "max_memory_bytes": 0, "max_disk_bytes": 0, "max_gpu_allocations": 0, "max_resident_instances": 0}
}
func lockAdminGuard(db *gorm.DB) error {
	var guard struct{ ID uint64 }
	return platform.WrapDB(db.Table("auth_admin_guard").Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").Where("id = 1").Take(&guard).Error)
}

// InitialAdministrator is returned only to the process that committed the first
// account. It must never be persisted in API or idempotency response payloads.
type InitialAdministrator struct {
	Username string `json:"-"`
	Password string `json:"-"`
}

func EnsureAdministrator(ctx context.Context, a *platform.App) (*InitialAdministrator, error) {
	var initial *InitialAdministrator
	err := a.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockAdminGuard(tx); err != nil {
			return err
		}
		var guard struct{ InitializedAt *time.Time }
		if err := tx.Table("auth_admin_guard").Select("initialized_at").Where("id=1").Take(&guard).Error; err != nil {
			return err
		}
		if guard.InitializedAt != nil {
			return nil
		}
		var count int64
		if err := tx.Table("users").Where("role=?", "admin").Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return tx.Table("auth_admin_guard").Where("id=1").Update("initialized_at", a.Time()).Error
		}
		name, err := randomToken(12)
		if err != nil {
			return err
		}
		password, err := randomToken(32)
		if err != nil {
			return err
		}
		username := "admin-" + name
		hash, err := hashPassword(password)
		if err != nil {
			return err
		}
		row := map[string]any{"username": username, "password_hash": []byte(hash), "role": "admin", "status": "active", "created_at": a.Time(), "updated_at": a.Time()}
		if err := tx.Table("users").Create(row).Error; err != nil {
			return platform.WrapDB(err)
		}
		id, err := platform.InsertedID(row)
		if err != nil {
			return err
		}
		if err := tx.Table("user_limits").Create(zeroLimits(id)).Error; err != nil {
			return platform.WrapDB(err)
		}
		if err := platform.Audit(ctx, tx, id, 0, 0, "auth.bootstrap", nil); err != nil {
			return err
		}
		if err := tx.Table("auth_admin_guard").Where("id=1").Update("initialized_at", a.Time()).Error; err != nil {
			return err
		}
		initial = &InitialAdministrator{Username: username, Password: password}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return initial, nil
}
