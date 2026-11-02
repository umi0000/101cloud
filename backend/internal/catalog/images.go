package catalog

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"101cloud/backend/internal/platform"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ImageNodeInput describes one exact artifact in one Incus image project.
// The fingerprint is always full SHA-256; aliases are never persisted as identity.
type ImageNodeInput struct {
	NodeID       string         `json:"node_id"`
	ProjectName  string         `json:"project_name"`
	Architecture string         `json:"architecture"`
	Fingerprint  string         `json:"fingerprint"`
	CloudInit    bool           `json:"cloud_init"`
	GPUReady     bool           `json:"gpu_ready"`
	SourceID     string         `json:"source_id"`
	Variant      string         `json:"variant"`
	Properties   map[string]any `json:"properties"`
}

func imageSourceURL(id string) (string, bool) {
	switch id {
	case "images":
		return "https://images.linuxcontainers.org", true
	default:
		return "", false
	}
}

func imageSources() []platform.Row {
	return []platform.Row{
		{"id": "images", "name": "Incus / Linux Containers", "url": "https://images.linuxcontainers.org", "protocol": "simplestreams"},
	}
}

func imageFingerprint(v any) string {
	s := platform.Text(v)
	if len(s) == 32 {
		return hex.EncodeToString([]byte(s))
	}
	if b, e := hex.DecodeString(s); e == nil && len(b) == 32 {
		return strings.ToLower(s)
	}
	return ""
}

func imageNodeDTO(r platform.Row) platform.Row {
	p := platform.Public(r, "id", "image_id", "node_id", "project_name", "architecture", "source_id", "variant", "status", "task_id", "last_verified_at", "revision", "created_at", "updated_at")
	p["fingerprint"] = imageFingerprint(r["fingerprint"])
	p["cloud_init"], p["gpu_ready"] = platform.Bool(r["cloud_init"]), platform.Bool(r["gpu_ready"])
	p["properties"] = jsonValue(r["properties"])
	p["error"] = jsonValue(r["error"])
	return p
}

func imageNodeRows(db *gorm.DB, imageID uint64) ([]platform.Row, error) {
	var rows []platform.Row
	if e := db.Table("image_nodes").Where("image_id=?", imageID).Order("node_id,project_name").Find(&rows).Error; e != nil {
		return nil, platform.WrapDB(e)
	}
	out := make([]platform.Row, 0, len(rows))
	for _, r := range rows {
		out = append(out, imageNodeDTO(r))
	}
	return out, nil
}

func imageWithNodes(db *gorm.DB, r platform.Row) (platform.Row, error) {
	p := imageDTO(r)
	nodes, e := imageNodeRows(db, platform.Uint(r["id"]))
	if e != nil {
		return nil, e
	}
	p["nodes"] = nodes
	if _, effective := r["cloud_init"]; !effective {
		for _, n := range nodes {
			if n["fingerprint"] == p["fingerprint"] && n["architecture"] == p["architecture"] {
				p["cloud_init"] = n["cloud_init"]
				break
			}
		}
	}
	return p, nil
}

// ResolveNodeImage has no legacy/global fallback. Existing instances retain their
// frozen metadata; new admissions require native-verified readiness in the exact project.
func ResolveNodeImage(ctx context.Context, db *gorm.DB, image platform.Row, nodeID uint64, project string) (platform.Row, error) {
	var n platform.Row
	e := db.WithContext(ctx).Table("image_nodes").Where("image_id=? AND node_id=? AND project_name=? AND status='ready' AND last_verified_at IS NOT NULL", image["id"], nodeID, project).Take(&n).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		return nil, platform.E(409, "image_not_ready", "所选镜像尚未在目标节点和项目完成原生验证，请选择已就绪镜像。")
	}
	if e != nil {
		return nil, platform.WrapDB(e)
	}
	if imageFingerprint(n["fingerprint"]) == "" {
		return nil, platform.E(409, "image_not_ready", "节点镜像指纹无效，请管理员重新验证镜像。")
	}
	r := make(platform.Row, len(image)+4)
	for k, v := range image {
		r[k] = v
	}
	for _, k := range []string{"architecture", "fingerprint", "gpu_ready", "cloud_init"} {
		r[k] = n[k]
	}
	properties := make(map[string]any)
	for k, v := range object(image["properties"]) {
		properties[k] = v
	}
	for k, v := range object(n["properties"]) {
		properties[k] = v
	}
	properties["gpu_ready"], properties["cloud_init"] = platform.Bool(n["gpu_ready"]), platform.Bool(n["cloud_init"])
	r["properties"], r["image_node_id"] = jsonString(properties), n["id"]
	return r, nil
}

