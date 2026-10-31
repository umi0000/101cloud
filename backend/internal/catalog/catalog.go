package catalog

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func Register(a *platform.App, user, admin *gin.RouterGroup) {
	registerProducts(a, user, admin)
	registerNodes(a, admin)
	registerNetworks(a, user, admin)
	user.POST("/quotes", platform.Handle(func(c *gin.Context) (any, error) {
		var s CreateElasticSpec
		if err := platform.Decode(c, &s); err != nil {
			return nil, err
		}
		q, e := Quote(c.Request.Context(), platform.DB(c, a), a, s)
		if e != nil {
			return nil, e
		}
		if e = quoteUserEligibility(c.Request.Context(), platform.DB(c, a), a, platform.Current(c).ID, s, q); e != nil {
			return nil, e
		}
		return q, nil
	}))
}
func invalid(message string) error  { return platform.E(422, "validation_failed", message) }
func conflict(message string) error { return platform.E(409, "conflict", message) }
func textValid(s string, n int) bool {
	return strings.TrimSpace(s) != "" && utf8.RuneCountInString(s) <= n
}

var codePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

func codeValid(s string) bool { return codePattern.MatchString(s) }
func jsonValue(v any) any {
	switch x := v.(type) {
	case []byte:
		var o any
		if json.Unmarshal(x, &o) == nil {
			return o
		}
	case string:
		var o any
		if json.Unmarshal([]byte(x), &o) == nil {
			return o
		}
	}
	return v
}
func object(v any) map[string]any { m, _ := jsonValue(v).(map[string]any); return m }
func jsonString(v any) string     { b, _ := json.Marshal(v); return string(b) }
func unsigned(s string, positive bool) (uint64, error) {
	n, e := strconv.ParseUint(s, 10, 64)
	if e != nil || s == "" || (positive && n == 0) {
		return 0, invalid("capacity must be an unsigned decimal integer string")
	}
	return n, nil
}
func rowID(row platform.Row) string { return strconv.FormatUint(platform.Uint(row["id"]), 10) }
func insert(c *gin.Context, a *platform.App, table string, row platform.Row) (platform.Row, error) {
	if e := platform.DB(c, a).Table(table).Create(&row).Error; e != nil {
		return nil, platform.WrapDB(e)
	}
	id := platform.Uint(row["id"])
	if id == 0 {
		if e := platform.DB(c, a).Raw("SELECT LAST_INSERT_ID() AS id").Scan(&row).Error; e != nil {
			return nil, platform.WrapDB(e)
		}
		id = platform.Uint(row["id"])
	}
	return platform.Get(platform.DB(c, a), table, id, false)
}
func detail(c *gin.Context, a *platform.App, table string) (platform.Row, error) {
	id, e := platform.ID(c, "id")
	if e != nil {
		return nil, e
	}
	r, e := platform.Get(platform.DB(c, a), table, id, false)
	if e == nil {
		platform.SetETag(c, r)
	}
	return r, e
}
func listProducts(c *gin.Context, a *platform.App, q *gorm.DB, filters []string) (any, error) {
	return platform.List(c, a, q, filters)
}

// listDTO pages full rows through the resource's detail DTO.
func listDTO(c *gin.Context, a *platform.App, q *gorm.DB, filters []string, dto func(platform.Row) platform.Row) (any, error) {
	return platform.ListMap(c, a, q, filters, func(r platform.Row) (platform.Row, error) { return dto(r), nil })
}
func imageDTO(r platform.Row) platform.Row {
	p := platform.Public(r, "id", "code", "version", "name", "architecture", "os_name", "os_release", "status", "revision", "created_at", "updated_at")
	p["gpu_ready"] = platform.Bool(r["gpu_ready"])
	p["instance_type"] = platform.Text(r["image_type"])
	p["fingerprint"] = hex.EncodeToString([]byte(platform.Text(r["fingerprint"])))
	p["properties"] = jsonValue(r["properties"])
	return p
}
func templateDTO(r platform.Row) platform.Row {
	p := platform.Public(r, "id", "code", "version", "name", "phase", "timeout_seconds", "status", "revision", "created_at", "updated_at")
	p["script"] = platform.Text(r["script_content"])
	p["parameter_schema"] = jsonValue(r["parameter_schema"])
	return p
}
func planDTO(db *gorm.DB, r platform.Row) (platform.Row, error) {
	p := platform.Public(r, "id", "code", "version", "name", "status", "init_template_version_id", "revision", "created_at", "updated_at")
	pricing, e := platform.PolicyID(db, "pricing", r["pricing_version_id"])
	if e != nil {
		return nil, e
	}
	p["pricing_version_id"] = pricing
	p["instance_type"] = platform.Text(r["image_type"])
	p["resources"] = Resources{VCPUs: platform.Uint(r["vcpus"]), MemoryBytes: platform.Text(r["memory_bytes"]), DiskBytes: platform.Text(r["disk_bytes"])}
	var model *string
	if platform.Text(r["gpu_model"]) != "" {
		s := platform.Text(r["gpu_model"])
		model = &s
	}
	p["gpu"] = GPURequest{Count: platform.Uint(r["gpu_count"]), Model: model, AccessMode: platform.Text(r["gpu_access_mode"]), MaxHourlyPT: platform.Text(r["gpu_max_hourly_pt"])}
	p["allowed_overrides"] = jsonValue(r["allowed_overrides"])
	p["policy_versions"] = jsonValue(r["policy_versions"])
	for _, v := range []struct{ table, col, key string }{{"plan_images", "image_id", "image_ids"}, {"plan_network_options", "network_option_id", "network_option_ids"}} {
		var ids []uint64
		if e := db.Table(v.table).Where("plan_id = ?", r["id"]).Order(v.col).Pluck(v.col, &ids).Error; e != nil {
			return nil, platform.WrapDB(e)
		}
		out := make([]string, len(ids))
		for i, id := range ids {
			out[i] = strconv.FormatUint(id, 10)
		}
		p[v.key] = out
	}
	return p, nil
}
func secretFree(v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return invalid("invalid JSON")
	}
	s := strings.ToLower(string(b))
	for _, bad := range []string{"-----begin", "private_key", "privatekey", "password", "passwd", "secret", "token", "ssh_authorized_keys", "user-data", "cloud-init"} {
		if strings.Contains(s, bad) {
			return invalid("credentials, keys and scripts are not permitted in parameters/properties")
		}
	}
	return nil
}
func requiredBody(c *gin.Context) error { var v struct{}; return platform.Decode(c, &v) }
func enum(s string, values ...string) bool {
	for _, v := range values {
		if s == v {
			return true
		}
	}
	return false
}
func missingReference(kind string) error {
	return invalid(fmt.Sprintf("invalid or unpublished %s reference", kind))
}
