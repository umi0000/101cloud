package catalog

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

const imagePrepareConfigKey = "image.prepare.config"
const imagePrepareNativeKey = "image.prepare.native"

type imagePrepareRootKey struct{}

func imagePrepareRootID(ctx context.Context, j platform.Job) uint64 {
	if id, ok := ctx.Value(imagePrepareRootKey{}).(uint64); ok {
		return id
	}
	return j.ID
}
func imagePrepareReconcileJob(ctx context.Context, a *platform.App, j platform.Job) error {
	var input map[string]any
	decoder := json.NewDecoder(bytes.NewReader(j.Parameters))
	decoder.UseNumber()
	if err := decoder.Decode(&input); err != nil {
		return platform.Unknown(err)
	}
	if _, wrapped := input["source_task_id"]; !wrapped {
		return imageRunPrepare(ctx, a, j)
	}
	rootID, err := platform.IDString(platform.Text(input["source_task_id"]))
	if err != nil {
		return platform.Unknown(err)
	}
	root, err := platform.Get(a.DB.WithContext(ctx), "tasks", rootID, false)
	if err != nil {
		return platform.Unknown(err)
	}
	if platform.Text(root["task_type"]) != "image.prepare" || platform.Uint(root["node_id"]) != j.NodeID || platform.Uint(root["user_id"]) != j.UserID || platform.Text(root["status"]) != "unknown" {
		return platform.Unknown(errors.New("image reconciliation source identity changed"))
	}
	j.Parameters = json.RawMessage(platform.Text(root["parameters_redacted"]))
	return imageRunPrepare(context.WithValue(ctx, imagePrepareRootKey{}, rootID), a, j)
}

// The first journal entry freezes selection identity independently of mutable
// catalog rows. The native entry is a write-ahead dispatch journal, not a retry
// counter: all lease generations inspect the same entry before doing native work.
type imagePrepareConfig struct {
	ImageID        string `json:"image_id"`
	NodeID         string `json:"node_id"`
	Project        string `json:"project_name"`
	Fingerprint    string `json:"fingerprint"`
	Architecture   string `json:"architecture"`
	InstanceType   string `json:"instance_type"`
	SourceType     string `json:"source_type"`
	SourceID       string `json:"source_id"`
	CloudInit      bool   `json:"cloud_init"`
	GPUReady       bool   `json:"gpu_ready"`
	Variant        string `json:"variant"`
	InferCloudInit bool   `json:"infer_cloud_init,omitempty"`
}

type imagePrepareInput struct {
	ImageNodeID string              `json:"image_node_id"`
	Config      *imagePrepareConfig `json:"config"`
}

func imagePrepareJob(ctx context.Context, a *platform.App, j platform.Job) error {
	return imageRunPrepare(ctx, a, j)
}

// Lock the task before the selection on every journal/state write. A superseded
// task or expired lease cannot publish readiness, persist intent, or adopt an op.
func imagePrepareFence(ctx context.Context, a *platform.App, j platform.Job, id uint64, fn func(*gorm.DB, platform.Row) error) error {
	return a.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var task platform.Row
		if err := tx.Raw("SELECT id FROM tasks WHERE id=? AND lease_generation=? AND status='running' AND lease_until>? FOR UPDATE", j.ID, j.LeaseGeneration, a.Time()).Scan(&task).Error; err != nil {
			return err
		}
		if platform.Uint(task["id"]) != j.ID {
			return errors.New("lost image preparation task lease")
		}
		rootID := imagePrepareRootID(ctx, j)
		if rootID != j.ID {
			var root platform.Row
			if err := tx.Raw("SELECT * FROM tasks WHERE id=? FOR UPDATE", rootID).Scan(&root).Error; err != nil {
				return err
			}
			if platform.Text(root["status"]) != "unknown" || platform.Text(root["lease_owner"]) != "reconcile:"+strconv.FormatUint(j.ID, 10) {
				return errors.New("lost image reconciliation source ownership")
			}
		}
		var row platform.Row
		if err := tx.Raw("SELECT * FROM image_nodes WHERE id=? FOR UPDATE", id).Scan(&row).Error; err != nil {
			return err
		}
		if platform.Uint(row["id"]) != id || platform.Uint(row["task_id"]) != rootID || platform.Uint(row["node_id"]) != j.NodeID {
			return platform.E(409, "image_task_superseded", "镜像节点配置已被其他任务替代。")
		}
		return fn(tx, row)
	})
}

