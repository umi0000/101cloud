SET time_zone = '+08:00';

-- Upgrade precondition: original populated economic policies/accounts do not encode
-- grace charging, restoration, storage charging, or offline charging decisions.
-- Refuse BEFORE billing DDL rather than invent those decisions or leave accounts
-- pointing at unmapped source rows. The migration runner also checks this before
-- the 20261030 migration batch. An operator-reviewed explicit data migration is
-- required for such a legacy database; a schema-only empty database is supported.
-- JSON_EXTRACT deliberately raises a SQL error on the unsafe branch, without
-- creating a temporary/persistent guard table or otherwise changing user data.
SELECT JSON_EXTRACT(CASE WHEN
 EXISTS(SELECT 1 FROM pricing_versions LIMIT 1) OR
 EXISTS(SELECT 1 FROM gpu_price_versions LIMIT 1) OR
 EXISTS(SELECT 1 FROM credit_policy_versions LIMIT 1) OR
 EXISTS(SELECT 1 FROM idle_policy_versions LIMIT 1) OR
 EXISTS(SELECT 1 FROM credit_accounts LIMIT 1)
 THEN 'Legacy economic data requires an explicit reviewed policy migration before this upgrade'
 ELSE '{}' END, '$');

-- Economic policy source tables retain their authoritative foreign-key identities.
ALTER TABLE credit_policy_versions
 ADD COLUMN daily_validity_seconds BIGINT UNSIGNED NULL,
 ADD COLUMN shutdown_grace_seconds INT UNSIGNED NULL,
 ADD COLUMN grace_charge_mode VARCHAR(16) NULL,
 ADD COLUMN restore_action VARCHAR(32) NULL,
 ADD COLUMN storage_charge_mode VARCHAR(24) NULL,
 ADD COLUMN offline_charge_mode VARCHAR(16) NULL;
CREATE TABLE policy_versions (
 id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
 kind VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 version VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 source_id BIGINT UNSIGNED NULL,
 config JSON NOT NULL,
 status VARCHAR(16) NOT NULL DEFAULT 'draft',
 effective_at DATETIME(6) NULL,
 created_by_user_id BIGINT UNSIGNED NOT NULL,
 revision BIGINT UNSIGNED NOT NULL DEFAULT 1,
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
 UNIQUE KEY uq_policy_kind_code_version(kind,code,version),
 UNIQUE KEY uq_policy_source(kind,source_id),
 CONSTRAINT fk_policy_creator FOREIGN KEY(created_by_user_id) REFERENCES users(id),
 CONSTRAINT ck_policy_kind CHECK(kind IN ('pricing','gpu-pricing','credit','gpu-scheduling','gpu-idle','metrics')),
 CONSTRAINT ck_policy_status CHECK(status IN ('draft','active','retired'))
) ENGINE=InnoDB;
CREATE TABLE platform_policy_bindings (
 kind VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
 policy_id BIGINT UNSIGNED NOT NULL,
 revision BIGINT UNSIGNED NOT NULL DEFAULT 1,
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
 CONSTRAINT fk_platform_policy_version FOREIGN KEY(policy_id) REFERENCES policy_versions(id)
) ENGINE=InnoDB;
CREATE TABLE policy_binding_sets (
 id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
 revision BIGINT UNSIGNED NOT NULL DEFAULT 1,
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6)
) ENGINE=InnoDB;
INSERT INTO policy_binding_sets(id) VALUES(1);
ALTER TABLE credit_accounts ADD COLUMN grant_cursor_at DATETIME(6) NULL;
ALTER TABLE usage_sessions
 ADD COLUMN metering_state VARCHAR(16) NOT NULL DEFAULT 'running',
 ADD COLUMN metered_microseconds BIGINT UNSIGNED NOT NULL DEFAULT 0,
 ADD COLUMN cumulative_expected_pt DECIMAL(20,6) NOT NULL DEFAULT 0;
