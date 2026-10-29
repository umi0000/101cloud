-- Container metrics schema upgrade for an existing 101cloud business database.
-- MySQL 8.4 LTS. Select the target database using the mysql client argument.
-- Run once after the original business schema; this is not a full reinitialization.
-- Contains only the five new metrics tables, no sample data or business DML.
-- Example invocation: mysql --default-character-set=utf8mb4 -u <DBA> -p 101cloud < backend/sql/migrations/20261029_instance_metrics.sql
-- DDL implicitly commits. Do not use --force to hide an already-applied or partial upgrade.
-- Agent publishes in-memory snapshots only; backend ingestion, rollups and retention
-- are application responsibilities. CPU uses its own counter observation window.
-- Verified with MySQL 8.4.11 in an owned temporary schema; test fixtures rolled back.
SET NAMES utf8mb4 COLLATE utf8mb4_0900_ai_ci;
SET SESSION time_zone = '+00:00';

CREATE TABLE instance_metric_samples (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    instance_id BIGINT UNSIGNED NOT NULL,
    node_id BIGINT UNSIGNED NOT NULL,
    agent_boot_id BINARY(16) NOT NULL COMMENT 'Random agent-start identity, not an OS or process identifier',
    sample_sequence BIGINT UNSIGNED NOT NULL COMMENT 'Monotonic within an agent boot; zero is permitted',
    instance_uuid CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'Immutable identity observed at collection time',
    instance_state VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'Normalized runtime state observed at this sample, not the current business state',
    runtime_started_at DATETIME(6) NULL COMMENT 'Runtime generation boundary; NULL if unavailable, never a PID',
    sampled_at DATETIME(6) NOT NULL,
    window_start_at DATETIME(6) NOT NULL,
    window_end_at DATETIME(6) NOT NULL,
    window_seconds DECIMAL(20,6) NOT NULL COMMENT 'Zero for a first snapshot without a preceding observation window',
    cpu_sampled_at DATETIME(6) NULL COMMENT 'CPU counter observation time before potentially slow GPU collection',
    cpu_window_seconds DECIMAL(20,6) NULL COMMENT 'Actual CPU counter interval, independent of the GPU frame window',
    cpu_usage_total_ns BIGINT UNSIGNED NULL,
    cpu_usage_delta_ns BIGINT UNSIGNED NULL COMMENT 'NULL across restarts, counter resets, or missing history',
    allocated_vcpus DECIMAL(18,6) NULL,
    allocated_cpu_seconds DECIMAL(40,9) NULL COMMENT 'Integral of allocated CPU cores over the valid observation window',
    cpu_utilization_pct DECIMAL(18,6) NULL COMMENT 'Allocation-normalized observation; may exceed 100 during quota bursts',
    memory_used_bytes BIGINT UNSIGNED NULL,
    memory_limit_bytes BIGINT UNSIGNED NULL COMMENT 'NULL for unknown or unbounded limits',
    memory_peak_bytes BIGINT UNSIGNED NULL,
    gpu_state VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    collection_complete BOOLEAN NOT NULL,
    metrics_complete BOOLEAN NOT NULL COMMENT 'Target instance metric quality, independently of node inventory completeness',
    attribution_complete BOOLEAN NOT NULL,
    reference_scan_quality VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    error_code VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL COMMENT 'Allowlisted anonymous code only; never raw collector errors',
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_ims_agent_sample (node_id, agent_boot_id, sample_sequence, instance_id),
    UNIQUE KEY uq_ims_snapshot_identity (id, instance_id, node_id, sampled_at),
    KEY ix_ims_instance_time (instance_id, sampled_at, id),
    KEY ix_ims_retention (sampled_at, id),
    CONSTRAINT fk_ims_instance FOREIGN KEY (instance_id) REFERENCES instances (id),
    CONSTRAINT fk_ims_node FOREIGN KEY (node_id) REFERENCES nodes (id),
    CONSTRAINT ck_ims_uuid CHECK (CHAR_LENGTH(instance_uuid) = 36),
    CONSTRAINT ck_ims_window CHECK (window_start_at <= window_end_at AND window_end_at <= sampled_at AND window_seconds >= 0 AND window_seconds = TIMESTAMPDIFF(MICROSECOND, window_start_at, window_end_at) / 1000000),
    CONSTRAINT ck_ims_runtime CHECK (runtime_started_at IS NULL OR runtime_started_at <= sampled_at),
    CONSTRAINT ck_ims_cpu CHECK ((allocated_vcpus IS NULL OR allocated_vcpus > 0) AND (allocated_cpu_seconds IS NULL OR allocated_cpu_seconds >= 0) AND (cpu_utilization_pct IS NULL OR cpu_utilization_pct >= 0)),
    CONSTRAINT ck_ims_cpu_delta CHECK (cpu_usage_delta_ns IS NULL OR (cpu_sampled_at IS NOT NULL AND cpu_window_seconds IS NOT NULL AND cpu_window_seconds > 0 AND cpu_usage_total_ns IS NOT NULL AND cpu_usage_delta_ns <= cpu_usage_total_ns)),
    CONSTRAINT ck_ims_cpu_window CHECK (cpu_window_seconds IS NULL OR (cpu_sampled_at IS NOT NULL AND cpu_window_seconds > 0)),
    CONSTRAINT ck_ims_gpu_state CHECK (gpu_state IN ('in_use', 'idle_candidate', 'unknown', 'not_attached')),
    CONSTRAINT ck_ims_complete CHECK (collection_complete IN (0, 1) AND metrics_complete IN (0, 1) AND attribution_complete IN (0, 1)),
    CONSTRAINT ck_ims_state CHECK (instance_state IN ('pending', 'running', 'stopped', 'frozen', 'starting', 'stopping', 'restarting', 'error', 'unknown')),
    CONSTRAINT ck_ims_reference_quality CHECK (reference_scan_quality IN ('complete', 'partial', 'unsupported', 'unknown')),
    CONSTRAINT ck_ims_error CHECK (error_code IS NULL OR error_code IN ('collection_failed', 'unsupported', 'partial_visibility', 'counter_reset', 'runtime_changed', 'insufficient_history', 'stale_sample', 'instance_unavailable', 'gpu_unavailable', 'unknown_error'))
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

-- One configured or actually observed GPU per instance snapshot. The UUID and hardware
-- snapshots remain authoritative history even when inventory changes; gpu_id is optional.
-- MPS and incomplete ownership/scan evidence must never imply idle; absent SM samples alone are not evidence either way.
-- idle_candidate is an observation only, not evidence that detaching/releasing a GPU is safe.
-- Retention workers delete GPU sample children before their instance sample parents.
CREATE TABLE instance_gpu_metric_samples (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    sample_id BIGINT UNSIGNED NOT NULL,
    instance_id BIGINT UNSIGNED NOT NULL,
    node_id BIGINT UNSIGNED NOT NULL,
    sampled_at DATETIME(6) NOT NULL,
    gpu_uuid VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    gpu_id BIGINT UNSIGNED NULL COMMENT 'Optional inventory link, never the sole hardware identity',
    gpu_model VARCHAR(255) NULL,
    gpu_memory_total_bytes BIGINT UNSIGNED NULL,
    is_configured BOOLEAN NULL COMMENT 'NULL when configuration could not be read',
    is_observed BOOLEAN NULL COMMENT 'Actual attributed use or reference; NULL when visibility is insufficient',
    gpu_state VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    memory_used_bytes BIGINT UNSIGNED NULL COMMENT 'Attributed device memory, not host-wide device usage',
    utilization_pct DECIMAL(9,6) NULL COMMENT 'Approximate attributed SM utilization; not a device-wide value',
    process_count INT UNSIGNED NULL COMMENT 'Distinct attributed GPU processes, not CUDA context object count; NULL is unknown',
    reference_count INT UNSIGNED NULL COMMENT 'Anonymous device-reference count; no reference paths are retained',
    attribution_complete BOOLEAN NOT NULL,
    reference_scan_quality VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    compute_process_support VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    graphics_process_support VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    utilization_support VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    memory_support VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    error_code VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_igms_sample_uuid (sample_id, gpu_uuid),
    KEY ix_igms_snapshot (sample_id, instance_id, node_id, sampled_at),
    KEY ix_igms_instance_time (instance_id, sampled_at, gpu_uuid),
    KEY ix_igms_retention (sampled_at, id),
    KEY ix_igms_gpu (gpu_id),
    CONSTRAINT fk_igms_snapshot FOREIGN KEY (sample_id, instance_id, node_id, sampled_at) REFERENCES instance_metric_samples (id, instance_id, node_id, sampled_at),
    CONSTRAINT fk_igms_gpu FOREIGN KEY (gpu_id) REFERENCES node_gpus (id),
    CONSTRAINT ck_igms_uuid CHECK (CHAR_LENGTH(gpu_uuid) > 0),
    CONSTRAINT ck_igms_flags CHECK ((is_configured IS NULL OR is_configured IN (0, 1)) AND (is_observed IS NULL OR is_observed IN (0, 1)) AND attribution_complete IN (0, 1)),
    CONSTRAINT ck_igms_state CHECK (gpu_state IN ('in_use', 'idle_candidate', 'unknown', 'not_attached')),
    CONSTRAINT ck_igms_utilization CHECK (utilization_pct IS NULL OR utilization_pct BETWEEN 0 AND 100),
    CONSTRAINT ck_igms_reference_quality CHECK (reference_scan_quality IN ('complete', 'partial', 'unsupported', 'unknown')),
    CONSTRAINT ck_igms_support CHECK (compute_process_support IN ('supported', 'unsupported', 'unknown') AND graphics_process_support IN ('supported', 'unsupported', 'unknown') AND utilization_support IN ('supported', 'unsupported', 'unknown') AND memory_support IN ('supported', 'unsupported', 'unknown')),
    CONSTRAINT ck_igms_error CHECK (error_code IS NULL OR error_code IN ('collection_failed', 'unsupported', 'partial_visibility', 'counter_reset', 'runtime_changed', 'insufficient_history', 'stale_sample', 'instance_unavailable', 'gpu_unavailable', 'unknown_error'))
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

-- Rollups store additive sufficient statistics, never AVG(avg). Backend clips each
-- observation to bucket boundaries, unions coverage without double-counting overlaps,
-- and merges only compatible instance/runtime intervals; counter resets add no CPU delta.
-- Coverage is independent of every metric's valid_seconds: an observed unknown sample
-- contributes coverage/sample_count/unknown_count but does not contribute synthetic zero.
-- An aggregate with no valid observations has NULL sum/peak, zero valid_seconds.
-- DECIMAL(40,6) preserves bytes*seconds beyond BIGINT; capacities/peaks stay unsigned BIGINT.
CREATE TABLE instance_metric_rollups (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    instance_id BIGINT UNSIGNED NOT NULL,
    node_id BIGINT UNSIGNED NOT NULL COMMENT 'Historical node, independent of the current instance placement',
    bucket_start_at DATETIME(6) NOT NULL,
    bucket_seconds INT UNSIGNED NOT NULL COMMENT 'Configured aggregation granularity, not a fixed retention period',
    sample_count BIGINT UNSIGNED NOT NULL,
    coverage_seconds DECIMAL(20,6) NOT NULL,
    unknown_count BIGINT UNSIGNED NOT NULL COMMENT 'Samples with incomplete collection or attribution',
    cpu_valid_seconds DECIMAL(20,6) NOT NULL,
    cpu_usage_delta_ns DECIMAL(40,0) NULL COMMENT 'Sum of valid counter differences, not cumulative counters',
    cpu_allocation_valid_seconds DECIMAL(20,6) NOT NULL,
    allocated_cpu_seconds DECIMAL(40,9) NULL,
    cpu_normalized_usage_delta_ns DECIMAL(40,0) NULL COMMENT 'Only CPU deltas whose allocation is known for the entire CPU window',
    cpu_utilization_pct_seconds DECIMAL(40,6) NULL COMMENT 'Time-weighted sum of known allocation-normalized percentages',
    cpu_used_cores_peak DECIMAL(20,6) NULL,
    cpu_utilization_peak_pct DECIMAL(20,6) NULL,
    memory_used_valid_seconds DECIMAL(20,6) NOT NULL,
    memory_used_byte_seconds DECIMAL(40,6) NULL,
    memory_limit_valid_seconds DECIMAL(20,6) NOT NULL,
    memory_limit_byte_seconds DECIMAL(40,6) NULL,
    memory_utilization_valid_seconds DECIMAL(20,6) NOT NULL,
    memory_utilization_pct_seconds DECIMAL(40,6) NULL COMMENT 'Only windows with paired known usage and nonzero limit',
    memory_utilization_peak_pct DECIMAL(20,6) NULL,
    memory_peak_bytes BIGINT UNSIGNED NULL COMMENT 'Maximum observed memory_used_bytes in this bucket, not the lifetime raw peak',
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_imr_instance_bucket (instance_id, node_id, bucket_start_at, bucket_seconds),
    KEY ix_imr_instance_time (instance_id, bucket_start_at, bucket_seconds),
    KEY ix_imr_node (node_id),
    KEY ix_imr_retention (bucket_start_at, id),
    CONSTRAINT fk_imr_instance FOREIGN KEY (instance_id) REFERENCES instances (id),
    CONSTRAINT fk_imr_node FOREIGN KEY (node_id) REFERENCES nodes (id),
    CONSTRAINT ck_imr_bucket CHECK (bucket_seconds > 0 AND TIMESTAMPDIFF(SECOND, bucket_start_at, '9999-12-31 23:59:59.999999') >= bucket_seconds),
    CONSTRAINT ck_imr_counts CHECK (sample_count > 0 AND unknown_count <= sample_count),
    CONSTRAINT ck_imr_coverage CHECK (coverage_seconds >= 0 AND coverage_seconds <= bucket_seconds),
    CONSTRAINT ck_imr_valid_seconds CHECK (cpu_valid_seconds BETWEEN 0 AND coverage_seconds AND cpu_allocation_valid_seconds BETWEEN 0 AND coverage_seconds AND memory_used_valid_seconds BETWEEN 0 AND coverage_seconds AND memory_limit_valid_seconds BETWEEN 0 AND coverage_seconds AND memory_utilization_valid_seconds BETWEEN 0 AND memory_used_valid_seconds AND memory_utilization_valid_seconds <= memory_limit_valid_seconds),
    CONSTRAINT ck_imr_cpu_sum CHECK ((cpu_valid_seconds = 0 AND cpu_usage_delta_ns IS NULL) OR (cpu_valid_seconds > 0 AND cpu_usage_delta_ns IS NOT NULL AND cpu_usage_delta_ns >= 0)),
    CONSTRAINT ck_imr_allocation_sum CHECK ((cpu_allocation_valid_seconds = 0 AND allocated_cpu_seconds IS NULL AND cpu_normalized_usage_delta_ns IS NULL AND cpu_utilization_pct_seconds IS NULL AND cpu_utilization_peak_pct IS NULL) OR (cpu_allocation_valid_seconds > 0 AND allocated_cpu_seconds IS NOT NULL AND allocated_cpu_seconds > 0 AND cpu_normalized_usage_delta_ns IS NOT NULL AND cpu_normalized_usage_delta_ns >= 0 AND cpu_utilization_pct_seconds IS NOT NULL AND cpu_utilization_pct_seconds >= 0 AND cpu_utilization_peak_pct IS NOT NULL AND cpu_utilization_peak_pct >= 0)),
    CONSTRAINT ck_imr_cpu_peak CHECK ((cpu_valid_seconds = 0 AND cpu_used_cores_peak IS NULL) OR (cpu_valid_seconds > 0 AND cpu_used_cores_peak IS NOT NULL AND cpu_used_cores_peak >= 0)),
    CONSTRAINT ck_imr_memory_sum CHECK ((memory_used_valid_seconds = 0 AND memory_used_byte_seconds IS NULL) OR (memory_used_valid_seconds > 0 AND memory_used_byte_seconds IS NOT NULL AND memory_used_byte_seconds >= 0)),
    CONSTRAINT ck_imr_limit_sum CHECK ((memory_limit_valid_seconds = 0 AND memory_limit_byte_seconds IS NULL) OR (memory_limit_valid_seconds > 0 AND memory_limit_byte_seconds IS NOT NULL AND memory_limit_byte_seconds >= 0)),
    CONSTRAINT ck_imr_memory_pct CHECK ((memory_utilization_valid_seconds = 0 AND memory_utilization_pct_seconds IS NULL AND memory_utilization_peak_pct IS NULL) OR (memory_utilization_valid_seconds > 0 AND memory_utilization_pct_seconds IS NOT NULL AND memory_utilization_pct_seconds >= 0 AND memory_utilization_peak_pct IS NOT NULL AND memory_utilization_peak_pct >= 0))
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

-- GPU rollups are independent of raw samples and instance rollup rows, so configured
-- raw/rollup retention policies can differ. UUID remains a logical hardware snapshot.
-- Weighted utilization = utilization_pct_seconds / utilization_valid_seconds; weighted
-- memory = memory_used_byte_seconds / memory_valid_seconds, only for positive denominators.
-- A sample may be unknown for attribution while still supplying another valid metric.
CREATE TABLE instance_gpu_metric_rollups (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    instance_id BIGINT UNSIGNED NOT NULL,
    node_id BIGINT UNSIGNED NOT NULL,
    bucket_start_at DATETIME(6) NOT NULL,
    bucket_seconds INT UNSIGNED NOT NULL,
    gpu_uuid VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    gpu_id BIGINT UNSIGNED NULL COMMENT 'Optional inventory link; UUID is retained regardless of inventory lifecycle',
    gpu_model VARCHAR(255) NULL,
    gpu_memory_total_bytes BIGINT UNSIGNED NULL,
    sample_count BIGINT UNSIGNED NOT NULL,
    coverage_seconds DECIMAL(20,6) NOT NULL,
    unknown_count BIGINT UNSIGNED NOT NULL COMMENT 'Includes incomplete attribution, MPS, unsupported, or failed observations',
    utilization_valid_seconds DECIMAL(20,6) NOT NULL,
    utilization_pct_seconds DECIMAL(40,6) NULL,
    utilization_peak_pct DECIMAL(9,6) NULL,
    memory_valid_seconds DECIMAL(20,6) NOT NULL,
    memory_used_byte_seconds DECIMAL(40,6) NULL,
    memory_peak_bytes BIGINT UNSIGNED NULL,
    process_valid_sample_count BIGINT UNSIGNED NOT NULL,
    max_processes INT UNSIGNED NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_igmr_instance_bucket_gpu (instance_id, node_id, bucket_start_at, bucket_seconds, gpu_uuid),
    KEY ix_igmr_instance_time (instance_id, bucket_start_at, gpu_uuid),
    KEY ix_igmr_node (node_id),
    KEY ix_igmr_gpu (gpu_id),
    KEY ix_igmr_retention (bucket_start_at, id),
    CONSTRAINT fk_igmr_instance FOREIGN KEY (instance_id) REFERENCES instances (id),
    CONSTRAINT fk_igmr_node FOREIGN KEY (node_id) REFERENCES nodes (id),
    CONSTRAINT fk_igmr_gpu FOREIGN KEY (gpu_id) REFERENCES node_gpus (id),
    CONSTRAINT ck_igmr_uuid CHECK (CHAR_LENGTH(gpu_uuid) > 0),
    CONSTRAINT ck_igmr_bucket CHECK (bucket_seconds > 0 AND TIMESTAMPDIFF(SECOND, bucket_start_at, '9999-12-31 23:59:59.999999') >= bucket_seconds),
    CONSTRAINT ck_igmr_counts CHECK (sample_count > 0 AND unknown_count <= sample_count AND process_valid_sample_count <= sample_count),
    CONSTRAINT ck_igmr_coverage CHECK (coverage_seconds >= 0 AND coverage_seconds <= bucket_seconds),
    CONSTRAINT ck_igmr_valid_seconds CHECK (utilization_valid_seconds BETWEEN 0 AND coverage_seconds AND memory_valid_seconds BETWEEN 0 AND coverage_seconds),
    CONSTRAINT ck_igmr_utilization_sum CHECK ((utilization_valid_seconds = 0 AND utilization_pct_seconds IS NULL AND utilization_peak_pct IS NULL) OR (utilization_valid_seconds > 0 AND utilization_pct_seconds IS NOT NULL AND utilization_pct_seconds BETWEEN 0 AND 100 * utilization_valid_seconds AND utilization_peak_pct IS NOT NULL AND utilization_peak_pct BETWEEN 0 AND 100)),
    CONSTRAINT ck_igmr_memory_sum CHECK ((memory_valid_seconds = 0 AND memory_used_byte_seconds IS NULL) OR (memory_valid_seconds > 0 AND memory_used_byte_seconds IS NOT NULL AND memory_used_byte_seconds >= 0)),
    CONSTRAINT ck_igmr_processes CHECK ((process_valid_sample_count = 0 AND max_processes IS NULL) OR (process_valid_sample_count > 0 AND max_processes IS NOT NULL))
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

-- Backend ingestion checkpoints for the Agent's bounded incremental sample history.
-- project_scope is the collection request scope (for example '*' or 'default'), not
-- a foreign key to an Incus project; '*' means all projects and inserts no default row.
-- In one backend transaction, insert/deduplicate raw instance/GPU samples and advance
-- this cursor only after every returned sample in that scope has been handled.
-- Lock the cursor row before advancing it. Sequences are comparable only within the
-- same agent_boot_id; a changed boot ID must be explicitly recognized as a new stream,
-- never silently compared to or merged with the previous boot's last_sequence.
-- Detect/report bounded-history gaps in the backend; advancing a cursor cannot recover
-- evicted samples and must not turn missing history into zero-valued observations.
CREATE TABLE node_metric_cursors (
    node_id BIGINT UNSIGNED NOT NULL,
    project_scope VARCHAR(63) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '*' COMMENT 'Backend collection scope, not a project identity relation',
    agent_boot_id BINARY(16) NOT NULL,
    last_sequence BIGINT UNSIGNED NOT NULL,
    last_sampled_at DATETIME(6) NULL COMMENT 'UTC sample time; NULL before any sample has been ingested',
    last_ingested_at DATETIME(6) NOT NULL COMMENT 'UTC time of the last committed ingestion checkpoint',
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (node_id, project_scope),
    CONSTRAINT fk_nmc_node FOREIGN KEY (node_id) REFERENCES nodes (id),
    CONSTRAINT ck_nmc_scope CHECK (CHAR_LENGTH(project_scope) > 0)
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