func imagePrepareIdentity(row, image platform.Row) imagePrepareConfig {
	source := platform.Text(row["source_id"])
	sourceType := "local"
	if source != "" {
		sourceType = "remote"
	}
	return imagePrepareConfig{
		ImageID: strconv.FormatUint(platform.Uint(row["image_id"]), 10),
		NodeID:  strconv.FormatUint(platform.Uint(row["node_id"]), 10),
		Project: platform.Text(row["project_name"]), Fingerprint: imageFingerprint(row["fingerprint"]),
		Architecture: platform.Text(row["architecture"]), InstanceType: platform.Text(image["image_type"]),
		SourceType: sourceType, SourceID: source, CloudInit: platform.Bool(row["cloud_init"]),
		GPUReady: platform.Bool(row["gpu_ready"]), Variant: platform.Text(row["variant"]),
	}
}

func imagePrepareInitialize(ctx context.Context, a *platform.App, j platform.Job, in imagePrepareInput, id uint64) (imagePrepareConfig, platform.Row, error) {
	var config imagePrepareConfig
	var native platform.Row
	err := imagePrepareFence(ctx, a, j, id, func(tx *gorm.DB, row platform.Row) error {
		image, err := platform.Get(tx, "images", platform.Uint(row["image_id"]), false)
		if err != nil {
			return err
		}
		current := imagePrepareIdentity(row, image)
		if in.Config == nil {
			var properties map[string]any
			if err := json.Unmarshal([]byte(platform.Text(image["properties"])), &properties); err != nil {
				return err
			}
			_, explicit := properties["cloud_init"]
			current.InferCloudInit = !explicit
		}
		config = current
		var journal platform.Row
		res := tx.Table("task_steps").Where("task_id=? AND step_key=? AND attempt=1", imagePrepareRootID(ctx, j), imagePrepareConfigKey).Take(&journal)
		if res.Error == nil {
			if err = json.Unmarshal([]byte(platform.Text(journal["parameters_redacted"])), &config); err != nil {
				return err
			}
		} else if !errors.Is(res.Error, gorm.ErrRecordNotFound) {
			return res.Error
		}
		// Native-derived legacy capability is evidence, not a mutation of the
		// immutable requested identity. Reconciles compare against that evidence.
		var evidence map[string]any
		if config.InferCloudInit && json.Unmarshal([]byte(platform.Text(journal["result_redacted"])), &evidence) == nil {
			if cloud, ok := evidence["native_cloud_init"].(bool); ok && current.CloudInit == cloud {
				current.CloudInit = config.CloudInit
			}
		}
		if config != current {
			return platform.E(409, "image_config_changed", "镜像节点配置与任务的不可变配置不一致。")
		}
		if in.Config != nil {
			input := *in.Config
			if input.SourceType == "" {
				input.SourceType = current.SourceType
			}
			if input != config {
				return platform.E(409, "image_config_changed", "镜像任务配置与节点登记配置不一致。")
			}
		}
		fp, err := hex.DecodeString(config.Fingerprint)
		if err != nil || len(fp) != 32 || config.Project == "" || config.Architecture == "" || (config.InstanceType != "container" && config.InstanceType != "virtual-machine") {
			return platform.E(422, "invalid_image_config", "镜像任务配置不完整。")
		}
		if config.SourceID != "" {
			if _, ok := imageSourceURL(config.SourceID); !ok {
				return platform.E(422, "invalid_image_source", "镜像来源不在受信任列表中。")
			}
		}
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			encoded, err := json.Marshal(config)
			if err != nil {
				return err
			}
			entry := platform.Row{"task_id": imagePrepareRootID(ctx, j), "step_key": imagePrepareConfigKey, "attempt": 1, "status": "succeeded", "parameters_redacted": string(encoded), "started_at": a.Time(), "finished_at": a.Time()}
			if err = tx.Table("task_steps").Create(&entry).Error; err != nil {
				return err
			}
		}
		res = tx.Table("task_steps").Where("task_id=? AND step_key=? AND attempt=1", imagePrepareRootID(ctx, j), imagePrepareNativeKey).Take(&native)
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			native = nil
			return nil
		}
		if res.Error == nil && (platform.Uint(native["node_id"]) != j.NodeID || platform.Text(native["project_name"]) != config.Project) {
			return errors.New("image operation journal context mismatch")
		}
		return res.Error
	})
	return config, native, err
}