CREATE TABLE credit_daily_grants (
 id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
 account_id BIGINT UNSIGNED NOT NULL,
 business_date DATE NOT NULL,
 policy_version_id BIGINT UNSIGNED NOT NULL,
 valid_from DATETIME(6) NOT NULL,
 expires_at DATETIME(6) NOT NULL,
 bucket_id BIGINT UNSIGNED NULL,
 revision BIGINT UNSIGNED NOT NULL DEFAULT 1,
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
 UNIQUE KEY uq_daily_grant_account_date(account_id,business_date),
 CONSTRAINT fk_daily_grant_account FOREIGN KEY(account_id) REFERENCES credit_accounts(id),
 CONSTRAINT fk_daily_grant_policy FOREIGN KEY(policy_version_id) REFERENCES credit_policy_versions(id),
 CONSTRAINT fk_daily_grant_bucket FOREIGN KEY(bucket_id) REFERENCES credit_buckets(id)
) ENGINE=InnoDB;
CREATE TABLE credit_enforcements (
 id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
 account_id BIGINT UNSIGNED NOT NULL,
 user_id BIGINT UNSIGNED NOT NULL,
 policy_version_id BIGINT UNSIGNED NOT NULL,
 status VARCHAR(16) NOT NULL,
 exhausted_at DATETIME(6) NOT NULL,
 shutdown_deadline DATETIME(6) NOT NULL,
 policy_snapshot JSON NOT NULL,
 notification_id BIGINT UNSIGNED NULL,
 active_account_id BIGINT UNSIGNED GENERATED ALWAYS AS (CASE WHEN status IN ('grace','stopping','unknown') THEN account_id ELSE NULL END) STORED,
 revision BIGINT UNSIGNED NOT NULL DEFAULT 1,
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
 UNIQUE KEY uq_credit_enforcement_active(active_account_id),
 KEY ix_credit_enforcement_deadline(status,shutdown_deadline),
 CONSTRAINT fk_credit_enforcement_account FOREIGN KEY(account_id,user_id) REFERENCES credit_accounts(id,user_id),
 CONSTRAINT fk_credit_enforcement_policy FOREIGN KEY(policy_version_id) REFERENCES credit_policy_versions(id),
 CONSTRAINT ck_credit_enforcement_status CHECK(status IN ('grace','stopping','completed','cancelled','unknown'))
) ENGINE=InnoDB;
CREATE TABLE credit_enforcement_instances (
 id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
 enforcement_id BIGINT UNSIGNED NOT NULL,
 instance_id BIGINT UNSIGNED NOT NULL,
 task_id BIGINT UNSIGNED NULL,
 status VARCHAR(16) NOT NULL DEFAULT 'pending',
 revision BIGINT UNSIGNED NOT NULL DEFAULT 1,
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
 UNIQUE KEY uq_credit_enforcement_instance(enforcement_id,instance_id),
 CONSTRAINT fk_enforcement_instance_event FOREIGN KEY(enforcement_id) REFERENCES credit_enforcements(id),
 CONSTRAINT fk_enforcement_instance_instance FOREIGN KEY(instance_id) REFERENCES instances(id),
 CONSTRAINT fk_enforcement_instance_task FOREIGN KEY(task_id) REFERENCES tasks(id)
) ENGINE=InnoDB;
CREATE TABLE notifications (
 id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
 user_id BIGINT UNSIGNED NOT NULL,
 type VARCHAR(64) NOT NULL,
 message TEXT NOT NULL,
 related_type VARCHAR(32) NULL,
 related_id BIGINT UNSIGNED NULL,
 read_at DATETIME(6) NULL,
 revision BIGINT UNSIGNED NOT NULL DEFAULT 1,
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
 KEY ix_notifications_user_time(user_id,created_at,id),
 CONSTRAINT fk_notification_user FOREIGN KEY(user_id) REFERENCES users(id)
) ENGINE=InnoDB;
