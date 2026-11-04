SET time_zone = '+08:00';

-- Forward-only semantic cutover. Keep source/public IDs and every price, balance,
-- settled interval, ledger entry, and completed enforcement audit unchanged.
-- Existing checks permitting retained/running_only/free remain compatible with
-- this conversion; the additional checks below narrow all future writes.
UPDATE credit_policy_versions
 SET storage_charge_mode='running_only',revision=revision+1,updated_at=updated_at
 WHERE storage_charge_mode='retained';
UPDATE policy_versions
 SET config=JSON_SET(config,'$.storage_charge_mode','running_only'),revision=revision+1,updated_at=updated_at
 WHERE kind='credit' AND JSON_UNQUOTE(JSON_EXTRACT(config,'$.storage_charge_mode'))='retained';

-- Active grace events carry frozen economic choices. Normalize only their storage
-- mode, without changing the deadline, grace charging, restoration, or status.
UPDATE credit_enforcements
 SET policy_snapshot=JSON_SET(policy_snapshot,'$.storage_charge_mode','running_only'),revision=revision+1,updated_at=updated_at
 WHERE status IN ('grace','stopping','unknown')
 AND JSON_UNQUOTE(JSON_EXTRACT(policy_snapshot,'$.storage_charge_mode'))='retained';

-- instances/instance_plans.policy_versions hold IDs, not embedded credit config;
-- plan_snapshot stores resource bounds and usage_sessions.config_snapshot stores
-- resource quantities. Their references already resolve the converted rows above.
ALTER TABLE credit_policy_versions
 ADD CONSTRAINT ck_credit_storage_running_only CHECK (storage_charge_mode IN ('running_only','free'));
ALTER TABLE policy_versions
 ADD CONSTRAINT ck_policy_credit_storage_running_only CHECK (kind <> 'credit' OR JSON_UNQUOTE(JSON_EXTRACT(config,'$.storage_charge_mode')) IN ('running_only','free'));