func imageRunPrepare(ctx context.Context, a *platform.App, j platform.Job) error {
	var in imagePrepareInput
	if err := json.Unmarshal(j.Parameters, &in); err != nil {
		return platform.Unknown(err)
	}
	id, err := platform.IDString(in.ImageNodeID)
	if err != nil {
		return platform.Unknown(err)
	}
	config, native, err := imagePrepareInitialize(ctx, a, j, in, id)
	if err != nil {
		return platform.Unknown(err)
	}
	var nativeCloudInit *bool
	finish := func(status string, cause error) error {
		return imagePrepareFinish(ctx, a, j, id, status, cause, nativeCloudInit)
	}
	exists, err := imagePrepareInspect(ctx, a, j.NodeID, config, &nativeCloudInit)
	if err != nil {
		if imagePrepareMismatch(err) {
			return finish("failed", err)
		}
		return finish("unknown", err)
	}
	if exists {
		return finish("ready", nil)
	}
	if config.SourceID == "" {
		return finish("failed", platform.E(422, "image_missing", "节点项目中不存在登记的本地镜像，请先上传该镜像。"))
	}
	operation := platform.Text(native["operation_id"])
	if native != nil && operation == "" {
		// Intent may have reached Incus even when the response was lost. Only
		// exact native evidence can resolve it; absence is never permission to POST.
		operation, err = imagePrepareFindOperation(ctx, a, j.NodeID, config)
		if err != nil || operation == "" {
			if err == nil {
				err = platform.E(409, "image_download_unknown", "镜像下载结果待核对；未找到可确认的原始操作，请勿重复下载。")
			}
			return finish("unknown", err)
		}
		if err = imagePrepareJournal(ctx, a, j, id, config, operation, false); err != nil {
			return platform.Unknown(err)
		}
	}
	if native == nil {
		if err = imagePrepareJournal(ctx, a, j, id, config, "", true); err != nil {
			return platform.Unknown(err)
		}
		server, _ := imageSourceURL(config.SourceID)
		response, postErr := a.Agent(ctx, j.NodeID, http.MethodPost, "/1.0/images?project="+url.QueryEscape(config.Project), platform.Row{
			"public": false, "source": platform.Row{"type": "image", "mode": "pull", "server": server, "protocol": "simplestreams", "fingerprint": config.Fingerprint},
		})
		if postErr != nil {
			return finish("unknown", postErr)
		}
		operation, err = imagePrepareOperationID(platform.Text(response["operation"]))
		if err != nil {
			return finish("unknown", err)
		}
		if err = imagePrepareJournal(ctx, a, j, id, config, operation, false); err != nil {
			return platform.Unknown(err)
		}
	}
	if err = imagePreparePoll(ctx, a, j, id, config, operation); err != nil {
		if imagePrepareOperationFailed(err) {
			return finish("failed", err)
		}
		return finish("unknown", err)
	}
	// An operation's success is not proof that the selected artifact exists.
	exists, err = imagePrepareInspect(ctx, a, j.NodeID, config, &nativeCloudInit)
	if err != nil {
		if imagePrepareMismatch(err) {
			return finish("failed", err)
		}
		return finish("unknown", err)
	}
	if !exists {
		return finish("unknown", platform.E(409, "image_download_unknown", "下载操作已结束，但尚未确认节点中的目标镜像。"))
	}
	return finish("ready", nil)
}

