package platform

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type Row = map[string]any

type Config struct {
	Listen               string
	Origin               string
	DSN                  string
	MasterKey            []byte
	SessionTTL           time.Duration
	ConsoleTTL           time.Duration
	ConsoleMaxConcurrent int
	NodeAllowCIDRs       []netip.Prefix
	TrustedProxies       []netip.Prefix
}

type User struct {
	ID        uint64
	Username  string
	Role      string
	SessionID uint64
}

func Current(c *gin.Context) User { u, _ := c.Get("user"); v, _ := u.(User); return v }
func Admin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if Current(c).Role != "admin" {
			Fail(c, E(403, "forbidden", "administrator required"))
			return
		}
		c.Set("admin", true)
		c.Next()
	}
}
func DB(c *gin.Context, a *App) *gorm.DB {
	if v, ok := c.Get("transaction"); ok {
		return v.(*gorm.DB)
	}
	return a.DB.WithContext(c.Request.Context())
}

type App struct {
	DB           *gorm.DB
	Config       Config
	Location     *time.Location
	mu           sync.RWMutex
	jobs         map[string]JobHandler
	reconcilers  map[string]JobHandler
	pollers      []poller
	agentClients sync.Map
}

func New(db *gorm.DB, cfg Config) *App {
	return &App{DB: db, Config: cfg, Location: time.FixedZone("UTC+8", 8*3600), jobs: make(map[string]JobHandler)}
}
func (a *App) Time() time.Time { return time.Now().In(a.Location).Truncate(time.Microsecond) }
func (a *App) Encrypt(p []byte) ([]byte, error) {
	b, e := aes.NewCipher(a.Config.MasterKey)
	if e != nil {
		return nil, e
	}
	g, e := cipher.NewGCM(b)
	if e != nil {
		return nil, e
	}
	n := make([]byte, g.NonceSize())
	if _, e = rand.Read(n); e != nil {
		return nil, e
	}
	return g.Seal(n, n, p, []byte("101cloud.node-token.v1")), nil
}
func (a *App) Decrypt(p []byte) ([]byte, error) {
	b, e := aes.NewCipher(a.Config.MasterKey)
	if e != nil {
		return nil, e
	}
	g, e := cipher.NewGCM(b)
	if e != nil {
		return nil, e
	}
	if len(p) < g.NonceSize() {
		return nil, errors.New("invalid encrypted token")
	}
	return g.Open(nil, p[:g.NonceSize()], p[g.NonceSize():], []byte("101cloud.node-token.v1"))
}
func RandomToken() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func (a *App) Policy(ctx context.Context, kind string) (Row, error) {
	var rows []Row
	e := a.DB.WithContext(ctx).Raw("SELECT p.* FROM policy_versions p JOIN platform_policy_bindings b ON b.policy_id=p.id WHERE b.kind=? AND p.status='active' AND p.effective_at<=?", kind, a.Time()).Scan(&rows).Error
	if e != nil {
		return nil, WrapDB(e)
	}
	if len(rows) != 1 {
		return nil, E(409, "policy_not_configured", "published policy required: "+kind)
	}
	var cfg Row
	if e = json.Unmarshal([]byte(Text(rows[0]["config"])), &cfg); e != nil {
		return nil, E(500, "internal_error", "invalid stored policy")
	}
	return cfg, nil
}
func Audit(ctx context.Context, db *gorm.DB, actor, taskID, instanceID uint64, action string, details any) error {
	b, e := json.Marshal(details)
	if e != nil {
		return e
	}
	r := Row{"event_type": action, "resource_type": "instance", "resource_id": nullableID(instanceID), "actor_user_id": nullableID(actor), "task_id": nullableID(taskID), "result": "succeeded", "result_redacted": string(b), "occurred_at": time.Now().In(time.FixedZone("UTC+8", 28800))}
	if instanceID == 0 {
		r["resource_type"] = "control_plane"
	}
	return db.WithContext(ctx).Table("audit_logs").Create(&r).Error
}
func nullableID(v uint64) any {
	if v == 0 {
		return nil
	}
	return v
}
func LogError(where string, e error) { slog.Error(where, "error", fmt.Sprintf("%T", e)) }
