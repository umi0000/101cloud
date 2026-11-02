-- Freeze existing instance bounds before plans become editable.
ALTER TABLE instances ADD COLUMN plan_snapshot JSON NULL;
UPDATE instances i JOIN instance_plans p ON p.id=i.instance_plan_id
 SET i.plan_snapshot=JSON_OBJECT('revision',CAST(p.revision AS CHAR),'vcpus',p.vcpus,
 'memory_bytes',CAST(p.memory_bytes AS CHAR),'disk_bytes',CAST(p.disk_bytes AS CHAR),
 'allowed_overrides',COALESCE(p.allowed_overrides,JSON_OBJECT()))
 WHERE i.product_type='elastic';
-- This constraint intentionally rejects orphaned elastic instances rather than
-- inventing resource bounds. Custom instances keep a NULL snapshot.
ALTER TABLE instances ADD CONSTRAINT ck_instances_plan_snapshot CHECK
 ((product_type='custom' AND plan_snapshot IS NULL) OR
  (product_type='elastic' AND plan_snapshot IS NOT NULL AND JSON_TYPE(plan_snapshot)='OBJECT'));
ALTER TABLE instance_plans DROP CHECK ck_instance_plans_status;
UPDATE instance_plans SET status='disabled' WHERE status IN ('draft','retired');
ALTER TABLE instance_plans ADD CONSTRAINT ck_instance_plans_status CHECK (status IN ('active','disabled'));