func imagePrepareInspect(ctx context.Context, a *platform.App, node uint64, config imagePrepareConfig, nativeCloudInit **bool) (bool, error) {
	response, err := a.Agent(ctx, node, http.MethodGet, "/1.0/images/"+config.Fingerprint+"?project="+url.QueryEscape(config.Project), nil)
	if platform.Uint(response["error_code"]) == 404 {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	metadata, ok := response["metadata"].(map[string]any)
	if !ok || platform.Uint(response["error_code"]) != 0 {
		return false, errors.New("missing native image metadata")
	}
	if platform.Text(metadata["fingerprint"]) == "" || platform.Text(metadata["architecture"]) == "" || platform.Text(metadata["type"]) == "" {
		return false, errors.New("incomplete native image identity")
	}
	if platform.Text(metadata["fingerprint"]) != config.Fingerprint || platform.Text(metadata["architecture"]) != config.Architecture || platform.Text(metadata["type"]) != config.InstanceType {
		return false, platform.E(422, "image_identity_mismatch", "节点镜像的完整指纹、架构或实例类型与登记配置不一致。")
	}
	if config.InferCloudInit {
		properties, _ := metadata["properties"].(map[string]any)
		if variant := platform.Text(properties["variant"]); variant != "" {
			cloud := variant == "cloud"
			*nativeCloudInit = &cloud
		}
	}
	return true, nil
}

func imagePrepareMismatch(err error) bool {
	var e *platform.HTTPError
	return errors.As(err, &e) && e.Code == "image_identity_mismatch"
}

func imagePrepareOperationFailed(err error) bool {
	var e *platform.HTTPError
	return errors.As(err, &e) && e.Code == "image_operation_failed"
}

func imagePrepareOperationID(reference string) (string, error) {
	u, err := url.Parse(reference)
	if err != nil || u.IsAbs() || u.Host != "" || u.Fragment != "" || !strings.HasPrefix(u.Path, "/1.0/operations/") {
		return "", errors.New("invalid image operation reference")
	}
	id := strings.TrimPrefix(u.Path, "/1.0/operations/")
	if id == "" || len(id) > 128 || strings.ContainsAny(id, "/\\\r\n") {
		return "", errors.New("invalid image operation id")
	}
	return id, nil
}

func imagePrepareJournal(ctx context.Context, a *platform.App, j platform.Job, id uint64, config imagePrepareConfig, operation string, dispatch bool) error {
	return imagePrepareFence(ctx, a, j, id, func(tx *gorm.DB, row platform.Row) error {
		if dispatch {
			entry := platform.Row{"task_id": imagePrepareRootID(ctx, j), "step_key": imagePrepareNativeKey, "attempt": 1, "node_id": j.NodeID, "project_name": config.Project, "status": "running", "parameters_redacted": "{}", "started_at": a.Time()}
			if err := tx.Table("task_steps").Create(&entry).Error; err != nil {
				return err
			}
		} else {
			res := tx.Table("task_steps").Where("task_id=? AND step_key=? AND attempt=1 AND (operation_id IS NULL OR operation_id=?)", imagePrepareRootID(ctx, j), imagePrepareNativeKey, operation).Updates(platform.Row{"operation_id": operation, "project_name": config.Project, "node_id": j.NodeID, "status": "waiting", "finished_at": nil})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return errors.New("image operation journal changed")
			}
		}
		if err := tx.Table("tasks").Where("id=?", j.ID).Update("phase", imagePrepareNativeKey).Error; err != nil {
			return err
		}
		return tx.Table("image_nodes").Where("id=? AND task_id=?", id, imagePrepareRootID(ctx, j)).Updates(platform.Row{"status": "downloading", "error": nil, "revision": gorm.Expr("revision+1")}).Error
	})
}

func imagePrepareFindOperation(ctx context.Context, a *platform.App, node uint64, config imagePrepareConfig) (string, error) {
	response, err := a.Agent(ctx, node, http.MethodGet, "/1.0/operations?recursion=1&project="+url.QueryEscape(config.Project), nil)
	if err != nil {
		return "", err
	}
	groups, ok := response["metadata"].(map[string]any)
	if !ok {
		return "", errors.New("missing native operation inventory")
	}
	found := ""
	for _, values := range groups {
		operations, ok := values.([]any)
		if !ok {
			return "", errors.New("invalid native operation inventory")
		}
		for _, value := range operations {
			op, ok := value.(map[string]any)
			if !ok {
				return "", errors.New("invalid native operation")
			}
			if project := platform.Text(op["project"]); project != "" && project != config.Project {
				continue
			}
			matched := false
			resources, _ := op["resources"].(map[string]any)
			images, _ := resources["images"].([]any)
			for _, image := range images {
				u, parseErr := url.Parse(platform.Text(image))
				if parseErr == nil && !u.IsAbs() && u.Path == "/1.0/images/"+config.Fingerprint && (u.Query().Get("project") == "" || u.Query().Get("project") == config.Project) {
					matched = true
				}
			}
			if !matched {
				continue
			}
			id, parseErr := imagePrepareOperationID("/1.0/operations/" + platform.Text(op["id"]))
			if parseErr != nil {
				return "", parseErr
			}
			if found != "" && found != id {
				return "", errors.New("multiple matching native image operations")
			}
			found = id
		}
	}
	return found, nil
}

