package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"101cloud/backend/internal/auth"
	"101cloud/backend/internal/billing"
	"101cloud/backend/internal/catalog"
	"101cloud/backend/internal/instances"
	"101cloud/backend/internal/monitoring"
	"101cloud/backend/internal/platform"
	"101cloud/backend/internal/webui"
	"github.com/gin-gonic/gin"
)

func Router(a *platform.App) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	// X-Forwarded-For is honored only when the TCP peer is a configured proxy;
	// X-Real-IP is ignored so a single proxy contract applies.
	var proxies []string
	for _, p := range a.Config.TrustedProxies {
		proxies = append(proxies, p.String())
	}
	r.RemoteIPHeaders = []string{"X-Forwarded-For"}
	if e := r.SetTrustedProxies(proxies); e != nil {
		panic("trusted proxies were validated at config load: " + e.Error())
	}
	r.Use(platform.RequestMiddleware())
	r.Use(gin.CustomRecovery(func(c *gin.Context, _ any) { platform.Fail(c, platform.E(500, "internal_error", "request failed")) }))
	api := r.Group("/api/v1")
	api.Use(auth.CSRF(a))
	api.GET("/health/live", func(c *gin.Context) { platform.Reply(c, 200, platform.Row{"status": "ok"}) })
	api.GET("/health/ready", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		db, e := a.DB.DB()
		if e != nil || db.PingContext(ctx) != nil {
			platform.Fail(c, platform.E(503, "not_ready", "database unavailable"))
			return
		}
		platform.Reply(c, 200, platform.Row{"status": "ready"})
	})
	user := api.Group("")
	user.Use(auth.Middleware(a))
	admin := user.Group("/admin")
	admin.Use(platform.Admin(), a.Mutations())
	user.Use(a.Mutations())
	auth.Register(a, api, user, admin)
	billing.Register(a, user, admin)
	catalog.Register(a, user, admin)
	instances.Register(a, user, admin)
	monitoring.Register(a, user, admin)
	frontend := webui.Handler()
	r.NoRoute(func(c *gin.Context) {
		if c.Request.URL.Path == "/api" || strings.HasPrefix(c.Request.URL.Path, "/api/") {
			platform.Fail(c, platform.E(http.StatusNotFound, "not_found", "route not found"))
			return
		}
		frontend.ServeHTTP(c.Writer, c.Request)
	})
	r.NoMethod(func(c *gin.Context) {
		platform.Fail(c, platform.E(http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed"))
	})
	r.HandleMethodNotAllowed = true
	return r
}
