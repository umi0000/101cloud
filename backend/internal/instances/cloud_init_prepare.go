package instances

import (
	"context"
	"errors"
	"net/http"

	"101cloud/backend/internal/platform"
)

const cloudInitPrepareStep = "create.cloud-init-prepare"

// Only the create worker may prepare its own stopped, never-started container.
// Unlike asynchronous Incus operations, this fixed-path mutation is idempotent:
// recovery may repeat it, but only while there is still no first-start intent.
func workerPrepareCloudInit(ctx context.Context, a *platform.App, j platform.Job, r platform.Row) error {
	if j.Type != "instance.create" || platform.Text(r["image_type"]) != "container" || !platform.Bool(object(object(r["incus_config_snapshot"])["creation"])["cloud_init_capable"]) {
		return nil
	}
	start, err := workerStep(ctx, a, j, "state.start")
	if err != nil {
		return err
	}
	if start != nil {
		return nil // Never touch a guest once its first start may have been submitted.
	}
	old, err := workerStep(ctx, a, j, cloudInitPrepareStep)
	if err != nil {
		return err
	}
	if old != nil {
		switch platform.Text(old["status"]) {
		case "succeeded":
			return nil
		case "cancelled":
			return workerSkipped
		}
	}
	current, err := workerInstance(ctx, a, j)
	if err != nil {
		return err
	}
	if current["deleted_at"] != nil || current["creation_failed_at"] != nil || platform.Text(current["desired_state"]) != "running" {
		return workerSkipped
	}
	if platform.Bool(object(current["incus_config_snapshot"])["creation_delivered"]) || !platform.Stamp(current["login_initialized_at"]).IsZero() || platform.Text(current["incus_uuid"]) == "" || platform.Text(current["project_name"]) == "" || platform.Text(current["incus_name"]) == "" || workerFingerprint(current["image_fingerprint"]) == "" {
		return errors.New("cloud-init preparation requires an undelivered creation identity")
	}
	created, err := workerStep(ctx, a, j, "create.native")
	if err != nil {
		return err
	}
	if created == nil || platform.Text(created["status"]) != "succeeded" {
		return errors.New("cloud-init preparation requires confirmed native creation")
	}
	m, _, err := workerObserve(ctx, a, current)
	if err != nil {
		return err
	}
	cfg := object(m["config"])
	if platform.Text(m["type"]) != "container" || platform.Text(cfg["volatile.uuid"]) != platform.Text(current["incus_uuid"]) || platform.Text(cfg["user.101cloud.instance_id"]) != platform.Text(current["id"]) || workerFingerprint(cfg["volatile.base_image"]) != workerFingerprint(current["image_fingerprint"]) {
		return errors.New("cloud-init preparation identity or image mismatch")
	}
	state, err := workerState(ctx, a, current)
	if err != nil {
		return err
	}
	if state != "stopped" {
		return errors.New("cloud-init preparation requires a stopped instance")
	}
	if err = a.Step(ctx, j, cloudInitPrepareStep, "", "running"); err != nil {
		return err
	}
	result, err := a.Agent(platform.WithTaskDispatch(ctx, j, cloudInitPrepareStep), j.NodeID, http.MethodPost, "/agent/v1/instances/cloud-init/prepare", platform.Row{
		"project": platform.Text(current["project_name"]), "name": platform.Text(current["incus_name"]),
		"expected_uuid": platform.Text(current["incus_uuid"]), "expected_instance_id": platform.Text(current["id"]),
		"expected_image_fingerprint": workerFingerprint(current["image_fingerprint"]),
	})
	if err != nil {
		return err
	}
	checked, validChecked := result["checked"].(bool)
	_, validRemoved := result["disabled_marker_removed"].(bool)
	if !validChecked || !checked || !validRemoved {
		return errors.New("cloud-init preparation omitted checked result")
	}
	return a.Step(ctx, j, cloudInitPrepareStep, "", "succeeded")
}
