package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type imageInput struct {
	Code         string           `json:"code"`
	Version      string           `json:"version"`
	InstanceType string           `json:"instance_type"`
	SourceType   string           `json:"source_type"`
	Properties   map[string]any   `json:"properties"`
	Nodes        []ImageNodeInput `json:"nodes"`
}
type planInput struct {
	Code                  string              `json:"code"`
	Version               string              `json:"version"`
	Name                  string              `json:"name"`
	InstanceType          string              `json:"instance_type"`
	Resources             Resources           `json:"resources"`
	GPU                   GPURequest          `json:"gpu"`
	NodeIDs               []string            `json:"node_ids"`
	GPUPolicy             *planGPUPolicyInput `json:"gpu_policy"`
	AllowedUserActions    []string            `json:"allowed_user_actions"`
	GPUUserPermissions    map[string]bool     `json:"gpu_user_permissions"`
	ImageIDs              []string            `json:"image_ids"`
	NetworkOptionIDs      []string            `json:"network_option_ids"`
	AllowedOverrides      map[string]any      `json:"allowed_overrides"`
	PolicyVersions        map[string]string   `json:"policy_versions"`
	InitTemplateVersionID *string             `json:"init_template_version_id"`
}
type templateInput struct {
	Code            string         `json:"code"`
	Version         string         `json:"version"`
	Name            string         `json:"name"`
	Script          string         `json:"script"`
	ParameterSchema map[string]any `json:"parameter_schema"`
	TimeoutSeconds  uint64         `json:"timeout_seconds"`
	Phase           string         `json:"phase"`
}

