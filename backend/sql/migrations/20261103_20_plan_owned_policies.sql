SET time_zone = '+08:00';

-- One-time cutover only: preserve explicitly stored bindings and economic source
-- identities before removing the former platform binding tables. Existing JSON
-- entries win, even when an administrator must later correct their compatibility.
UPDATE instance_plans SET policy_versions=JSON_OBJECT(),updated_at=updated_at WHERE policy_versions IS NULL OR JSON_TYPE(policy_versions)='NULL';
UPDATE instance_plans r JOIN policy_versions p ON p.kind='pricing' AND p.source_id=r.pricing_version_id
 SET r.policy_versions=JSON_INSERT(r.policy_versions,'$."pricing"',CAST(p.id AS CHAR)),r.updated_at=r.updated_at;
UPDATE instance_plans r JOIN policy_versions p ON p.kind='credit' AND p.source_id=r.credit_policy_version_id
 SET r.policy_versions=JSON_INSERT(r.policy_versions,'$."credit"',CAST(p.id AS CHAR)),r.updated_at=r.updated_at;
UPDATE instance_plans r JOIN policy_versions p ON p.kind='gpu-idle' AND p.source_id=r.idle_policy_version_id
 SET r.policy_versions=JSON_INSERT(r.policy_versions,'$."gpu-idle"',CAST(p.id AS CHAR)),r.updated_at=r.updated_at;
UPDATE instances SET policy_versions=JSON_OBJECT(),updated_at=updated_at WHERE policy_versions IS NULL OR JSON_TYPE(policy_versions)='NULL';
UPDATE instances r JOIN policy_versions p ON p.kind='pricing' AND p.source_id=r.pricing_version_id
 SET r.policy_versions=JSON_INSERT(r.policy_versions,'$."pricing"',CAST(p.id AS CHAR)),r.updated_at=r.updated_at;
UPDATE instances r JOIN policy_versions p ON p.kind='credit' AND p.source_id=r.credit_policy_version_id
 SET r.policy_versions=JSON_INSERT(r.policy_versions,'$."credit"',CAST(p.id AS CHAR)),r.updated_at=r.updated_at;
UPDATE instances r JOIN policy_versions p ON p.kind='gpu-idle' AND p.source_id=r.idle_policy_version_id
 SET r.policy_versions=JSON_INSERT(r.policy_versions,'$."gpu-idle"',CAST(p.id AS CHAR)),r.updated_at=r.updated_at;

-- A single instance binding cannot represent conflicting attached source prices.
-- Require an operator's explicit selection rather than silently choosing one.
SELECT JSON_EXTRACT(CASE WHEN EXISTS(
 SELECT a.instance_id FROM gpu_allocations a JOIN instances i ON i.id=a.instance_id
 WHERE a.active_slot=1 AND NOT JSON_CONTAINS_PATH(i.policy_versions,'one','$."gpu-pricing"')
 GROUP BY a.instance_id HAVING COUNT(DISTINCT a.gpu_price_version_id)>1
) THEN 'Legacy instance has multiple active GPU pricing sources and needs an explicit policy binding' ELSE '{}' END,'$');

-- A legacy instance's own latest allocation is stronger evidence than a platform
-- binding. Never touch its allocations, rates, usage sessions, or ledger rows.
UPDATE instances i JOIN (
 SELECT instance_id,policy_id FROM (
  SELECT a.instance_id,p.id AS policy_id,
   ROW_NUMBER() OVER(PARTITION BY a.instance_id ORDER BY
    CASE WHEN a.active_slot=1 THEN 0 ELSE 1 END,a.id DESC) AS position
  FROM gpu_allocations a JOIN policy_versions p ON p.kind='gpu-pricing' AND p.source_id=a.gpu_price_version_id
 ) ranked WHERE position=1
) old_price ON old_price.instance_id=i.id
 SET i.policy_versions=JSON_INSERT(i.policy_versions,'$."gpu-pricing"',CAST(old_price.policy_id AS CHAR)),i.updated_at=i.updated_at;

-- Copy only genuinely missing kinds from the old explicitly stored map. These
-- values are migration inputs, never a new runtime default or singleton.
UPDATE instance_plans r JOIN platform_policy_bindings b ON b.kind='pricing'
 JOIN policy_versions p ON p.id=b.policy_id AND p.kind=b.kind
 SET r.policy_versions=JSON_INSERT(r.policy_versions,'$."pricing"',CAST(p.id AS CHAR)),r.updated_at=r.updated_at
 WHERE r.pricing_version_id IS NULL;
