package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"101cloud/backend/internal/platform"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var nodeGPUUUIDPattern = regexp.MustCompile(`^GPU-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// These jobs only read Agent/Incus state. Drain changes eligibility in the
// request transaction; no worker here stops, migrates or reconfigures a guest.
func nodeInventoryJob(ctx context.Context, a *platform.App, job platform.Job) error {
	start := a.Time()
	initial, e := platform.Get(a.DB.WithContext(ctx), "nodes", job.NodeID, false)
	if e != nil {
		return e
	}
	if e = a.Step(ctx, job, "read-inventory", "", "running"); e != nil {
		return e
	}
	health, e := a.Agent(ctx, job.NodeID, "GET", "/agent/v1/health", nil)
	if e != nil || platform.Text(health["status"]) != "ok" || !platform.Bool(health["incus"]) {
		if updateErr := nodeRecordHealth(ctx, a, job, initial, start, false); updateErr != nil {
			return updateErr
		}
		return platform.E(503, "node_unavailable", "the pinned Agent connection or Incus health check failed")
	}
	resources, e := a.Agent(ctx, job.NodeID, "GET", "/agent/v1/resources", nil)
	if e != nil {
		if updateErr := nodeRecordHealth(ctx, a, job, initial, start, true); updateErr != nil {
			return updateErr
		}
		return platform.E(503, "node_unavailable", "node resource inventory could not be read")
	}
	snapshot, e := nodeResourceSnapshot(resources)
	if e != nil {
		return e
	}
	instances, e := nodeReadInstances(ctx, a, job.NodeID)
	if e != nil {
		if updateErr := nodeRecordHealth(ctx, a, job, initial, start, true); updateErr != nil {
			return updateErr
		}
		return e
	}
	var gpuCount, unmanagedCount int
	e = a.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		node, e := platform.Get(tx, "nodes", job.NodeID, true)
		if e != nil {
			return e
		}
		if e = nodeInventoryFence(tx, job, node, initial, start); e != nil {
			return e
		}
		if gpuCount, e = nodeSaveGPUs(tx, a, job.NodeID, snapshot, start); e != nil {
			return e
		}
		if unmanagedCount, e = nodeSaveUnmanaged(tx, a, job.NodeID, instances, start); e != nil {
			return e
		}
		data, e := json.Marshal(snapshot)
		if e != nil {
			return e
		}
		updates := platform.Row{"resources": string(data), "resources_sampled_at": start, "last_heartbeat_at": start, "connection_verified_at": start, "health": "online", "updated_at": a.Time(), "revision": gorm.Expr("revision + 1")}
		return tx.Table("nodes").Where("id = ?", job.NodeID).Updates(updates).Error
	})
	if e != nil {
		return platform.WrapDB(e)
	}
	if e = a.Step(ctx, job, "read-inventory", "", "succeeded"); e != nil {
		return e
	}
	result := platform.Row{"node_id": strconv.FormatUint(job.NodeID, 10), "observed_at": start.Format(time.RFC3339Nano), "gpu_count": gpuCount, "unmanaged_instance_count": unmanagedCount, "health": "online"}
	if job.Type == "node.drain" {
		var managedCount int64
		if e = a.DB.WithContext(ctx).Table("instances").Where("node_id = ? AND deleted_at IS NULL", job.NodeID).Count(&managedCount).Error; e != nil {
			return platform.WrapDB(e)
		}
		result["mode"] = "prevent_new_allocations"
		result["managed_instance_count"] = managedCount
		result["instances_stopped"] = false
		result["instances_migrated"] = false
	}
	return a.TaskResult(ctx, job, result)
}

func nodeSameConnection(a, b platform.Row) bool {
	for _, key := range []string{"agent_endpoint", "token_ciphertext", "server_cert_sha256", "key_version"} {
		if platform.Text(a[key]) != platform.Text(b[key]) {
			return false
		}
	}
	return true
}
func nodeInventoryFence(tx *gorm.DB, job platform.Job, current, initial platform.Row, sampled time.Time) error {
	if e := nodeObservationFence(current, initial, sampled); e != nil {
		return e
	}
	var task platform.Row
	if e := tx.Table("tasks").Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", job.ID).Take(&task).Error; e != nil {
		return e
	}
	if platform.Uint(task["lease_generation"]) != job.LeaseGeneration || platform.Text(task["status"]) != "running" || !platform.Stamp(task["lease_until"]).After(time.Now()) {
		return platform.E(409, "task_lease_lost", "task no longer owns the inventory lease")
	}
	return nil
}

func nodeObservationFence(current, initial platform.Row, sampled time.Time) error {
	if !nodeSameConnection(current, initial) {
		return platform.E(409, "configuration_conflict", "node connection changed during the probe; run a new probe")
	}
	if platform.Stamp(current["resources_sampled_at"]).After(sampled) || platform.Stamp(current["last_heartbeat_at"]).After(sampled) {
		return platform.E(409, "observation_superseded", "a newer node probe has already been recorded")
	}
	return nil
}
func nodeRecordHealth(ctx context.Context, a *platform.App, job platform.Job, initial platform.Row, sampled time.Time, online bool) error {
	return a.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		current, e := platform.Get(tx, "nodes", job.NodeID, true)
		if e != nil {
			return e
		}
		if e = nodeInventoryFence(tx, job, current, initial, sampled); e != nil {
			return e
		}
		state := "offline"
		if online {
			state = "online"
		}
		updates := platform.Row{"health": state, "updated_at": a.Time(), "revision": gorm.Expr("revision + 1")}
		if online {
			updates["last_heartbeat_at"] = sampled
			updates["connection_verified_at"] = sampled
		}
		return tx.Table("nodes").Where("id = ?", job.NodeID).Updates(updates).Error
	})
}

func nodeResourceSnapshot(raw map[string]any) (platform.Row, error) {
	server, ok := raw["server_resources"].(map[string]any)
	if !ok {
		return nil, platform.E(503, "observation_incomplete", "Agent hardware inventory is incomplete")
	}
	gpus, ok := raw["gpus"].([]any)
	if !ok {
		return nil, platform.E(503, "observation_incomplete", "Agent GPU inventory is incomplete")
	}
	// Full hardware sections are inventory-only; never retain arbitrary Agent
	// response objects or native server configuration in the resource snapshot.
	safeServer := platform.Row{}
	for _, key := range []string{"cpu", "memory", "storage", "network", "gpu", "system", "usb", "pci"} {
		if v, exists := server[key]; exists {
			safeServer[key] = v
		}
	}
	safeGPUs := make([]any, 0, len(gpus))
	for _, rawGPU := range gpus {
		gpu, ok := rawGPU.(map[string]any)
		if !ok {
			return nil, platform.E(503, "observation_incomplete", "Agent GPU inventory is malformed")
		}
		safe := platform.Row{}
		for _, key := range []string{"uuid", "pci_address", "model", "utilization_percent", "memory_used_mib", "memory_total_mib", "memory_utilization_percent", "load_percent"} {
			safe[key] = gpu[key]
		}
		if platform.Text(gpu["error"]) != "" {
			safe["error"] = "telemetry_unavailable"
		}
		safeGPUs = append(safeGPUs, safe)
	}
	out := platform.Row{"server_resources": safeServer, "gpus": safeGPUs, "average_load_percent": raw["average_load_percent"], "network_interfaces": raw["network_interfaces"], "system_dns": raw["system_dns"]}
	// Network errors have stable meaning to consumers, but may contain command
	// diagnostics. Expose only their presence, not subprocess output.
	out["network_errors"] = []string{}
	if errs, ok := raw["network_errors"].([]any); ok && len(errs) > 0 {
		out["network_errors"] = []string{"network_observation_incomplete"}
	}
	return out, nil
}

func nodeMetric(v any) (float64, bool) {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case json.Number:
		var e error
		f, e = x.Float64()
		if e != nil {
			return 0, false
		}
	case int:
		f = float64(x)
	case uint64:
		f = float64(x)
	default:
		return 0, false
	}
	return f, !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0
}
func nodeSaveGPUs(tx *gorm.DB, a *platform.App, nodeID uint64, snapshot platform.Row, sampled time.Time) (int, error) {
	var rows []platform.Row
	if e := tx.Table("node_gpus").Clauses(clause.Locking{Strength: "UPDATE"}).Where("node_id = ?", nodeID).Order("id ASC").Find(&rows).Error; e != nil {
		return 0, e
	}
	existing := make(map[string]platform.Row, len(rows))
	for _, r := range rows {
		existing[strings.ToLower(platform.Text(r["uuid"]))] = r
	}
	gpus, _ := snapshot["gpus"].([]any)
	seen := make(map[string]bool, len(gpus))
	count := 0
	for _, v := range gpus {
		gpu := v.(map[string]any)
		uuid := platform.Text(gpu["uuid"])
		// Unsupported cards without UUID identity remain in the complete snapshot,
		// not a made-up allocatable node_gpus identity.
		if uuid == "" {
			continue
		}
		key := strings.ToLower(uuid)
		if seen[key] {
			return 0, platform.E(503, "observation_incomplete", "Agent returned a duplicate GPU identity")
		}
		seen[key] = true
		if !nodeGPUUUIDPattern.MatchString(uuid) {
			return 0, platform.E(503, "observation_incomplete", "Agent GPU identity is invalid")
		}
		model := platform.Text(gpu["model"])
		if !textValid(model, 255) {
			return 0, platform.E(503, "observation_incomplete", "Agent GPU model is missing")
		}
		var memory any
		if m, ok := nodeMetric(gpu["memory_total_mib"]); ok && m > 0 && m < 18446744073709551616 {
			memory = uint64(m)
		}
		observationHealthy := platform.Text(gpu["error"]) == ""
		if _, ok := nodeMetric(gpu["load_percent"]); !ok {
			observationHealthy = false
		}
		updates := platform.Row{"model": model, "pci_address": gpu["pci_address"], "last_seen_at": sampled, "updated_at": a.Time()}
		if old, found := existing[key]; found {
			// A temporary telemetry gap does not erase a previously measured capacity.
			if memory != nil {
				updates["memory_mib"] = memory
			}
			state := platform.Text(old["status"])
			if state == "missing" || state == "error" {
				state = "disabled"
			}
			if state == "available" && !observationHealthy {
				state = "error"
			}
			updates["status"] = state
			updates["revision"] = gorm.Expr("revision + 1")
			if e := tx.Table("node_gpus").Where("id = ?", platform.Uint(old["id"])).Updates(updates).Error; e != nil {
				return 0, e
			}
		} else {
			updates["node_id"] = nodeID
			updates["uuid"] = uuid
			updates["memory_mib"] = memory
			updates["max_allocations"] = 1
			updates["status"] = "disabled"
			updates["revision"] = 1
			updates["created_at"] = sampled
			if e := tx.Table("node_gpus").Create(&updates).Error; e != nil {
				return 0, e
			}
		}
		count++
	}
	for key, old := range existing {
		if !seen[key] && enum(platform.Text(old["status"]), "available", "error", "missing") {
			if e := tx.Table("node_gpus").Where("id = ?", platform.Uint(old["id"])).Updates(platform.Row{"status": "missing", "revision": gorm.Expr("revision + 1"), "updated_at": a.Time()}).Error; e != nil {
				return 0, e
			}
		}
	}
	return count, nil
}

func nodeNativeMetadata(response map[string]any) (any, error) {
	if platform.Text(response["type"]) != "sync" || platform.Uint(response["status_code"]) != 200 {
		return nil, platform.E(503, "observation_incomplete", "Incus inventory response was not a successful synchronous response")
	}
	metadata, ok := response["metadata"]
	if !ok {
		return nil, platform.E(503, "observation_incomplete", "Incus inventory response has no metadata")
	}
	return metadata, nil
}
func nodeReadInstances(ctx context.Context, a *platform.App, nodeID uint64) ([]platform.Row, error) {
	response, e := a.Agent(ctx, nodeID, "GET", "/1.0", nil)
	if e != nil {
		return nil, platform.E(503, "node_unavailable", "Incus server inventory could not be read")
	}
	metadata, e := nodeNativeMetadata(response)
	if e != nil {
		return nil, e
	}
	server, ok := metadata.(map[string]any)
	if !ok {
		return nil, platform.E(503, "observation_incomplete", "Incus server metadata is malformed")
	}
	environment, _ := server["environment"].(map[string]any)
	clustered := platform.Bool(environment["server_clustered"])
	serverName := platform.Text(environment["server_name"])
	if clustered && serverName == "" {
		return nil, platform.E(503, "observation_incomplete", "cluster member identity is unavailable")
	}
	response, e = a.Agent(ctx, nodeID, "GET", "/1.0/projects?recursion=1", nil)
	if e != nil {
		return nil, platform.E(503, "node_unavailable", "Incus projects could not be inventoried")
	}
	metadata, e = nodeNativeMetadata(response)
	if e != nil {
		return nil, e
	}
	projects, ok := metadata.([]any)
	if !ok {
		return nil, platform.E(503, "observation_incomplete", "Incus project inventory is malformed")
	}
	out := []platform.Row{}
	seen := map[string]bool{}
	for _, v := range projects {
		project, ok := v.(map[string]any)
		if !ok {
			return nil, platform.E(503, "observation_incomplete", "Incus project inventory is malformed")
		}
		name := platform.Text(project["name"])
		if !codeValid(name) || len(name) > 63 || seen[name] {
			return nil, platform.E(503, "observation_incomplete", "Incus project identity is invalid")
		}
		seen[name] = true
		response, e = a.Agent(ctx, nodeID, "GET", "/1.0/instances?recursion=1&project="+url.QueryEscape(name), nil)
		if e != nil {
			return nil, platform.E(503, "node_unavailable", "Incus instances could not be inventoried")
		}
		metadata, e = nodeNativeMetadata(response)
		if e != nil {
			return nil, e
		}
		instances, ok := metadata.([]any)
		if !ok {
			return nil, platform.E(503, "observation_incomplete", "Incus instance inventory is malformed")
		}
		for _, iv := range instances {
			item, ok := iv.(map[string]any)
			if !ok {
				return nil, platform.E(503, "observation_incomplete", "Incus instance inventory is malformed")
			}
			if clustered {
				location := platform.Text(item["location"])
				if location == "" {
					return nil, platform.E(503, "observation_incomplete", "Incus instance member identity is missing")
				}
				if location != serverName {
					continue
				}
			}
			iname := platform.Text(item["name"])
			if !textValid(iname, 63) {
				return nil, platform.E(503, "observation_incomplete", "Incus instance identity is missing")
			}
			kind := platform.Text(item["type"])
			if !enum(kind, "container", "virtual-machine") {
				return nil, platform.E(503, "observation_incomplete", "Incus instance type is unsupported")
			}
			config, _ := item["config"].(map[string]any)
			expanded, _ := item["expanded_config"].(map[string]any)
			safeConfig := platform.Row{}
			for _, key := range []string{"limits.cpu", "limits.memory", "limits.processes"} {
				if value, exists := expanded[key]; exists {
					safeConfig[key] = value
				} else if value, exists := config[key]; exists {
					safeConfig[key] = value
				}
			}
			// No user.*, cloud-init, environment, profiles, scripts, mounts, devices or
			// native configuration blobs enter the unmanaged-instance snapshot.
			var uuid any
			if u := platform.Text(config["volatile.uuid"]); u != "" {
				if len(u) != 36 {
					return nil, platform.E(503, "observation_incomplete", "Incus instance UUID is invalid")
				}
				uuid = u
			}
			out = append(out, platform.Row{"project_name": name, "incus_name": iname, "incus_uuid": uuid, "instance_type": kind, "architecture": platform.Text(item["architecture"]), "observed_state": strings.ToLower(platform.Text(item["status"])), "config": jsonString(safeConfig)})
		}
	}
	return out, nil
}

func nodeSaveUnmanaged(tx *gorm.DB, a *platform.App, nodeID uint64, inventory []platform.Row, sampled time.Time) (int, error) {
	var managed []platform.Row
	if e := tx.Table("instances").Select("project_name,incus_name,incus_uuid").Where("node_id = ? AND deleted_at IS NULL", nodeID).Find(&managed).Error; e != nil {
		return 0, e
	}
	names := map[string]bool{}
	uuids := map[string]bool{}
	for _, r := range managed {
		names[platform.Text(r["project_name"])+"\x00"+platform.Text(r["incus_name"])] = true
		if uuid := platform.Text(r["incus_uuid"]); uuid != "" {
			uuids[uuid] = true
		}
	}
	var existing []platform.Row
	if e := tx.Table("node_unmanaged_instances").Where("node_id = ?", nodeID).Find(&existing).Error; e != nil {
		return 0, e
	}
	old := map[string]platform.Row{}
	for _, r := range existing {
		old[platform.Text(r["project_name"])+"\x00"+platform.Text(r["incus_name"])] = r
	}
	keep := make([]uint64, 0, len(inventory))
	count := 0
	seen := map[string]bool{}
	for _, r := range inventory {
		key := platform.Text(r["project_name"]) + "\x00" + platform.Text(r["incus_name"])
		if seen[key] {
			return 0, platform.E(503, "observation_incomplete", "Incus returned duplicate instance identities")
		}
		seen[key] = true
		if names[key] || uuids[platform.Text(r["incus_uuid"])] {
			continue
		}
		r["node_id"] = nodeID
		r["observed_at"] = sampled
		r["updated_at"] = a.Time()
		if previous, exists := old[key]; exists {
			id := platform.Uint(previous["id"])
			r["revision"] = gorm.Expr("revision + 1")
			if e := tx.Table("node_unmanaged_instances").Where("id = ?", id).Updates(r).Error; e != nil {
				return 0, e
			}
			keep = append(keep, id)
		} else {
			r["revision"] = 1
			r["created_at"] = sampled
			if e := tx.Table("node_unmanaged_instances").Create(&r).Error; e != nil {
				return 0, e
			}
			var created platform.Row
			if e := tx.Table("node_unmanaged_instances").Select("id").Where("node_id = ? AND project_name = ? AND incus_name = ?", nodeID, r["project_name"], r["incus_name"]).Take(&created).Error; e != nil {
				return 0, e
			}
			keep = append(keep, platform.Uint(created["id"]))
		}
		count++
	}
	query := tx.Table("node_unmanaged_instances").Where("node_id = ?", nodeID)
	if len(keep) > 0 {
		query = query.Where("id NOT IN ?", keep)
	}
	if e := query.Delete(&map[string]any{}).Error; e != nil && !errors.Is(e, gorm.ErrRecordNotFound) {
		return 0, e
	}
	return count, nil
}
