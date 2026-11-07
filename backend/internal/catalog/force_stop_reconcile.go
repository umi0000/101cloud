package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
)

// ReconcileForceStoppedTask is observation-only: a cancelled source never grants
// permission to replay a download, alter native configuration, or release assets.
func ReconcileForceStoppedTask(ctx context.Context, a *platform.App, job platform.Job, source platform.Row) error {
	if platform.Text(source["status"]) != "cancelled" || platform.Text(source["error_code"]) != "task_force_stopped" || platform.Uint(source["node_id"]) != job.NodeID || platform.Uint(source["user_id"]) != job.UserID || platform.Uint(source["instance_id"]) != 0 {
		return platform.Unknown(errors.New("force-stopped catalog source identity changed"))
	}
	ctx = context.WithValue(ctx, forceStoppedCatalogSourceKey{}, source)
	job.Type = platform.Text(source["task_type"])
	job.Parameters = json.RawMessage(platform.Text(source["parameters_redacted"]))
	if err := a.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return catalogTaskFence(ctx, tx, job, a.Time()) }); err != nil {
		return err
	}
	var err error
	switch job.Type {
	case "image.prepare":
		err = reconcileForceStoppedImage(ctx, a, job, source)
	case "node.probe", "node.drain":
		err = nodeInventoryJob(ctx, a, job)
	case "network.validate":
		err = networkRunValidation(ctx, a, job, true)
	default:
		return platform.Unknown(errors.New("force-stopped task has no safe catalog reconciler"))
	}
	if err != nil {
		return err
	}
	return a.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := catalogTaskFence(ctx, tx, job, a.Time()); err != nil {
			return err
		}
		return tx.Table("tasks").Where("id=?", source["id"]).Updates(platform.Row{"result_redacted": gorm.Expr("JSON_SET(COALESCE(result_redacted, JSON_OBJECT()), '$.force_stop.reconciled_at', ?, '$.force_stop.reconciliation_completed', CAST('true' AS JSON))", a.Time().Format(time.RFC3339Nano)), "revision": gorm.Expr("revision+1")}).Error
	})
}

func reconcileForceStoppedImage(ctx context.Context, a *platform.App, job platform.Job, source platform.Row) error {
	var input imagePrepareInput
	if err := json.Unmarshal(job.Parameters, &input); err != nil {
		return err
	}
	id, err := platform.IDString(input.ImageNodeID)
	if err != nil {
		return err
	}
	selection, err := platform.Get(a.DB.WithContext(ctx), "image_nodes", id, false)
	if err != nil {
		return err
	}
	image, err := platform.Get(a.DB.WithContext(ctx), "images", platform.Uint(selection["image_id"]), false)
	if err != nil {
		return err
	}
	identity := imagePrepareIdentity(selection, image)
	config := identity
	var journal platform.Row
	res := a.DB.WithContext(ctx).Table("task_steps").Where("task_id=? AND step_key=? AND attempt=1", source["id"], imagePrepareConfigKey).Take(&journal)
	if res.Error == nil {
		if err = json.Unmarshal([]byte(platform.Text(journal["parameters_redacted"])), &config); err != nil {
			return err
		}
	} else if !errors.Is(res.Error, gorm.ErrRecordNotFound) {
		return res.Error
	} else if input.Config != nil {
		config = *input.Config
	}
	// Derived cloud-init evidence is allowed; the immutable artifact selection
	// must still match the selection that this cancelled task originally owned.
	identity.InferCloudInit = config.InferCloudInit
	if config.InferCloudInit {
		identity.CloudInit = config.CloudInit
	}
	if config != identity || platform.Uint(selection["task_id"]) != platform.Uint(source["id"]) || platform.Uint(selection["node_id"]) != job.NodeID {
		return platform.Unknown(errors.New("force-stopped image selection changed"))
	}
	var cloudInit *bool
	exists, err := imagePrepareInspect(ctx, a, job.NodeID, config, &cloudInit)
	if err != nil {
		return platform.Unknown(err)
	}
	if !exists {
		return platform.Unknown(errors.New("force-stopped image is not yet observable; download must not be replayed"))
	}
	return a.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := catalogTaskFence(ctx, tx, job, a.Time()); err != nil {
			return err
		}
		current, err := platform.Get(tx, "image_nodes", id, true)
		if err != nil {
			return err
		}
		if platform.Uint(current["task_id"]) != platform.Uint(source["id"]) || platform.Uint(current["revision"]) != platform.Uint(selection["revision"]) {
			return platform.Unknown(errors.New("force-stopped image selection changed during observation"))
		}
		changes := platform.Row{"status": "ready", "error": nil, "last_verified_at": a.Time(), "revision": gorm.Expr("revision+1")}
		if cloudInit != nil {
			changes["cloud_init"] = *cloudInit
		}
		return tx.Table("image_nodes").Where("id=?", id).Updates(changes).Error
	})
}