UPDATE instance_plans r JOIN platform_policy_bindings b ON b.kind='credit'
 JOIN policy_versions p ON p.id=b.policy_id AND p.kind=b.kind
 SET r.policy_versions=JSON_INSERT(r.policy_versions,'$."credit"',CAST(p.id AS CHAR)),r.updated_at=r.updated_at
 WHERE r.credit_policy_version_id IS NULL;
UPDATE instance_plans r JOIN platform_policy_bindings b ON b.kind='metrics'
 JOIN policy_versions p ON p.id=b.policy_id AND p.kind=b.kind
 SET r.policy_versions=JSON_INSERT(r.policy_versions,'$."metrics"',CAST(p.id AS CHAR)),r.updated_at=r.updated_at
 WHERE TRUE;
UPDATE instance_plans r JOIN platform_policy_bindings b ON b.kind='gpu-idle'
 JOIN policy_versions p ON p.id=b.policy_id AND p.kind=b.kind
 SET r.policy_versions=JSON_INSERT(r.policy_versions,'$."gpu-idle"',CAST(p.id AS CHAR)),r.updated_at=r.updated_at
 WHERE r.image_type='container' AND r.idle_policy_version_id IS NULL;
UPDATE instance_plans r JOIN platform_policy_bindings b ON b.kind='gpu-scheduling'
 JOIN policy_versions p ON p.id=b.policy_id AND p.kind=b.kind
 SET r.policy_versions=JSON_INSERT(r.policy_versions,'$."gpu-scheduling"',CAST(p.id AS CHAR)),r.updated_at=r.updated_at
 WHERE TRUE AND r.image_type='container';
UPDATE instances r JOIN platform_policy_bindings b ON b.kind='pricing'
 JOIN policy_versions p ON p.id=b.policy_id AND p.kind=b.kind
 SET r.policy_versions=JSON_INSERT(r.policy_versions,'$."pricing"',CAST(p.id AS CHAR)),r.updated_at=r.updated_at
 WHERE r.pricing_version_id IS NULL;
UPDATE instances r JOIN platform_policy_bindings b ON b.kind='credit'
 JOIN policy_versions p ON p.id=b.policy_id AND p.kind=b.kind
 SET r.policy_versions=JSON_INSERT(r.policy_versions,'$."credit"',CAST(p.id AS CHAR)),r.updated_at=r.updated_at
 WHERE r.credit_policy_version_id IS NULL;
UPDATE instances r JOIN platform_policy_bindings b ON b.kind='metrics'
 JOIN policy_versions p ON p.id=b.policy_id AND p.kind=b.kind
 SET r.policy_versions=JSON_INSERT(r.policy_versions,'$."metrics"',CAST(p.id AS CHAR)),r.updated_at=r.updated_at
 WHERE TRUE;
UPDATE instances r JOIN platform_policy_bindings b ON b.kind='gpu-idle'
 JOIN policy_versions p ON p.id=b.policy_id AND p.kind=b.kind
 SET r.policy_versions=JSON_INSERT(r.policy_versions,'$."gpu-idle"',CAST(p.id AS CHAR)),r.updated_at=r.updated_at
 WHERE r.image_type='container' AND r.idle_policy_version_id IS NULL;
UPDATE instances r JOIN platform_policy_bindings b ON b.kind='gpu-scheduling'
 JOIN policy_versions p ON p.id=b.policy_id AND p.kind=b.kind
 SET r.policy_versions=JSON_INSERT(r.policy_versions,'$."gpu-scheduling"',CAST(p.id AS CHAR)),r.updated_at=r.updated_at
 WHERE TRUE AND r.image_type='container';

-- Missing plan GPU pricing may use only an explicit, compatible old binding.
-- In particular, do not 'repair' existing explicit model/access mismatches.
UPDATE instance_plans r JOIN platform_policy_bindings b ON b.kind='gpu-pricing'
 JOIN policy_versions p ON p.id=b.policy_id AND p.kind=b.kind
 JOIN gpu_price_versions source ON source.id=p.source_id
 SET r.policy_versions=JSON_INSERT(r.policy_versions,'$."gpu-pricing"',CAST(p.id AS CHAR)),r.updated_at=r.updated_at
 WHERE r.image_type='container' AND source.gpu_model=r.gpu_model AND source.access_mode=r.gpu_access_mode;