func registerImages(a *platform.App, admin *gin.RouterGroup) {
	a.RegisterJob("image.prepare", imagePrepareJob)
	a.RegisterReconciler("image.prepare", imagePrepareReconcileJob)
	admin.GET("/image-sources", platform.Handle(func(c *gin.Context) (any, error) { return platform.Row{"items": imageSources()}, nil }))
	admin.GET("/nodes/:id/remote-images", platform.Handle(func(c *gin.Context) (any, error) {
		id, e := platform.ID(c, "id")
		if e != nil {
			return nil, e
		}
		source := c.Query("source_id")
		if source == "" {
			source = "images"
		}
		if _, ok := imageSourceURL(source); !ok {
			return nil, invalid("source_id must be images")
		}
		query := url.Values{}
		allowed := map[string]bool{"source_id": true, "os": true, "release": true, "architecture": true, "instance_type": true, "cloud_init": true}
		for key, values := range c.Request.URL.Query() {
			if !allowed[key] || len(values) != 1 {
				return nil, invalid("invalid remote catalog query parameter")
			}
			if key != "source_id" {
				query.Set(key, values[0])
			}
		}
		if v := query.Get("cloud_init"); v != "" && v != "true" && v != "false" {
			return nil, invalid("cloud_init must be true or false")
		}
		if v := query.Get("instance_type"); v != "" && !enum(v, "container", "virtual-machine") {
			return nil, invalid("invalid instance_type")
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
		defer cancel()
		r, e := a.Agent(ctx, id, http.MethodGet, "/agent/v1/image-sources/"+source+"/images?"+query.Encode(), nil)
		if e != nil {
			return nil, platform.E(503, "image_catalog_unavailable", "节点无法读取可信镜像目录，请稍后重试或检查节点连接。")
		}
		items, ok := r["items"].([]any)
		if !ok {
			return nil, platform.E(503, "image_catalog_unavailable", "节点返回的镜像目录无效。")
		}
		return platform.Row{"items": items}, nil
	}))
	admin.GET("/images/:id/nodes", platform.Handle(func(c *gin.Context) (any, error) {
		r, e := detail(c, a, "images")
		if e != nil {
			return nil, e
		}
		items, e := imageNodeRows(platform.DB(c, a), platform.Uint(r["id"]))
		return platform.Row{"items": items}, e
	}))
	admin.POST("/images/:id/nodes", platform.Handle(func(c *gin.Context) (any, error) {
		var b struct {
			Nodes []ImageNodeInput `json:"nodes"`
		}
		if e := platform.Decode(c, &b); e != nil {
			return nil, e
		}
		id, e := platform.ID(c, "id")
		if e != nil {
			return nil, e
		}
		r, e := platform.Get(platform.DB(c, a), "images", id, true)
		if e != nil {
			return nil, e
		}
		if e = platform.Revision(c, r); e != nil {
			return nil, e
		}
		if e = validateImageNodes(c, a, r, b.Nodes); e != nil {
			return nil, e
		}
		tasks, e := saveImageNodes(c, a, r, b.Nodes)
		if e != nil {
			return nil, e
		}
		if _, e = platform.Save(c, a, "images", r, platform.Row{}); e != nil {
			return nil, e
		}
		platform.Reply(c, 202, platform.Row{"image_id": platform.Text(r["id"]), "tasks": tasks})
		return nil, nil
	}))
}

func imageCreate(c *gin.Context, a *platform.App) (any, error) {
	var b imageInput
	if e := platform.Decode(c, &b); e != nil {
		return nil, e
	}
	r, e := imageValues(b, platform.Current(c).ID)
	if e != nil {
		return nil, e
	}
	if e = validateImageNodes(c, a, r, b.Nodes); e != nil {
		return nil, e
	}
	r, e = insert(c, a, "images", r)
	if e != nil {
		return nil, e
	}
	if _, e = saveImageNodes(c, a, r, b.Nodes); e != nil {
		return nil, e
	}
	out, e := imageWithNodes(platform.DB(c, a), r)
	if e != nil {
		return nil, e
	}
	platform.SetETag(c, r)
	platform.Reply(c, 201, out)
	return nil, nil
}

func validateImageNodes(c *gin.Context, a *platform.App, image platform.Row, nodes []ImageNodeInput) error {
	if len(nodes) == 0 || len(nodes) > 100 {
		return invalid("nodes must contain 1-100 targets")
	}
	seen := make(map[string]bool, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		id, e := platform.IDString(n.NodeID)
		if e != nil {
			return e
		}
		n.NodeID = platform.Text(id)
		if _, e = platform.Get(platform.DB(c, a), "nodes", id, false); e != nil {
			return e
		}
		fp, e := hex.DecodeString(n.Fingerprint)
		if e != nil || len(fp) != 32 || !networkName(n.ProjectName) || !textValid(n.Architecture, 32) || len(n.Variant) > 128 {
			return invalid("each node requires an exact SHA-256 fingerprint, project and architecture")
		}
		n.Fingerprint = strings.ToLower(n.Fingerprint)
		key := platform.Text(id) + ":" + n.ProjectName
		if seen[key] {
			return invalid("duplicate node and project target")
		}
		seen[key] = true
		if n.Properties == nil {
			n.Properties = map[string]any{}
		}
		if e = validateImageProperties(n.Properties); e != nil {
			return e
		}
		if platform.Text(image["source_type"]) == "local" {
			if n.SourceID != "" {
				return invalid("local images must not specify source_id")
			}
			continue
		}
		if _, ok := imageSourceURL(n.SourceID); !ok {
			return invalid("remote node requires trusted source_id")
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
		remote, e := a.Agent(ctx, id, http.MethodGet, "/agent/v1/image-sources/"+n.SourceID+"/images/"+n.Fingerprint, nil)
		cancel()
		if e != nil {
			return platform.E(422, "image_selection_unverified", "所选镜像无法在该节点的可信远程目录验证，请重新加载目录。")
		}
		cloudInit, validCloudInit := remote["cloud_init"].(bool)
		if !validCloudInit {
			return invalid("remote catalog did not provide cloud-init capability evidence")
		}
		if n.Variant == "" {
			n.Variant = platform.Text(remote["variant"])
		}
		if imageFingerprint(remote["fingerprint"]) != n.Fingerprint || platform.Text(remote["os_name"]) != platform.Text(image["os_name"]) || platform.Text(remote["os_release"]) != platform.Text(image["os_release"]) || platform.Text(remote["instance_type"]) != platform.Text(image["image_type"]) || platform.Text(remote["architecture"]) != n.Architecture || cloudInit != n.CloudInit || platform.Text(remote["variant"]) != n.Variant {
			return invalid("remote artifact metadata does not match logical image and node configuration")
		}
	}
	return nil
}

func saveImageNodes(c *gin.Context, a *platform.App, image platform.Row, nodes []ImageNodeInput) ([]platform.Row, error) {
	db := platform.DB(c, a)
	tasks := make([]platform.Row, 0, len(nodes))
	for _, n := range nodes {
		nodeID := platform.Uint(n.NodeID)
		var old platform.Row
		e := db.Table("image_nodes").Clauses(clause.Locking{Strength: "UPDATE"}).Where("image_id=? AND node_id=? AND project_name=?", image["id"], nodeID, n.ProjectName).Take(&old).Error
		if e != nil && !errors.Is(e, gorm.ErrRecordNotFound) {
			return nil, platform.WrapDB(e)
		}
		if e == nil {
			switch platform.Text(old["status"]) {
			case "pending", "downloading", "unknown":
				return nil, platform.E(409, "image_preparation_active", "节点镜像正在准备或结果不确定，请先查看或协调原任务，勿重复派发。")
			}
			if platform.Uint(old["task_id"]) != 0 {
				prior, e := platform.Get(db, "tasks", platform.Uint(old["task_id"]), true)
				if e != nil {
					return nil, e
				}
				status := platform.Text(prior["status"])
				if status != "succeeded" && status != "failed" {
					return nil, platform.E(409, "image_preparation_active", "原镜像任务尚未安全结束，请先协调原任务。")
				}
			}
		}
		fp, _ := hex.DecodeString(n.Fingerprint)
		values := platform.Row{"image_id": image["id"], "node_id": nodeID, "project_name": n.ProjectName, "architecture": n.Architecture, "fingerprint": fp, "cloud_init": n.CloudInit, "gpu_ready": n.GPUReady, "source_id": nil, "variant": n.Variant, "properties": jsonString(n.Properties), "status": "pending", "task_id": nil, "last_verified_at": nil, "error": nil}
		if n.SourceID != "" {
			values["source_id"] = n.SourceID
		}
		var imageNodeID uint64
		if platform.Uint(old["id"]) != 0 {
			imageNodeID = platform.Uint(old["id"])
			values["revision"] = gorm.Expr("revision+1")
			if e = db.Table("image_nodes").Where("id=?", imageNodeID).Updates(values).Error; e != nil {
				return nil, platform.WrapDB(e)
			}
		} else {
			values["revision"] = 1
			if e = db.Table("image_nodes").Create(&values).Error; e != nil {
				return nil, platform.WrapDB(e)
			}
			imageNodeID = platform.Uint(values["id"])
			if imageNodeID == 0 {
				imageNodeID = platform.Uint(values["@id"])
			}
			if imageNodeID == 0 {
				return nil, errors.New("image node insert did not return identity")
			}
		}
		config := platform.Row{"image_id": platform.Text(image["id"]), "node_id": n.NodeID, "project_name": n.ProjectName, "architecture": n.Architecture, "fingerprint": n.Fingerprint, "instance_type": image["image_type"], "source_type": image["source_type"], "source_id": n.SourceID, "cloud_init": n.CloudInit, "gpu_ready": n.GPUReady, "variant": n.Variant}
		task, e := platform.Enqueue(db, "image.prepare", platform.Current(c).ID, nodeID, 0, false, platform.Row{"image_node_id": platform.Text(imageNodeID), "config": config})
		if e != nil {
			return nil, e
		}
		if e = db.Table("image_nodes").Where("id=?", imageNodeID).Update("task_id", platform.Uint(task["task_id"])).Error; e != nil {
			return nil, platform.WrapDB(e)
		}
		tasks = append(tasks, platform.Row{"node_id": n.NodeID, "project_name": n.ProjectName, "task_id": task["task_id"]})
	}
	return tasks, nil
}

func imageList(c *gin.Context, a *platform.App, admin bool) (any, error) {
	db := platform.DB(c, a)
	q := db.Table("images")
	if !admin {
		q = q.Where("images.status='active'")
	}
	var nodeID uint64
	var project string
	if s := c.Query("node_id"); s != "" {
		var e error
		nodeID, e = platform.IDString(s)
		if e != nil {
			return nil, e
		}
		node, e := platform.Get(db, "nodes", nodeID, false)
		if e != nil {
			return nil, e
		}
		project = platform.Text(node["default_project_name"])
		if v := c.Query("project_name"); v != "" {
			project = v
		}
		if !networkName(project) {
			return nil, invalid("invalid project_name")
		}
		q = q.Where("EXISTS (SELECT 1 FROM image_nodes ni WHERE ni.image_id=images.id AND ni.node_id=? AND ni.project_name=? AND ni.status='ready' AND ni.last_verified_at IS NOT NULL)", nodeID, project)
	} else if c.Query("project_name") != "" {
		return nil, invalid("project_name requires node_id")
	}
	if s := c.Query("instance_type"); s != "" {
		if !enum(s, "container", "virtual-machine") {
			return nil, invalid("invalid instance_type")
		}
		q = q.Where("images.image_type=?", s)
	}
	if s := c.Query("architecture"); s != "" {
		if nodeID != 0 {
			q = q.Where("EXISTS (SELECT 1 FROM image_nodes ni WHERE ni.image_id=images.id AND ni.node_id=? AND ni.project_name=? AND ni.status='ready' AND ni.architecture=?)", nodeID, project, s)
		} else {
			q = q.Where("images.architecture=?", s)
		}
	}
	return platform.ListMap(c, a, q, []string{"instance_type", "architecture", "node_id", "project_name"}, func(r platform.Row) (platform.Row, error) {
		if nodeID != 0 {
			var e error
			r, e = ResolveNodeImage(c.Request.Context(), db, r, nodeID, project)
			if e != nil {
				return nil, e
			}
		}
		return imageWithNodes(db, r)
	})
}
