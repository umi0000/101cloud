-- Durable monitoring cursors, privacy-safe quality, rollup coverage and console lifecycle.
-- Existing metrics tables remain authoritative. All DATETIME values are UTC+8.
SET time_zone = '+08:00';
ALTER TABLE instance_metric_samples ADD COLUMN quality_reasons JSON NULL;
ALTER TABLE instance_gpu_metric_samples ADD COLUMN metrics_complete BOOLEAN NOT NULL DEFAULT FALSE, ADD COLUMN quality_reasons JSON NULL;
ALTER TABLE instance_metric_rollups ADD COLUMN coverage_intervals JSON NULL;
ALTER TABLE instance_gpu_metric_rollups ADD COLUMN coverage_intervals JSON NULL;

CREATE TABLE metric_gaps (
 id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
 node_id BIGINT UNSIGNED NOT NULL,
 project_scope VARCHAR(63) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 from_at DATETIME(6) NOT NULL,
 to_at DATETIME(6) NOT NULL,
 reason VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 revision BIGINT UNSIGNED NOT NULL DEFAULT 1,
 created_at DATETIME(6) NOT NULL,
 updated_at DATETIME(6) NOT NULL,
 PRIMARY KEY(id), KEY ix_metric_gaps_node_time(node_id,from_at,to_at),
 CONSTRAINT fk_metric_gaps_node FOREIGN KEY(node_id) REFERENCES nodes(id),
 CONSTRAINT ck_metric_gaps_range CHECK(from_at <= to_at),
 CONSTRAINT ck_metric_gaps_reason CHECK(reason IN ('agent_restarted','history_evicted','late_sample'))
) ENGINE=InnoDB;

CREATE TABLE metric_rollup_progress (
 node_id BIGINT UNSIGNED NOT NULL,
 bucket_seconds INT UNSIGNED NOT NULL,
 through_at DATETIME(6) NOT NULL,
 updated_at DATETIME(6) NOT NULL,
 PRIMARY KEY(node_id,bucket_seconds),
 CONSTRAINT fk_metric_rollup_progress_node FOREIGN KEY(node_id) REFERENCES nodes(id),
 CONSTRAINT ck_metric_rollup_progress_bucket CHECK(bucket_seconds>0)
) ENGINE=InnoDB;

CREATE TABLE metric_gpu_idle_windows (
 instance_id BIGINT UNSIGNED NOT NULL,
 gpu_uuid VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 agent_boot_id BINARY(16) NOT NULL,
 runtime_started_at DATETIME(6) NULL,
 last_sequence BIGINT UNSIGNED NOT NULL,
 last_sampled_at DATETIME(6) NOT NULL,
 idle_since DATETIME(6) NULL,
 updated_at DATETIME(6) NOT NULL,
 PRIMARY KEY(instance_id,gpu_uuid),
 CONSTRAINT fk_metric_gpu_idle_instance FOREIGN KEY(instance_id) REFERENCES instances(id)
) ENGINE=InnoDB;

CREATE TABLE metric_gpu_high_windows (
 node_id BIGINT UNSIGNED NOT NULL,
 gpu_uuid VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 high_since DATETIME(6) NULL,
 last_sampled_at DATETIME(6) NOT NULL,
 updated_at DATETIME(6) NOT NULL,
 PRIMARY KEY(node_id,gpu_uuid),
 CONSTRAINT fk_metric_gpu_high_node FOREIGN KEY(node_id) REFERENCES nodes(id)
) ENGINE=InnoDB;

CREATE TABLE metric_rebalance_events (
 id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
 instance_id BIGINT UNSIGNED NOT NULL,
 node_id BIGINT UNSIGNED NOT NULL,
 task_id BIGINT UNSIGNED NOT NULL,
 revision BIGINT UNSIGNED NOT NULL DEFAULT 1,
 created_at DATETIME(6) NOT NULL,
 updated_at DATETIME(6) NOT NULL,
 PRIMARY KEY(id), KEY ix_metric_rebalance_instance_time(instance_id,created_at),
 CONSTRAINT fk_metric_rebalance_instance FOREIGN KEY(instance_id) REFERENCES instances(id),
 CONSTRAINT fk_metric_rebalance_node FOREIGN KEY(node_id) REFERENCES nodes(id),
 CONSTRAINT fk_metric_rebalance_task FOREIGN KEY(task_id) REFERENCES tasks(id)
) ENGINE=InnoDB;

CREATE TABLE console_sessions (
 id CHAR(43) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 user_id BIGINT UNSIGNED NOT NULL,
 auth_session_id BIGINT UNSIGNED NOT NULL,
 instance_id BIGINT UNSIGNED NOT NULL,
 node_id BIGINT UNSIGNED NOT NULL,
 instance_owner_id BIGINT UNSIGNED NOT NULL,
 is_admin BOOLEAN NOT NULL,
 kind VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 cols INT UNSIGNED NOT NULL,
 `rows` INT UNSIGNED NOT NULL,
 operation_path VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NULL,
 secrets_ciphertext BLOB NULL,
 prepare_task_id BIGINT UNSIGNED NULL,
 prepare_started_at DATETIME(6) NULL,
 connected_at DATETIME(6) NULL,
 closed_at DATETIME(6) NULL,
 expires_at DATETIME(6) NOT NULL,
 revision BIGINT UNSIGNED NOT NULL DEFAULT 1,
 created_at DATETIME(6) NOT NULL,
 updated_at DATETIME(6) NOT NULL,
 PRIMARY KEY(id),
 KEY ix_console_user_status_expiry(user_id,status,expires_at),
 KEY ix_console_status_expiry(status,expires_at),
 KEY ix_console_instance(instance_id),
 CONSTRAINT fk_console_user FOREIGN KEY(user_id) REFERENCES users(id),
 CONSTRAINT fk_console_instance FOREIGN KEY(instance_id) REFERENCES instances(id),
 CONSTRAINT fk_console_node FOREIGN KEY(node_id) REFERENCES nodes(id),
 CONSTRAINT fk_console_task FOREIGN KEY(prepare_task_id) REFERENCES tasks(id),
 CONSTRAINT ck_console_kind CHECK(kind IN ('terminal','serial','vga')),
 CONSTRAINT ck_console_status CHECK(status IN ('preparing','ready','active','closed','failed','expired')),
 CONSTRAINT ck_console_dimensions CHECK(cols BETWEEN 1 AND 1000 AND `rows` BETWEEN 1 AND 1000)
) ENGINE=InnoDB;