func imagePreparePoll(ctx context.Context, a *platform.App, j platform.Job, id uint64, config imagePrepareConfig, operation string) error {
	parsed, err := imagePrepareOperationID("/1.0/operations/" + operation)
	if err != nil {
		return err
	}
	if parsed != operation {
		return errors.New("noncanonical image operation id")
	}
	path := "/1.0/operations/" + url.PathEscape(operation) + "?project=" + url.QueryEscape(config.Project)
	for {
		// Do not continue native reads after a task is superseded or its lease expires.
		if err := imagePrepareFence(ctx, a, j, id, func(*gorm.DB, platform.Row) error { return nil }); err != nil {
			return err
		}
		response, err := a.Agent(ctx, j.NodeID, http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		if platform.Uint(response["error_code"]) != 0 {
			return errors.New("native image operation read failed")
		}
		metadata, ok := response["metadata"].(map[string]any)
		if !ok {
			return errors.New("missing image download operation state")
		}
		status := platform.Uint(metadata["status_code"])
		if status == 0 {
			return errors.New("missing image download operation status")
		}
		detail, _ := metadata["metadata"].(map[string]any)
		progress := platform.Text(detail["download_progress"])
		if len(progress) > 512 {
			progress = progress[:512]
		}
		if err = a.TaskResult(ctx, j, platform.Row{"image_node_id": strconv.FormatUint(id, 10), "node_id": config.NodeID, "project_name": config.Project, "operation_id": operation, "status": "downloading", "progress": progress}); err != nil {
			return err
		}
		if status == 200 {
			return nil
		}
		if status == 400 || status == 401 {
			return platform.E(409, "image_operation_failed", "节点镜像下载操作失败，可检查节点后显式重试。")
		}
		if status >= 200 {
			return errors.New("unrecognized native image operation status")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func imagePrepareFinish(ctx context.Context, a *platform.App, j platform.Job, id uint64, status string, cause error, nativeCloudInit *bool) error {
	if ctx.Err() != nil {
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		ctx = finishCtx
	}
	err := imagePrepareFence(ctx, a, j, id, func(tx *gorm.DB, row platform.Row) error {
		changes := platform.Row{"status": status, "error": nil, "revision": gorm.Expr("revision+1")}
		result := platform.Row{"image_node_id": strconv.FormatUint(id, 10), "node_id": strconv.FormatUint(j.NodeID, 10), "status": status, "error": nil}
		if cause != nil {
			public := platform.PublicError(cause, status == "unknown")
			encoded, err := json.Marshal(platform.Row{"code": public["code"], "message": public["message"]})
			if err != nil {
				return err
			}
			changes["error"] = string(encoded)
			result["error"] = public
		}
		if status == "ready" {
			changes["last_verified_at"] = a.Time()
			if nativeCloudInit != nil {
				changes["cloud_init"] = *nativeCloudInit
				evidence, err := json.Marshal(platform.Row{"native_cloud_init": *nativeCloudInit})
				if err != nil {
					return err
				}
				if err = tx.Table("task_steps").Where("task_id=? AND step_key=? AND attempt=1", imagePrepareRootID(ctx, j), imagePrepareConfigKey).Update("result_redacted", string(evidence)).Error; err != nil {
					return err
				}
			}
		}
		if err := tx.Table("image_nodes").Where("id=? AND task_id=?", id, imagePrepareRootID(ctx, j)).Updates(changes).Error; err != nil {
			return err
		}
		stepStatus := status
		if status == "ready" {
			stepStatus = "succeeded"
		}
		stepChanges := platform.Row{"status": stepStatus, "finished_at": nil}
		if status != "unknown" {
			stepChanges["finished_at"] = a.Time()
		}
		if err := tx.Table("task_steps").Where("task_id=? AND step_key=? AND attempt=1", imagePrepareRootID(ctx, j), imagePrepareNativeKey).Updates(stepChanges).Error; err != nil {
			return err
		}
		if rootID := imagePrepareRootID(ctx, j); rootID != j.ID && status == "failed" {
			public := platform.PublicError(cause, false)
			if err := tx.Table("tasks").Where("id=?", rootID).Updates(platform.Row{"status": "failed", "finished_at": a.Time(), "error_code": public["code"], "lease_owner": nil, "lease_until": nil, "revision": gorm.Expr("revision+1")}).Error; err != nil {
				return err
			}
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return err
		}
		return tx.Table("tasks").Where("id=?", j.ID).Updates(platform.Row{"result_redacted": gorm.Expr("JSON_MERGE_PATCH(COALESCE(result_redacted, JSON_OBJECT()), CAST(? AS JSON))", string(encoded)), "revision": gorm.Expr("revision+1")}).Error
	})
	if err != nil {
		return platform.Unknown(err)
	}
	if status == "unknown" {
		return platform.Unknown(cause)
	}
	return cause
}
