SET time_zone = '+08:00';

-- GPU idle safety is fixed by complete, fresh zero-activity evidence, not policy
-- percentages. Preserve policy identities, timing, actions and unrelated config.
UPDATE policy_versions
 SET config=JSON_REMOVE(config,'$.gpu_utilization_threshold_pct','$.gpu_memory_threshold_pct'),revision=revision+1,updated_at=updated_at
 WHERE kind='gpu-idle'
 AND JSON_CONTAINS_PATH(config,'one','$.gpu_utilization_threshold_pct','$.gpu_memory_threshold_pct');

-- Retain the independent CPU bound before removing the obsolete GPU columns.
ALTER TABLE idle_policy_versions DROP CHECK ck_idle_policy_versions_thresholds;
ALTER TABLE idle_policy_versions
 ADD CONSTRAINT ck_idle_policy_versions_thresholds CHECK (cpu_utilization_threshold_pct BETWEEN 0 AND 100);
ALTER TABLE idle_policy_versions
 DROP COLUMN gpu_utilization_threshold_pct,
 DROP COLUMN gpu_memory_threshold_pct;