func registerProducts(a *platform.App, user, admin *gin.RouterGroup) {
	user.GET("/catalog/plans", platform.Handle(func(c *gin.Context) (any, error) { return planList(c, a, false) }))
	user.GET("/catalog/plans/:plan_id", platform.Handle(func(c *gin.Context) (any, error) {
		id, e := platform.ID(c, "plan_id")
		if e != nil {
			return nil, e
		}
		r, e := platform.Get(platform.DB(c, a), "instance_plans", id, false)
		if e != nil {
			return nil, e
		}
		if platform.Text(r["status"]) != "active" {
			return nil, platform.E(404, "not_found", "plan not found")
		}
		platform.SetETag(c, r)
		return planDTO(platform.DB(c, a), r)
	}))
	user.GET("/catalog/images", platform.Handle(func(c *gin.Context) (any, error) { return imageList(c, a, false) }))
	user.GET("/catalog/gpu-options", platform.Handle(func(c *gin.Context) (any, error) {
		// Option IDs are gpu-pricing policy_versions IDs, the value accepted by plans and quotes.
		inner := platform.DB(c, a).Table("gpu_price_versions p").Select("pv.id,p.gpu_model AS model,p.access_mode,p.gpu_hourly_pt,p.created_at,(SELECT MAX(g.memory_mib) FROM node_gpus g WHERE g.gpu_price_version_id=p.id) AS memory_mib,CASE WHEN EXISTS (SELECT 1 FROM node_gpus g JOIN nodes n ON n.id=g.node_id WHERE g.gpu_price_version_id=p.id AND g.status='available' AND n.administrative_state='active' AND n.health='online') THEN 'available' ELSE 'unavailable' END AS availability").Joins("JOIN policy_versions pv ON pv.kind='gpu-pricing' AND pv.source_id=p.id").Where("p.status='active' AND p.effective_at<=?", a.Time())
		if s := c.Query("node_id"); s != "" {
			id, e := platform.IDString(s)
			if e != nil {
				return nil, e
			}
			inner = inner.Where("EXISTS (SELECT 1 FROM node_gpus g WHERE g.gpu_price_version_id=p.id AND g.node_id=?)", id)
		}
		return listProducts(c, a, platform.DB(c, a).Table("(?) AS options", inner), []string{"node_id"})
	}))
	admin.GET("/images", platform.Handle(func(c *gin.Context) (any, error) { return imageList(c, a, true) }))
	admin.POST("/images", platform.Handle(func(c *gin.Context) (any, error) { return imageCreate(c, a) }))
	admin.GET("/images/:id", platform.Handle(func(c *gin.Context) (any, error) {
		r, e := detail(c, a, "images")
		if e != nil {
			return nil, e
		}
		return imageWithNodes(platform.DB(c, a), r)
	}))
	admin.PATCH("/images/:id", platform.Handle(func(c *gin.Context) (any, error) {
		var b struct {
			Properties map[string]any `json:"properties"`
			Status     *string        `json:"status"`
		}
		if e := platform.Decode(c, &b); e != nil {
			return nil, e
		}
		r, e := detail(c, a, "images")
		if e != nil {
			return nil, e
		}
		ch := platform.Row{}
		if b.Properties != nil {
			if e := validateImageProperties(b.Properties); e != nil {
				return nil, e
			}
			ch["properties"] = jsonString(b.Properties)
		}
		if b.Status != nil {
			if !enum(*b.Status, "active", "disabled", "retired") {
				return nil, invalid("invalid status")
			}
			ch["status"] = *b.Status
		}
		r, e = platform.Save(c, a, "images", r, ch)
		if e != nil {
			return nil, e
		}
		return imageWithNodes(platform.DB(c, a), r)
	}))
	admin.GET("/plans", platform.Handle(func(c *gin.Context) (any, error) { return planList(c, a, true) }))
	admin.POST("/plans", platform.Handle(func(c *gin.Context) (any, error) {
		var b planInput
		if e := platform.Decode(c, &b); e != nil {
			return nil, e
		}
		row, e := planValues(c, a, b, false)
		if e != nil {
			return nil, e
		}
		r, e := insert(c, a, "instance_plans", row)
		if e != nil {
			return nil, e
		}
		if e = savePlanRelations(platform.DB(c, a), platform.Uint(r["id"]), b); e != nil {
			return nil, e
		}
		out, e := planDTO(platform.DB(c, a), r)
		if e != nil {
			return nil, e
		}
		platform.Reply(c, 201, out)
		return nil, nil
	}))
	admin.GET("/plans/:id", platform.Handle(func(c *gin.Context) (any, error) {
		r, e := detail(c, a, "instance_plans")
		if e != nil {
			return nil, e
		}
		return planDTO(platform.DB(c, a), r)
	}))
	admin.PATCH("/plans/:id", platform.Handle(func(c *gin.Context) (any, error) {
		var b map[string]any
		if e := platform.Decode(c, &b); e != nil {
			return nil, e
		}
		if e := platform.ValidateKeys(b, "code", "version", "name", "instance_type", "resources", "gpu", "node_ids", "gpu_policy", "allowed_user_actions", "gpu_user_permissions", "image_ids", "network_option_ids", "allowed_overrides", "policy_versions", "init_template_version_id"); e != nil {
			return nil, e
		}
		r, e := detail(c, a, "instance_plans")
		if e != nil {
			return nil, e
		}
		dto, e := planDTO(platform.DB(c, a), r)
		if e != nil {
			return nil, e
		}
		merged := platform.Row{}
		for _, k := range []string{"code", "version", "name", "instance_type", "resources", "gpu", "node_ids", "gpu_policy", "allowed_user_actions", "gpu_user_permissions", "image_ids", "network_option_ids", "allowed_overrides", "policy_versions", "init_template_version_id"} {
			merged[k] = dto[k]
		}
		for k, v := range b {
			merged[k] = v
		}
		var input planInput
		if e = decodeMap(merged, &input); e != nil {
			return nil, e
		}
		row, e := planValues(c, a, input, platform.Text(r["status"]) == "active")
		if e != nil {
			return nil, e
		}
		delete(row, "status")
		r, e = platform.Save(c, a, "instance_plans", r, row)
		if e != nil {
			return nil, e
		}
		if e = savePlanRelations(platform.DB(c, a), platform.Uint(r["id"]), input); e != nil {
			return nil, e
		}
		return planDTO(platform.DB(c, a), r)
	}))
	admin.POST("/plans/:id/enable", platform.Handle(func(c *gin.Context) (any, error) {
		if e := requiredBody(c); e != nil {
			return nil, e
		}
		r, e := detail(c, a, "instance_plans")
		if e != nil {
			return nil, e
		}
		if platform.Text(r["status"]) != "disabled" {
			return nil, conflict("only disabled plans can enable")
		}
		dto, e := planDTO(platform.DB(c, a), r)
		if e != nil {
			return nil, e
		}
		var b planInput
		fields := platform.Row{}
		for _, k := range []string{"code", "version", "name", "instance_type", "resources", "gpu", "node_ids", "gpu_policy", "allowed_user_actions", "gpu_user_permissions", "image_ids", "network_option_ids", "allowed_overrides", "policy_versions", "init_template_version_id"} {
			fields[k] = dto[k]
		}
		if e = decodeMap(fields, &b); e != nil {
			return nil, e
		}
		if _, e = planValues(c, a, b, true); e != nil {
			return nil, e
		}
		r, e = platform.Save(c, a, "instance_plans", r, platform.Row{"status": "active"})
		if e != nil {
			return nil, e
		}
		return planDTO(platform.DB(c, a), r)
	}))
	admin.POST("/plans/:id/disable", platform.Handle(func(c *gin.Context) (any, error) {
		if e := requiredBody(c); e != nil {
			return nil, e
		}
		r, e := detail(c, a, "instance_plans")
		if e != nil {
			return nil, e
		}
		if platform.Text(r["status"]) != "active" {
			return nil, conflict("only active plans can disable")
		}
		r, e = platform.Save(c, a, "instance_plans", r, platform.Row{"status": "disabled"})
		if e != nil {
			return nil, e
		}
		return planDTO(platform.DB(c, a), r)
	}))
	admin.GET("/init-templates", platform.Handle(func(c *gin.Context) (any, error) {
		return listDTO(c, a, platform.DB(c, a).Table("init_template_versions"), nil, templateDTO)
	}))
	admin.POST("/init-templates", platform.Handle(func(c *gin.Context) (any, error) {
		var b templateInput
		if e := platform.Decode(c, &b); e != nil {
			return nil, e
		}
		r, e := templateValues(b, platform.Current(c).ID)
		if e != nil {
			return nil, e
		}
		r, e = insert(c, a, "init_template_versions", r)
		if e != nil {
			return nil, e
		}
		platform.Reply(c, 201, templateDTO(r))
		return nil, nil
	}))
	admin.GET("/init-templates/:id", platform.Handle(func(c *gin.Context) (any, error) {
		r, e := detail(c, a, "init_template_versions")
		if e != nil {
			return nil, e
		}
		return templateDTO(r), nil
	}))
	admin.PATCH("/init-templates/:id", platform.Handle(func(c *gin.Context) (any, error) {
		var b map[string]any
		if e := platform.Decode(c, &b); e != nil {
			return nil, e
		}
		if e := platform.ValidateKeys(b, "name", "script", "parameter_schema", "timeout_seconds", "phase"); e != nil {
			return nil, e
		}
		r, e := detail(c, a, "init_template_versions")
		if e != nil {
			return nil, e
		}
		if platform.Text(r["status"]) != "draft" {
			return nil, conflict("only draft templates can change")
		}
		dto := templateDTO(r)
		fields := platform.Row{}
		for _, k := range []string{"code", "version", "name", "script", "parameter_schema", "timeout_seconds", "phase"} {
			fields[k] = dto[k]
		}
		for k, v := range b {
			fields[k] = v
		}
		var input templateInput
		if e = decodeMap(fields, &input); e != nil {
			return nil, e
		}
		row, e := templateValues(input, platform.Current(c).ID)
		if e != nil {
			return nil, e
		}
		delete(row, "status")
		delete(row, "created_by_user_id")
		r, e = platform.Save(c, a, "init_template_versions", r, row)
		if e != nil {
			return nil, e
		}
		return templateDTO(r), nil
	}))
	admin.POST("/init-templates/:id/publish", platform.Handle(func(c *gin.Context) (any, error) {
		if e := requiredBody(c); e != nil {
			return nil, e
		}
		r, e := detail(c, a, "init_template_versions")
		if e != nil {
			return nil, e
		}
		if platform.Text(r["status"]) != "draft" {
			return nil, conflict("only draft templates can publish")
		}
		if e = validateSchemaDefinition(object(r["parameter_schema"]), 0); e != nil {
			return nil, e
		}
		r, e = platform.Save(c, a, "init_template_versions", r, platform.Row{"status": "active"})
		if e != nil {
			return nil, e
		}
		return templateDTO(r), nil
	}))
	admin.POST("/init-templates/:id/retire", platform.Handle(func(c *gin.Context) (any, error) {
		r, e := retireProduct(c, a, "init_template_versions")
		if e != nil {
			return nil, e
		}
		return templateDTO(r), nil
	}))
}
func decodeMap(v any, dst any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return invalid("invalid fields")
	}
	if e = json.Unmarshal(b, dst); e != nil {
		return invalid("invalid fields")
	}
	return nil
}
func planList(c *gin.Context, a *platform.App, admin bool) (any, error) {
	db := platform.DB(c, a)
	q := db.Table("instance_plans")
	if !admin {
		q = q.Where("status='active'")
	}
	if s := c.Query("instance_type"); s != "" {
		if !enum(s, "container", "virtual-machine") {
			return nil, invalid("invalid instance type")
		}
		q = q.Where("image_type=?", s)
	}
	return platform.ListMap(c, a, q, []string{"instance_type"}, func(r platform.Row) (platform.Row, error) { return planDTO(db, r) })
}
func validateImageProperties(p map[string]any) error {
	if p == nil {
		return invalid("properties must be an object")
	}
	return secretFree(p)
}
func imageValues(b imageInput, actor uint64) (platform.Row, error) {
	if len(b.Nodes) == 0 {
		return nil, invalid("nodes must contain at least one target")
	}
	first := b.Nodes[0]
	fp, e := hex.DecodeString(first.Fingerprint)
	if e != nil || len(fp) != 32 {
		return nil, invalid("image fingerprint must be full SHA-256 hex")
	}
	defaultIdentity := b.Code == ""
	if b.Code == "" {
		b.Code = strings.ToLower(first.Fingerprint[:12])
	}
	if b.Version == "" {
		b.Version = "1"
	}
	if !codeValid(b.Code) || !codeValid(b.Version) || !enum(b.InstanceType, "container", "virtual-machine") || !textValid(first.Architecture, 32) || !enum(b.SourceType, "local", "remote") {
		return nil, invalid("invalid image identity/type/source/architecture")
	}
	if e = validateImageProperties(b.Properties); e != nil {
		return nil, e
	}
	if v, exists := b.Properties["gpu_ready"]; exists {
		if _, ok := v.(bool); !ok {
			return nil, invalid("properties.gpu_ready must be a boolean")
		}
	}
	osName, osRelease := platform.Text(b.Properties["os_name"]), platform.Text(b.Properties["os_release"])
	if !textValid(osName, 64) || !textValid(osRelease, 64) {
		return nil, invalid("properties.os_name and os_release required")
	}
	name := b.Code + " " + b.Version
	if label := osName + " " + osRelease + " (" + b.Code + ")"; defaultIdentity && utf8.RuneCountInString(label) <= 128 {
		name = label
	}
	r := platform.Row{"code": b.Code, "version": b.Version, "name": name, "source_type": b.SourceType, "fingerprint": fp, "architecture": first.Architecture, "image_type": b.InstanceType, "gpu_ready": first.GPUReady, "os_name": osName, "os_release": osRelease, "properties": jsonString(b.Properties), "status": "active", "created_by_user_id": actor}
	if b.SourceType == "remote" {
		sourceURL, ok := imageSourceURL(first.SourceID)
		if !ok {
			return nil, invalid("remote image source_id is required")
		}
		r["source_url"], r["source_protocol"] = sourceURL, "simplestreams"
	}
	return r, nil
}
func planValues(c *gin.Context, a *platform.App, b planInput, publish bool) (platform.Row, error) {
	if !codeValid(b.Code) || !codeValid(b.Version) || !textValid(b.Name, 128) || !enum(b.InstanceType, "container", "virtual-machine") {
		return nil, invalid("invalid plan identity/type")
	}
	if e := validResources(b.Resources); e != nil {
		return nil, e
	}
	if b.AllowedOverrides == nil {
		return nil, invalid("allowed_overrides must be an object")
	}
	if e := validateOverrideDefinition(b.AllowedOverrides, b.Resources, b.GPU); e != nil {
		return nil, e
	}
	if e := validPlanGPU(b.GPU, b.InstanceType, platform.Uint(object(b.AllowedOverrides["gpu_count"])["max"])); e != nil {
		return nil, e
	}
	pol, e := resolvePolicies(c.Request.Context(), platform.DB(c, a), a, b.PolicyVersions, publish)
	if e != nil {
		return nil, e
	}
	if b.GPU.Model != nil && pol["gpu-pricing"] == nil {
		return nil, invalid("GPU policy required")
	}
	policy, actions, permissions, e := planGPUConfiguration(b, pol)
	if e != nil {
		return nil, e
	}
	if e = validatePlanPlacement(c, a, b, publish); e != nil {
		return nil, e
	}
	var template any
	if b.InitTemplateVersionID != nil && *b.InitTemplateVersionID != "" {
		id, e := platform.IDString(*b.InitTemplateVersionID)
		if e != nil {
			return nil, e
		}
		r, e := platform.Get(platform.DB(c, a), "init_template_versions", id, true)
		if e != nil {
			return nil, e
		}
		if publish && platform.Text(r["status"]) != "active" {
			return nil, missingReference("template")
		}
		template = id
	}
	var model any
	if b.GPU.Model != nil {
		model = *b.GPU.Model
	}
	return platform.Row{"code": b.Code, "version": b.Version, "name": b.Name, "image_type": b.InstanceType, "vcpus": b.Resources.VCPUs, "memory_bytes": mustUnsigned(b.Resources.MemoryBytes), "disk_bytes": mustUnsigned(b.Resources.DiskBytes), "gpu_count": b.GPU.Count, "gpu_model": model, "gpu_access_mode": b.GPU.AccessMode, "gpu_max_hourly_pt": policy.Rebalance.MaxHourlyPT, "gpu_policy": jsonString(policy), "allowed_user_actions": jsonString(actions), "gpu_user_permissions": jsonString(permissions), "pricing_version_id": platform.Uint(pol["pricing"]["source_id"]), "credit_policy_version_id": platform.Uint(pol["credit"]["source_id"]), "idle_policy_version_id": platform.Uint(pol["gpu-idle"]["source_id"]), "init_template_version_id": template, "init_parameters": "{}", "allow_residency": false, "status": "disabled", "policy_versions": jsonString(b.PolicyVersions), "allowed_overrides": jsonString(b.AllowedOverrides)}, nil
}
func savePlanRelations(db *gorm.DB, id uint64, b planInput) error {
	for _, v := range []struct {
		table, col string
		ids        []string
	}{{"plan_nodes", "node_id", b.NodeIDs}, {"plan_images", "image_id", b.ImageIDs}, {"plan_network_options", "network_option_id", b.NetworkOptionIDs}} {
		if e := db.Table(v.table).Where("plan_id=?", id).Delete(&platform.Row{}).Error; e != nil {
			return platform.WrapDB(e)
		}
		for _, s := range v.ids {
			rid, _ := platform.IDString(s)
			row := platform.Row{"plan_id": id, v.col: rid}
			if e := db.Table(v.table).Create(&row).Error; e != nil {
				return platform.WrapDB(e)
			}
		}
	}
	return nil
}
func retireProduct(c *gin.Context, a *platform.App, table string) (platform.Row, error) {
	if e := platform.Decode(c, &struct{}{}); e != nil {
		return nil, e
	}
	r, e := detail(c, a, table)
	if e != nil {
		return nil, e
	}
	if platform.Text(r["status"]) != "active" {
		return nil, conflict("only active versions can retire")
	}
	return platform.Save(c, a, table, r, platform.Row{"status": "retired"})
}
func templateValues(b templateInput, actor uint64) (platform.Row, error) {
	if !codeValid(b.Code) || !codeValid(b.Version) || !textValid(b.Name, 128) || b.Script == "" || len(b.Script) > 262144 || b.TimeoutSeconds == 0 || b.TimeoutSeconds > 86400 || !enum(b.Phase, "cloud_init", "post_start") {
		return nil, invalid("invalid template identity/script/phase/timeout")
	}
	lower := strings.ToLower(b.Script)
	for _, s := range []string{"-----begin", "password=", "password:", "passwd:", "private_key", "token=", "secret="} {
		if strings.Contains(lower, s) {
			return nil, invalid("script may not contain embedded credentials")
		}
	}
	if e := validateSchemaDefinition(b.ParameterSchema, 0); e != nil {
		return nil, e
	}
	sum := sha256.Sum256([]byte(b.Script))
	return platform.Row{"code": b.Code, "version": b.Version, "name": b.Name, "phase": b.Phase, "script_content": b.Script, "script_sha256": sum[:], "parameter_schema": jsonString(b.ParameterSchema), "timeout_seconds": b.TimeoutSeconds, "status": "draft", "created_by_user_id": actor}, nil
}
func validateOverrideDefinition(m map[string]any, r Resources, g GPURequest) error {
	for k, v := range m {
		if !enum(k, "vcpus", "memory_bytes", "disk_bytes", "gpu_count") {
			return invalid("unknown allowed_overrides field")
		}
		b := object(v)
		if b == nil {
			return invalid("override boundary must be object")
		}
		if e := platform.ValidateKeys(b, "min", "max", "step"); e != nil {
			return e
		}
		lo, e := unsigned(platform.Text(b["min"]), k != "gpu_count")
		if e != nil {
			return e
		}
		hi, e := unsigned(platform.Text(b["max"]), k != "gpu_count")
		if e != nil {
			return e
		}
		step, e := unsigned(platform.Text(b["step"]), true)
		if e != nil {
			return e
		}
		if hi < lo || step == 0 {
			return invalid("invalid override range")
		}
		base := map[string]uint64{"vcpus": r.VCPUs, "memory_bytes": mustUnsigned(r.MemoryBytes), "disk_bytes": mustUnsigned(r.DiskBytes), "gpu_count": g.Count}[k]
		// A zero GPU base is the always-allowed opt-out, so it need not lie in the range.
		if k == "gpu_count" && base == 0 {
			continue
		}
		if base < lo || base > hi || (base-lo)%step != 0 {
			return invalid("base resource outside override range")
		}
	}
	return nil
}
func validateSchemaDefinition(s map[string]any, depth int) error {
	if s == nil || depth > 16 {
		return invalid("invalid parameter schema")
	}
	if e := secretFree(s); e != nil {
		return e
	}
	if e := platform.ValidateKeys(s, "type", "properties", "required", "additionalProperties", "enum", "minimum", "maximum", "minLength", "maxLength", "items", "maxItems", "description", "title"); e != nil {
		return e
	}
	typ := platform.Text(s["type"])
	if !enum(typ, "object", "string", "integer", "number", "boolean", "array") {
		return invalid("unsupported schema type")
	}
	if depth == 0 && typ != "object" {
		return invalid("parameter schema root must be object")
	}
	if typ == "object" {
		props := object(s["properties"])
		if props == nil {
			return invalid("object schema requires properties")
		}
		for _, v := range props {
			if e := validateSchemaDefinition(object(v), depth+1); e != nil {
				return e
			}
		}
		if b, ok := s["additionalProperties"].(bool); !ok || b {
			return invalid("additionalProperties must explicitly be false")
		}
		if req, ok := s["required"]; ok {
			xs, ok := req.([]any)
			if !ok {
				return invalid("required must be string array")
			}
			for _, x := range xs {
				if _, ok := props[platform.Text(x)]; !ok {
					return invalid("required parameter absent from properties")
				}
			}
		}
	}
	if typ == "array" {
		return validateSchemaDefinition(object(s["items"]), depth+1)
	}
	return nil
}