-- A never-allocated legacy instance has no immutable GPU economics to infer;
-- do not invent a price by taking its node's binding or the cheapest profile.
-- An administrator must explicitly configure a GPU profile before attaching.

-- Refuse a destructive cutover if historical explicit inputs were incomplete.
SELECT JSON_EXTRACT(CASE WHEN
 EXISTS(SELECT 1 FROM instance_plans WHERE
  NOT JSON_CONTAINS_PATH(policy_versions,'all','$.pricing','$.credit','$.metrics') OR
  (image_type='container' AND NOT JSON_CONTAINS_PATH(policy_versions,'all','$."gpu-idle"','$."gpu-scheduling"')) OR
  (image_type='container' AND gpu_model IS NOT NULL AND NOT JSON_CONTAINS_PATH(policy_versions,'one','$."gpu-pricing"'))) OR
 EXISTS(SELECT 1 FROM instances WHERE
  NOT JSON_CONTAINS_PATH(policy_versions,'all','$.pricing','$.credit','$.metrics') OR
  (image_type='container' AND NOT JSON_CONTAINS_PATH(policy_versions,'all','$."gpu-idle"','$."gpu-scheduling"'))) OR
 EXISTS(SELECT 1 FROM instances i JOIN gpu_allocations a ON a.instance_id=i.id
  WHERE NOT JSON_CONTAINS_PATH(i.policy_versions,'one','$."gpu-pricing"'))
 THEN 'Plan-owned policy migration requires complete historical explicit policy mappings' ELSE '{}' END,'$');

-- Fail before destructive DDL when a node has no stored metrics assignment.
SELECT JSON_EXTRACT(CASE WHEN EXISTS(SELECT 1 FROM nodes) AND NOT EXISTS(
 SELECT 1 FROM platform_policy_bindings b JOIN policy_versions p ON p.id=b.policy_id AND p.kind='metrics' WHERE b.kind='metrics'
) THEN 'Plan-owned policy migration requires the existing explicit metrics binding for populated nodes' ELSE '{}' END,'$');
ALTER TABLE nodes ADD COLUMN metrics_policy_id BIGINT UNSIGNED NULL;
UPDATE nodes n JOIN platform_policy_bindings b ON b.kind='metrics'
 JOIN policy_versions p ON p.id=b.policy_id AND p.kind='metrics'
 SET n.metrics_policy_id=p.id,n.updated_at=n.updated_at;
ALTER TABLE nodes MODIFY COLUMN metrics_policy_id BIGINT UNSIGNED NOT NULL,
 ADD CONSTRAINT fk_nodes_metrics_policy FOREIGN KEY(metrics_policy_id) REFERENCES policy_versions(id);

-- VMs do not need GPU idle configuration. Existing source IDs remain intact.
ALTER TABLE instance_plans MODIFY COLUMN idle_policy_version_id BIGINT UNSIGNED NULL;
ALTER TABLE instances MODIFY COLUMN idle_policy_version_id BIGINT UNSIGNED NULL;

-- Keep every enforcement event/member/deadline/snapshot; permit independent
-- instance-credit groups to exhaust the same independent user allowance wallet.
ALTER TABLE credit_enforcements DROP INDEX uq_credit_enforcement_active,
 ADD UNIQUE KEY uq_credit_enforcement_active(active_account_id,policy_version_id);

-- High-window state is ephemeral, not metric/history data. Its former singleton
-- policy interpretation cannot safely be reused by multiple scheduling profiles.
DELETE FROM metric_gpu_high_windows;
ALTER TABLE metric_gpu_high_windows DROP PRIMARY KEY,
 ADD COLUMN policy_version_id BIGINT UNSIGNED NOT NULL,
 ADD PRIMARY KEY(node_id,gpu_uuid,policy_version_id),
 ADD CONSTRAINT fk_metric_gpu_high_policy FOREIGN KEY(policy_version_id) REFERENCES policy_versions(id);

-- Allocation and usage GPU price source references are historical snapshots.
-- Only the hardware-level pricing binding and its availability check disappear.
ALTER TABLE node_gpus DROP CHECK ck_node_gpus_available_price,
 DROP FOREIGN KEY fk_node_gpus_price,
 DROP INDEX ix_node_gpus_price,
 DROP COLUMN gpu_price_version_id;
DROP TABLE platform_policy_bindings;
DROP TABLE policy_binding_sets;
