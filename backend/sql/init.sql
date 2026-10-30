-- 101cloud 业务后端数据库结构初始化
-- 目标：MySQL 8.4 LTS / InnoDB / utf8mb4。
-- 用途：全新空库初始化；不是升级迁移，也不会覆盖、删除现有表。
-- 不包含任何业务数据、示例数据、默认管理员、默认价格、额度或数据库账户。
-- 执行方式：mysql --default-character-set=utf8mb4 -u <DBA用户> -p < backend/sql/init.sql
-- 已在MySQL 8.4.11唯一临时库验证37表初始化、原32表增量升级及指标约束；测试事务已回滚。
-- DDL会隐式提交，不能以事务回滚整个初始化；出错后应检查已完成的DDL再处理。
-- 应用的每个数据库连接同样必须使用UTC+8（+08:00）时间和严格SQL模式；外部时间入库前显式转换。
-- 内存/磁盘容量使用bytes；GPU观测memory_mib是明确标注的MiB指标。
-- 金额与费率使用DECIMAL(20,6)，不得经浮点数计算。
-- 所有外键省略referential actions，使用MySQL默认RESTRICT/NO ACTION，禁止级联抹除历史。
-- 已发布的价格/模板版本不可原地改写，credit_ledger/audit_logs必须采用append-only写入；
-- 本DDL不创建数据库用户、权限或触发器，这些写入限制由应用和数据库授权实现。
-- 节点容量、用户配额、共享名额、IP池重叠/子网包含性、每日发放、额度预留及扣费，
-- 必须由应用在固定锁顺序的短事务中处理；不要在事务里等待Host Agent HTTP请求。
-- Agent超时不等于外部操作失败，未知/回收中状态必须继续占用资源，不能盲目重发。
-- Token只保存密文；主密钥位于数据库之外。所有JSON快照/参数/日志必须去除秘密。

SET NAMES utf8mb4 COLLATE utf8mb4_0900_ai_ci;
SET SESSION time_zone = '+08:00';

CREATE DATABASE IF NOT EXISTS `101cloud`
  DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
USE `101cloud`;

-- Core identity, configuration, inventory, instances, and GPU allocation history.
-- Timestamps use UTC+8 (+08:00). Rates and policy values must be explicitly configured.

CREATE TABLE users (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    username VARCHAR(64) NOT NULL,
    email VARCHAR(254) NULL,
    password_hash VARBINARY(255) NOT NULL,
    role VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    status VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    last_login_at DATETIME(6) NULL,
    deleted_at DATETIME(6) NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_users_username (username),
    UNIQUE KEY uq_users_email (email),
    CONSTRAINT ck_users_role CHECK (role IN ('user', 'admin')),
    CONSTRAINT ck_users_status CHECK (status IN ('active', 'disabled', 'deleted')),
    CONSTRAINT ck_users_password_hash CHECK (OCTET_LENGTH(password_hash) > 0),
    CONSTRAINT ck_users_deleted CHECK ((status = 'deleted') = (deleted_at IS NOT NULL))
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

CREATE TABLE user_sessions (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    user_id BIGINT UNSIGNED NOT NULL,
    token_hash BINARY(32) NOT NULL COMMENT 'SHA-256 of the high-entropy session token; never store plaintext tokens',
    expires_at DATETIME(6) NOT NULL,
    revoked_at DATETIME(6) NULL,
    last_used_at DATETIME(6) NULL,
    client_ip VARBINARY(16) NULL,
    user_agent VARCHAR(1024) NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_user_sessions_token_hash (token_hash),
    KEY ix_user_sessions_user_expiry (user_id, expires_at),
    KEY ix_user_sessions_expiry (expires_at),
    CONSTRAINT fk_user_sessions_user FOREIGN KEY (user_id) REFERENCES users (id),
    CONSTRAINT ck_user_sessions_ip CHECK (client_ip IS NULL OR OCTET_LENGTH(client_ip) IN (4, 16)),
    CONSTRAINT ck_user_sessions_expiry CHECK (expires_at > created_at)
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

CREATE TABLE user_limits (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    user_id BIGINT UNSIGNED NOT NULL,
    max_instances INT UNSIGNED NOT NULL,
    max_vcpus INT UNSIGNED NOT NULL,
    max_memory_bytes BIGINT UNSIGNED NOT NULL,
    max_disk_bytes BIGINT UNSIGNED NOT NULL,
    max_gpu_allocations INT UNSIGNED NOT NULL,
    max_resident_instances INT UNSIGNED NOT NULL,
    max_port_forwards INT UNSIGNED NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_user_limits_user (user_id),
    CONSTRAINT fk_user_limits_user FOREIGN KEY (user_id) REFERENCES users (id),
    CONSTRAINT ck_user_limits_resident_count CHECK (max_resident_instances <= max_instances)
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

CREATE TABLE availability_zones (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    name VARCHAR(128) NOT NULL,
    description TEXT NULL,
    status VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_availability_zones_code (code),
    CONSTRAINT ck_availability_zones_status CHECK (status IN ('active', 'disabled'))
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

CREATE TABLE pricing_versions (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    cpu_hourly_pt DECIMAL(20,6) NOT NULL COMMENT 'pt per vCPU-hour',
    memory_gib_hourly_pt DECIMAL(20,6) NOT NULL COMMENT 'pt per GiB-hour; memory_bytes is divided by 1073741824',
    disk_gib_hourly_pt DECIMAL(20,6) NOT NULL COMMENT 'pt per provisioned GiB-hour; disk_bytes is divided by 1073741824',
    status VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    effective_at DATETIME(6) NOT NULL,
    created_by_user_id BIGINT UNSIGNED NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_pricing_versions_code_version (code, version),
    KEY ix_pricing_versions_effective (status, effective_at),
    KEY ix_pricing_versions_creator (created_by_user_id),
    CONSTRAINT fk_pricing_versions_creator FOREIGN KEY (created_by_user_id) REFERENCES users (id),
    CONSTRAINT ck_pricing_versions_rates CHECK (cpu_hourly_pt >= 0 AND memory_gib_hourly_pt >= 0 AND disk_gib_hourly_pt >= 0),
    CONSTRAINT ck_pricing_versions_status CHECK (status IN ('draft', 'active', 'retired'))
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

CREATE TABLE gpu_price_versions (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    name VARCHAR(128) NOT NULL,
    gpu_model VARCHAR(255) NOT NULL,
    access_mode VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    gpu_hourly_pt DECIMAL(20,6) NOT NULL COMMENT 'pt per allocated physical GPU-hour',
    status VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    effective_at DATETIME(6) NOT NULL,
    created_by_user_id BIGINT UNSIGNED NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_gpu_price_versions_code_version (code, version),
    KEY ix_gpu_price_versions_effective (status, effective_at),
    KEY ix_gpu_price_versions_creator (created_by_user_id),
    CONSTRAINT fk_gpu_price_versions_creator FOREIGN KEY (created_by_user_id) REFERENCES users (id),
    CONSTRAINT ck_gpu_price_versions_rate CHECK (gpu_hourly_pt >= 0),
    CONSTRAINT ck_gpu_price_versions_access_mode CHECK (access_mode IN ('shared', 'exclusive')),
    CONSTRAINT ck_gpu_price_versions_status CHECK (status IN ('draft', 'active', 'retired'))
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

CREATE TABLE credit_policy_versions (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    daily_allowance_pt DECIMAL(20,6) NOT NULL,
    bonus_cap_pt DECIMAL(20,6) NOT NULL,
    timezone VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'IANA time zone used to derive the daily credit cycle',
    reset_time TIME(6) NOT NULL,
    redemption_validity_seconds BIGINT UNSIGNED NULL COMMENT 'NULL means redeemed bonus credit does not expire',
    billing_window_seconds INT UNSIGNED NOT NULL,
    status VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    effective_at DATETIME(6) NOT NULL,
    created_by_user_id BIGINT UNSIGNED NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_credit_policy_versions_code_version (code, version),
    KEY ix_credit_policy_versions_effective (status, effective_at),
    KEY ix_credit_policy_versions_creator (created_by_user_id),
    CONSTRAINT fk_credit_policy_versions_creator FOREIGN KEY (created_by_user_id) REFERENCES users (id),
    CONSTRAINT ck_credit_policy_versions_pt CHECK (daily_allowance_pt >= 0 AND bonus_cap_pt >= 0),
    CONSTRAINT ck_credit_policy_versions_timezone CHECK (CHAR_LENGTH(timezone) > 0),
    CONSTRAINT ck_credit_policy_versions_reset CHECK (reset_time >= '00:00:00' AND reset_time < '24:00:00'),
    CONSTRAINT ck_credit_policy_versions_windows CHECK ((redemption_validity_seconds IS NULL OR redemption_validity_seconds > 0) AND billing_window_seconds > 0),
    CONSTRAINT ck_credit_policy_versions_status CHECK (status IN ('draft', 'active', 'retired'))
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

CREATE TABLE idle_policy_versions (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    sample_interval_seconds INT UNSIGNED NOT NULL,
    evaluation_window_seconds INT UNSIGNED NOT NULL,
    consecutive_idle_windows INT UNSIGNED NOT NULL,
    cpu_utilization_threshold_pct DECIMAL(5,2) NOT NULL,
    gpu_utilization_threshold_pct DECIMAL(5,2) NOT NULL,
    gpu_memory_threshold_pct DECIMAL(5,2) NOT NULL,
    network_bytes_threshold BIGINT UNSIGNED NOT NULL,
    action VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    status VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    effective_at DATETIME(6) NOT NULL,
    created_by_user_id BIGINT UNSIGNED NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_idle_policy_versions_code_version (code, version),
    KEY ix_idle_policy_versions_effective (status, effective_at),
    KEY ix_idle_policy_versions_creator (created_by_user_id),
    CONSTRAINT fk_idle_policy_versions_creator FOREIGN KEY (created_by_user_id) REFERENCES users (id),
    CONSTRAINT ck_idle_policy_versions_windows CHECK (sample_interval_seconds > 0 AND evaluation_window_seconds >= sample_interval_seconds AND consecutive_idle_windows > 0),
    CONSTRAINT ck_idle_policy_versions_thresholds CHECK (cpu_utilization_threshold_pct BETWEEN 0 AND 100 AND gpu_utilization_threshold_pct BETWEEN 0 AND 100 AND gpu_memory_threshold_pct BETWEEN 0 AND 100),
    CONSTRAINT ck_idle_policy_versions_action CHECK (action IN ('release_gpu', 'stop_instance')),
    CONSTRAINT ck_idle_policy_versions_status CHECK (status IN ('draft', 'active', 'retired'))
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

CREATE TABLE images (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    name VARCHAR(128) NOT NULL,
    source_type VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    source_protocol VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL COMMENT 'Image transport protocol, such as incus or simplestreams; distinct from local/remote source location',
    source_url VARCHAR(2048) NULL,
    source_alias VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NULL,
    source_project VARCHAR(63) CHARACTER SET ascii COLLATE ascii_bin NULL,
    fingerprint BINARY(32) NOT NULL COMMENT 'SHA-256 image fingerprint as raw digest bytes',
    architecture VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    image_type VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    gpu_ready BOOLEAN NOT NULL,
    os_name VARCHAR(64) NOT NULL,
    os_release VARCHAR(64) NOT NULL,
    properties JSON NOT NULL,
    status VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_by_user_id BIGINT UNSIGNED NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_images_code_version (code, version),
    KEY ix_images_fingerprint_type (fingerprint, image_type),
    KEY ix_images_status_arch (status, architecture, image_type),
    KEY ix_images_creator (created_by_user_id),
    CONSTRAINT fk_images_creator FOREIGN KEY (created_by_user_id) REFERENCES users (id),
    CONSTRAINT ck_images_source_type CHECK (source_type IN ('local', 'remote')),
    CONSTRAINT ck_images_source_url CHECK (source_type = 'local' OR source_url IS NOT NULL),
    CONSTRAINT ck_images_architecture CHECK (CHAR_LENGTH(architecture) > 0),
    CONSTRAINT ck_images_type CHECK (image_type IN ('container', 'virtual-machine')),
    CONSTRAINT ck_images_gpu_ready CHECK (gpu_ready IN (0, 1)),
    CONSTRAINT ck_images_properties CHECK (JSON_TYPE(properties) = 'OBJECT'),
    CONSTRAINT ck_images_status CHECK (status IN ('active', 'disabled', 'retired'))
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

CREATE TABLE init_template_versions (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    name VARCHAR(128) NOT NULL,
    version VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    phase VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    script_content LONGTEXT NOT NULL,
    script_sha256 BINARY(32) NOT NULL,
    parameter_schema JSON NOT NULL COMMENT 'JSON Schema for the parameters accepted by this exact script version',
    timeout_seconds INT UNSIGNED NOT NULL,
    status VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_by_user_id BIGINT UNSIGNED NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_init_template_versions_code_version (code, version),
    KEY ix_init_template_versions_creator (created_by_user_id),
    CONSTRAINT fk_init_template_versions_creator FOREIGN KEY (created_by_user_id) REFERENCES users (id),
    CONSTRAINT ck_init_template_versions_phase CHECK (phase IN ('cloud_init', 'post_start')),
    CONSTRAINT ck_init_template_versions_script CHECK (OCTET_LENGTH(script_content) > 0),
    CONSTRAINT ck_init_template_versions_schema CHECK (JSON_TYPE(parameter_schema) = 'OBJECT'),
    CONSTRAINT ck_init_template_versions_timeout CHECK (timeout_seconds > 0),
    CONSTRAINT ck_init_template_versions_status CHECK (status IN ('draft', 'active', 'retired'))
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

CREATE TABLE nodes (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    availability_zone_id BIGINT UNSIGNED NOT NULL,
    name VARCHAR(128) NOT NULL,
    agent_endpoint VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'Canonical HTTPS origin; normalize host and port before storage',
    token_ciphertext BLOB NOT NULL COMMENT 'Authenticated encrypted Agent bearer token including its nonce/tag; key is held outside this database',
    key_version INT UNSIGNED NOT NULL COMMENT 'External encryption key identifier, never key material',
    server_cert_sha256 BINARY(32) NOT NULL COMMENT 'Pinned SHA-256 of the Agent leaf certificate DER',
    default_storage_pool VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    default_project_name VARCHAR(63) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    allocatable_vcpus INT UNSIGNED NOT NULL,
    allocatable_memory_bytes BIGINT UNSIGNED NOT NULL,
    allocatable_disk_bytes BIGINT UNSIGNED NOT NULL,
    resident_vcpu_budget INT UNSIGNED NOT NULL,
    resident_memory_budget_bytes BIGINT UNSIGNED NOT NULL,
    resident_disk_budget_bytes BIGINT UNSIGNED NOT NULL,
    resources JSON NULL,
    resources_sampled_at DATETIME(6) NULL,
    last_heartbeat_at DATETIME(6) NULL,
    status VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_nodes_name (name),
    UNIQUE KEY uq_nodes_agent_endpoint (agent_endpoint),
    KEY ix_nodes_zone_status (availability_zone_id, status),
    CONSTRAINT fk_nodes_zone FOREIGN KEY (availability_zone_id) REFERENCES availability_zones (id),
    CONSTRAINT ck_nodes_endpoint CHECK (LEFT(agent_endpoint, 8) = 'https://' AND CHAR_LENGTH(agent_endpoint) > 8),
    CONSTRAINT ck_nodes_token CHECK (OCTET_LENGTH(token_ciphertext) > 0 AND key_version > 0),
    CONSTRAINT ck_nodes_defaults CHECK (CHAR_LENGTH(default_storage_pool) > 0 AND CHAR_LENGTH(default_project_name) > 0),
    CONSTRAINT ck_nodes_resident_budget CHECK (resident_vcpu_budget <= allocatable_vcpus AND resident_memory_budget_bytes <= allocatable_memory_bytes AND resident_disk_budget_bytes <= allocatable_disk_bytes),
    CONSTRAINT ck_nodes_resources CHECK ((resources IS NULL AND resources_sampled_at IS NULL) OR (resources IS NOT NULL AND JSON_TYPE(resources) = 'OBJECT' AND resources_sampled_at IS NOT NULL)),
    CONSTRAINT ck_nodes_status CHECK (status IN ('active', 'disabled', 'draining', 'offline', 'maintenance'))
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

CREATE TABLE node_gpus (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    node_id BIGINT UNSIGNED NOT NULL,
    uuid VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    model VARCHAR(255) NOT NULL,
    pci_address VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL COMMENT 'Non-authoritative last-observed snapshot only; UUID is the GPU identity and Agent resolves current PCI address',
    memory_mib BIGINT UNSIGNED NOT NULL,
    max_allocations INT UNSIGNED NOT NULL,
    gpu_price_version_id BIGINT UNSIGNED NULL COMMENT 'May be unpriced while disabled; required before allocation',
    status VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    last_seen_at DATETIME(6) NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_node_gpus_node_uuid (node_id, uuid),
    UNIQUE KEY uq_node_gpus_id_node (id, node_id),
    KEY ix_node_gpus_node_status (node_id, status),
    KEY ix_node_gpus_price (gpu_price_version_id),
    CONSTRAINT fk_node_gpus_node FOREIGN KEY (node_id) REFERENCES nodes (id),
    CONSTRAINT fk_node_gpus_price FOREIGN KEY (gpu_price_version_id) REFERENCES gpu_price_versions (id),
    CONSTRAINT ck_node_gpus_capacity CHECK (memory_mib > 0 AND max_allocations > 0),
    CONSTRAINT ck_node_gpus_uuid CHECK (CHAR_LENGTH(uuid) > 0),
    CONSTRAINT ck_node_gpus_status CHECK (status IN ('available', 'disabled', 'maintenance', 'missing', 'error')),
    CONSTRAINT ck_node_gpus_available_price CHECK (status <> 'available' OR gpu_price_version_id IS NOT NULL)
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

CREATE TABLE network_pools (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    availability_zone_id BIGINT UNSIGNED NOT NULL,
    name VARCHAR(128) NOT NULL,
    address_family TINYINT UNSIGNED NOT NULL,
    network_address VARBINARY(16) NOT NULL,
    prefix_length TINYINT UNSIGNED NOT NULL,
    range_start VARBINARY(16) NOT NULL,
    range_end VARBINARY(16) NOT NULL,
    gateway VARBINARY(16) NULL,
    dns_servers JSON NOT NULL COMMENT 'JSON array of textual IP addresses; never inferred from a node interface',
    status VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_network_pools_zone_name (availability_zone_id, name),
    UNIQUE KEY uq_network_pools_id_family (id, address_family),
    KEY ix_network_pools_zone_status (availability_zone_id, status),
    CONSTRAINT fk_network_pools_zone FOREIGN KEY (availability_zone_id) REFERENCES availability_zones (id),
    CONSTRAINT ck_network_pools_family CHECK (address_family IN (4, 6)),
    CONSTRAINT ck_network_pools_address_lengths CHECK ((address_family = 4 AND OCTET_LENGTH(network_address) = 4 AND OCTET_LENGTH(range_start) = 4 AND OCTET_LENGTH(range_end) = 4 AND (gateway IS NULL OR OCTET_LENGTH(gateway) = 4)) OR (address_family = 6 AND OCTET_LENGTH(network_address) = 16 AND OCTET_LENGTH(range_start) = 16 AND OCTET_LENGTH(range_end) = 16 AND (gateway IS NULL OR OCTET_LENGTH(gateway) = 16))),
    CONSTRAINT ck_network_pools_prefix CHECK ((address_family = 4 AND prefix_length <= 32) OR (address_family = 6 AND prefix_length <= 128)),
    CONSTRAINT ck_network_pools_range CHECK (range_start <= range_end),
    CONSTRAINT ck_network_pools_dns CHECK (JSON_TYPE(dns_servers) = 'ARRAY'),
    CONSTRAINT ck_network_pools_status CHECK (status IN ('active', 'disabled', 'exhausted'))
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

CREATE TABLE node_network_bindings (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    node_id BIGINT UNSIGNED NOT NULL,
    network_pool_id BIGINT UNSIGNED NOT NULL,
    mode VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    parent_interface VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NULL,
    incus_network_name VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NULL,
    project_name VARCHAR(63) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    mtu INT UNSIGNED NULL,
    status VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_node_network_bindings_node_pool (node_id, network_pool_id),
    UNIQUE KEY uq_node_network_bindings_id_node (id, node_id),
    UNIQUE KEY uq_node_network_bindings_id_pool (id, network_pool_id),
    UNIQUE KEY uq_node_network_bindings_id_node_pool (id, node_id, network_pool_id),
    KEY ix_node_network_bindings_pool (network_pool_id),
    CONSTRAINT fk_node_network_bindings_node FOREIGN KEY (node_id) REFERENCES nodes (id),
    CONSTRAINT fk_node_network_bindings_pool FOREIGN KEY (network_pool_id) REFERENCES network_pools (id),
    CONSTRAINT ck_node_network_bindings_mode CHECK (mode IN ('macvlan', 'bridge', 'managed_bridge')),
    CONSTRAINT ck_node_network_bindings_target CHECK ((mode IN ('macvlan', 'bridge') AND parent_interface IS NOT NULL AND CHAR_LENGTH(parent_interface) > 0) OR (mode = 'managed_bridge' AND incus_network_name IS NOT NULL AND CHAR_LENGTH(incus_network_name) > 0)),
    CONSTRAINT ck_node_network_bindings_project CHECK (CHAR_LENGTH(project_name) > 0),
    CONSTRAINT ck_node_network_bindings_mtu CHECK (mtu IS NULL OR mtu >= 576),
    CONSTRAINT ck_node_network_bindings_status CHECK (status IN ('active', 'disabled'))
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

CREATE TABLE instance_plans (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    availability_zone_id BIGINT UNSIGNED NOT NULL,
    code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    version VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    name VARCHAR(128) NOT NULL,
    description TEXT NULL,
    image_type VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    vcpus INT UNSIGNED NOT NULL,
    memory_bytes BIGINT UNSIGNED NOT NULL,
    disk_bytes BIGINT UNSIGNED NOT NULL,
    gpu_count INT UNSIGNED NOT NULL,
    gpu_model VARCHAR(255) NULL,
    pricing_version_id BIGINT UNSIGNED NOT NULL,
    credit_policy_version_id BIGINT UNSIGNED NOT NULL,
    idle_policy_version_id BIGINT UNSIGNED NOT NULL,
    init_template_version_id BIGINT UNSIGNED NULL,
    init_parameters JSON NOT NULL COMMENT 'Non-secret parameters only; credentials and other secrets must not be persisted here',
    allow_residency BOOLEAN NOT NULL,
    status VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_instance_plans_code_version (code, version),
    KEY ix_instance_plans_zone_name (availability_zone_id, name),
    KEY ix_instance_plans_pricing (pricing_version_id),
    KEY ix_instance_plans_credit (credit_policy_version_id),
    KEY ix_instance_plans_idle (idle_policy_version_id),
    KEY ix_instance_plans_init (init_template_version_id),
    CONSTRAINT fk_instance_plans_zone FOREIGN KEY (availability_zone_id) REFERENCES availability_zones (id),
    CONSTRAINT fk_instance_plans_pricing FOREIGN KEY (pricing_version_id) REFERENCES pricing_versions (id),
    CONSTRAINT fk_instance_plans_credit FOREIGN KEY (credit_policy_version_id) REFERENCES credit_policy_versions (id),
    CONSTRAINT fk_instance_plans_idle FOREIGN KEY (idle_policy_version_id) REFERENCES idle_policy_versions (id),
    CONSTRAINT fk_instance_plans_init FOREIGN KEY (init_template_version_id) REFERENCES init_template_versions (id),
    CONSTRAINT ck_instance_plans_type CHECK (image_type IN ('container', 'virtual-machine')),
    CONSTRAINT ck_instance_plans_resources CHECK (vcpus > 0 AND memory_bytes > 0 AND disk_bytes > 0),
    CONSTRAINT ck_instance_plans_parameters CHECK (JSON_TYPE(init_parameters) = 'OBJECT'),
    CONSTRAINT ck_instance_plans_residency CHECK (allow_residency IN (0, 1)),
    CONSTRAINT ck_instance_plans_status CHECK (status IN ('active', 'disabled', 'retired'))
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

CREATE TABLE instances (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    user_id BIGINT UNSIGNED NOT NULL,
    node_id BIGINT UNSIGNED NOT NULL,
    image_id BIGINT UNSIGNED NOT NULL,
    instance_plan_id BIGINT UNSIGNED NOT NULL,
    logical_name VARCHAR(128) NOT NULL,
    project_name VARCHAR(63) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    incus_name VARCHAR(63) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    incus_uuid CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL COMMENT 'Incus instance UUID; populated after creation',
    image_type VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    architecture VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    image_fingerprint BINARY(32) NOT NULL,
    vcpus INT UNSIGNED NOT NULL,
    memory_bytes BIGINT UNSIGNED NOT NULL,
    disk_bytes BIGINT UNSIGNED NOT NULL,
    storage_pool VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    pricing_version_id BIGINT UNSIGNED NOT NULL,
    cpu_hourly_pt DECIMAL(20,6) NOT NULL,
    memory_gib_hourly_pt DECIMAL(20,6) NOT NULL,
    disk_gib_hourly_pt DECIMAL(20,6) NOT NULL,
    credit_policy_version_id BIGINT UNSIGNED NOT NULL,
    billing_mode VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    desired_state VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    observed_state VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    provisioning_state VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    observed_at DATETIME(6) NULL,
    auto_release_gpu BOOLEAN NOT NULL,
    auto_rebind_gpu BOOLEAN NOT NULL,
    idle_policy_version_id BIGINT UNSIGNED NOT NULL,
    max_gpu_hourly_pt DECIMAL(20,6) NOT NULL COMMENT 'User-configured upper bound for automatic GPU selection',
    init_template_version_id BIGINT UNSIGNED NULL,
    init_parameters JSON NOT NULL COMMENT 'Non-secret parameters only; credentials and other secrets must not be persisted here',
    incus_config_snapshot JSON NOT NULL COMMENT 'Redacted non-secret configuration snapshot only; exclude passwords, private keys, tokens and secret script contents',
    deleted_at DATETIME(6) NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_instances_id_user_node (id, user_id, node_id),
    UNIQUE KEY uq_instances_id_user (id, user_id),
    UNIQUE KEY uq_instances_id_node (id, node_id),
    UNIQUE KEY uq_instances_incus_identity (node_id, project_name, incus_name),
    UNIQUE KEY uq_instances_node_incus_uuid (node_id, incus_uuid),
    KEY ix_instances_user_deleted (user_id, deleted_at),
    KEY ix_instances_node_states (node_id, desired_state, provisioning_state),
    KEY ix_instances_image (image_id),
    KEY ix_instances_plan (instance_plan_id),
    KEY ix_instances_pricing (pricing_version_id),
    KEY ix_instances_credit (credit_policy_version_id),
    KEY ix_instances_idle (idle_policy_version_id),
    KEY ix_instances_init (init_template_version_id),
    CONSTRAINT fk_instances_user FOREIGN KEY (user_id) REFERENCES users (id),
    CONSTRAINT fk_instances_node FOREIGN KEY (node_id) REFERENCES nodes (id),
    CONSTRAINT fk_instances_image FOREIGN KEY (image_id) REFERENCES images (id),
    CONSTRAINT fk_instances_plan FOREIGN KEY (instance_plan_id) REFERENCES instance_plans (id),
    CONSTRAINT fk_instances_pricing FOREIGN KEY (pricing_version_id) REFERENCES pricing_versions (id),
    CONSTRAINT fk_instances_credit FOREIGN KEY (credit_policy_version_id) REFERENCES credit_policy_versions (id),
    CONSTRAINT fk_instances_idle FOREIGN KEY (idle_policy_version_id) REFERENCES idle_policy_versions (id),
    CONSTRAINT fk_instances_init FOREIGN KEY (init_template_version_id) REFERENCES init_template_versions (id),
    CONSTRAINT ck_instances_names CHECK (CHAR_LENGTH(logical_name) > 0 AND CHAR_LENGTH(project_name) > 0 AND CHAR_LENGTH(incus_name) > 0 AND CHAR_LENGTH(storage_pool) > 0),
    CONSTRAINT ck_instances_type CHECK (image_type IN ('container', 'virtual-machine')),
    CONSTRAINT ck_instances_resources CHECK (vcpus > 0 AND memory_bytes > 0 AND disk_bytes > 0),
    CONSTRAINT ck_instances_rates CHECK (cpu_hourly_pt >= 0 AND memory_gib_hourly_pt >= 0 AND disk_gib_hourly_pt >= 0 AND max_gpu_hourly_pt >= 0),
    CONSTRAINT ck_instances_billing_mode CHECK (billing_mode IN ('credit', 'admin_exempt')),
    CONSTRAINT ck_instances_desired CHECK (desired_state IN ('running', 'stopped', 'deleted')),
    CONSTRAINT ck_instances_observed CHECK (observed_state IN ('unknown', 'pending', 'stopped', 'running', 'frozen', 'error', 'deleted')),
    CONSTRAINT ck_instances_provisioning CHECK (provisioning_state IN ('pending', 'creating', 'initializing', 'ready', 'failed', 'deleting', 'deleted', 'unknown')),
    CONSTRAINT ck_instances_auto_gpu CHECK (auto_release_gpu IN (0, 1) AND auto_rebind_gpu IN (0, 1)),
    CONSTRAINT ck_instances_parameters CHECK (JSON_TYPE(init_parameters) = 'OBJECT'),
    CONSTRAINT ck_instances_config CHECK (JSON_TYPE(incus_config_snapshot) = 'OBJECT'),
    CONSTRAINT ck_instances_deleted CHECK (deleted_at IS NULL OR desired_state = 'deleted')
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

CREATE TABLE residency_requests (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    instance_id BIGINT UNSIGNED NOT NULL,
    user_id BIGINT UNSIGNED NOT NULL,
    node_id BIGINT UNSIGNED NOT NULL,
    requested_vcpus INT UNSIGNED NOT NULL,
    requested_memory_bytes BIGINT UNSIGNED NOT NULL,
    requested_disk_bytes BIGINT UNSIGNED NOT NULL,
    approved_vcpus INT UNSIGNED NULL,
    approved_memory_bytes BIGINT UNSIGNED NULL,
    approved_disk_bytes BIGINT UNSIGNED NULL,
    approved_billing_mode VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NULL,
    reason TEXT NOT NULL,
    status VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    reviewed_by_user_id BIGINT UNSIGNED NULL,
    review_note TEXT NULL,
    reviewed_at DATETIME(6) NULL,
    starts_at DATETIME(6) NULL,
    expires_at DATETIME(6) NULL,
    revoked_at DATETIME(6) NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    KEY ix_residency_requests_instance_owner_node (instance_id, user_id, node_id),
    KEY ix_residency_requests_user_status (user_id, status),
    KEY ix_residency_requests_node_status (node_id, status),
    KEY ix_residency_requests_reviewer (reviewed_by_user_id),
    CONSTRAINT fk_residency_requests_instance FOREIGN KEY (instance_id, user_id, node_id) REFERENCES instances (id, user_id, node_id),
    CONSTRAINT fk_residency_requests_reviewer FOREIGN KEY (reviewed_by_user_id) REFERENCES users (id),
    CONSTRAINT ck_residency_requests_resources CHECK (requested_vcpus > 0 AND requested_memory_bytes > 0 AND requested_disk_bytes > 0),
    CONSTRAINT ck_residency_requests_status CHECK (status IN ('pending', 'approved', 'rejected', 'cancelled', 'revoked', 'expired')),
    CONSTRAINT ck_residency_requests_review CHECK (status NOT IN ('approved', 'rejected', 'revoked', 'expired') OR (reviewed_by_user_id IS NOT NULL AND reviewed_at IS NOT NULL)),
    CONSTRAINT ck_residency_requests_period CHECK (expires_at IS NULL OR (starts_at IS NOT NULL AND expires_at > starts_at)),
    CONSTRAINT ck_residency_requests_approved CHECK (status NOT IN ('approved', 'revoked', 'expired') OR (starts_at IS NOT NULL AND approved_vcpus IS NOT NULL AND approved_memory_bytes IS NOT NULL AND approved_disk_bytes IS NOT NULL AND approved_billing_mode IS NOT NULL)),
    CONSTRAINT ck_residency_requests_approved_resources CHECK ((approved_vcpus IS NULL OR approved_vcpus > 0) AND (approved_memory_bytes IS NULL OR approved_memory_bytes > 0) AND (approved_disk_bytes IS NULL OR approved_disk_bytes > 0)),
    CONSTRAINT ck_residency_requests_billing_mode CHECK (approved_billing_mode IS NULL OR approved_billing_mode IN ('credit', 'admin_exempt')),
    CONSTRAINT ck_residency_requests_revoked CHECK ((status = 'revoked') = (revoked_at IS NOT NULL))
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

CREATE TABLE gpu_allocations (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    instance_id BIGINT UNSIGNED NOT NULL,
    user_id BIGINT UNSIGNED NOT NULL,
    node_id BIGINT UNSIGNED NOT NULL,
    gpu_id BIGINT UNSIGNED NOT NULL,
    gpu_price_version_id BIGINT UNSIGNED NOT NULL,
    gpu_hourly_pt DECIMAL(20,6) NOT NULL,
    status VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    active_slot TINYINT UNSIGNED GENERATED ALWAYS AS (CASE WHEN status IN ('reserved', 'attaching', 'active', 'releasing', 'unknown') THEN 1 ELSE NULL END) STORED,
    reserved_at DATETIME(6) NOT NULL,
    attached_at DATETIME(6) NULL,
    release_requested_at DATETIME(6) NULL,
    released_at DATETIME(6) NULL,
    release_reason VARCHAR(255) NULL,
    last_verified_at DATETIME(6) NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    UNIQUE KEY uq_gpu_allocations_id_instance_user (id, instance_id, user_id),
    UNIQUE KEY uq_gpu_allocations_active_instance_gpu (instance_id, gpu_id, active_slot),
    KEY ix_gpu_allocations_instance_owner_node (instance_id, user_id, node_id),
    KEY ix_gpu_allocations_gpu_node (gpu_id, node_id),
    KEY ix_gpu_allocations_gpu_active (gpu_id, active_slot),
    KEY ix_gpu_allocations_user_status (user_id, status),
    KEY ix_gpu_allocations_node_status (node_id, status),
    KEY ix_gpu_allocations_price (gpu_price_version_id),
    CONSTRAINT fk_gpu_allocations_instance FOREIGN KEY (instance_id, user_id, node_id) REFERENCES instances (id, user_id, node_id),
    CONSTRAINT fk_gpu_allocations_gpu FOREIGN KEY (gpu_id, node_id) REFERENCES node_gpus (id, node_id),
    CONSTRAINT fk_gpu_allocations_price FOREIGN KEY (gpu_price_version_id) REFERENCES gpu_price_versions (id),
    CONSTRAINT ck_gpu_allocations_rate CHECK (gpu_hourly_pt >= 0),
    CONSTRAINT ck_gpu_allocations_status CHECK (status IN ('reserved', 'attaching', 'active', 'releasing', 'unknown', 'released', 'failed', 'cancelled')),
    CONSTRAINT ck_gpu_allocations_active_release CHECK (status NOT IN ('reserved', 'attaching', 'active', 'releasing', 'unknown') OR released_at IS NULL),
    CONSTRAINT ck_gpu_allocations_released CHECK (status <> 'released' OR released_at IS NOT NULL),
    CONSTRAINT ck_gpu_allocations_attach_time CHECK (attached_at IS NULL OR attached_at >= reserved_at),
    CONSTRAINT ck_gpu_allocations_release_time CHECK (released_at IS NULL OR released_at >= reserved_at)
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;

-- Runtime / billing / networking tables. Parent file creates and selects 101cloud.
-- All *_pt values are exact point amounts, never floating-point currency.
-- Parameters, reports and audit JSON must be redacted before persistence. Never store
-- passwords, redemption plaintext, node tokens, private keys or command output secrets.

CREATE TABLE `tasks` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `task_type` VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `idempotency_scope` VARBINARY(128) NOT NULL,
  `idempotency_key` VARBINARY(191) NOT NULL,
  `user_id` BIGINT UNSIGNED NULL,
  `node_id` BIGINT UNSIGNED NULL,
  `instance_id` BIGINT UNSIGNED NULL,
  `is_instance_mutation` TINYINT UNSIGNED NOT NULL,
  `status` VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `phase` VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
  `attempt_count` INT UNSIGNED NOT NULL DEFAULT 0,
  `lease_owner` VARBINARY(128) NULL,
  `lease_until` DATETIME(6) NULL,
  `next_attempt_at` DATETIME(6) NULL,
  `parameters_redacted` JSON NOT NULL,
  `result_redacted` JSON NULL,
  `error_code` VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NULL,
  `started_at` DATETIME(6) NULL,
  `finished_at` DATETIME(6) NULL,
  `active_mutation_key` BIGINT UNSIGNED GENERATED ALWAYS AS (
    CASE WHEN `is_instance_mutation` = 1
      AND `status` IN ('queued','running','waiting','unknown')
    THEN `instance_id` ELSE NULL END
  ) STORED,
  `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  `updated_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uq_tasks_idempotency` (`idempotency_scope`,`idempotency_key`),
  UNIQUE KEY `uq_tasks_active_mutation` (`active_mutation_key`),
  UNIQUE KEY `uq_tasks_id_instance` (`id`,`instance_id`),
  KEY `ix_tasks_dispatch` (`status`,`next_attempt_at`,`lease_until`),
  KEY `ix_tasks_instance_created` (`instance_id`,`created_at`),
  CONSTRAINT `fk_tasks_user` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`),
  CONSTRAINT `fk_tasks_node` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`),
  CONSTRAINT `fk_tasks_instance_owner_node` FOREIGN KEY (`instance_id`,`user_id`,`node_id`) REFERENCES `instances` (`id`,`user_id`,`node_id`),
  CONSTRAINT `ck_tasks_status` CHECK (`status` IN ('queued','running','waiting','unknown','succeeded','failed','cancelled')),
  CONSTRAINT `ck_tasks_mutation_flag` CHECK (`is_instance_mutation` IN (0,1)),
  CONSTRAINT `ck_tasks_instance_context` CHECK (`instance_id` IS NULL OR (`user_id` IS NOT NULL AND `node_id` IS NOT NULL)),
  CONSTRAINT `ck_tasks_mutation_instance` CHECK (`is_instance_mutation` = 0 OR `instance_id` IS NOT NULL),
  CONSTRAINT `ck_tasks_lease_pair` CHECK ((`lease_owner` IS NULL AND `lease_until` IS NULL) OR (`lease_owner` IS NOT NULL AND `lease_until` IS NOT NULL)),
  CONSTRAINT `ck_tasks_running_lease` CHECK (`status` <> 'running' OR (`lease_owner` IS NOT NULL AND `lease_until` IS NOT NULL)),
  CONSTRAINT `ck_tasks_finished_state` CHECK ((`status` IN ('succeeded','failed','cancelled') AND `finished_at` IS NOT NULL) OR (`status` IN ('queued','running','waiting','unknown') AND `finished_at` IS NULL)),
  CONSTRAINT `ck_tasks_time_order` CHECK (`finished_at` IS NULL OR `started_at` IS NULL OR `finished_at` >= `started_at`),
  CONSTRAINT `ck_tasks_idempotency_nonempty` CHECK (OCTET_LENGTH(`idempotency_scope`) > 0 AND OCTET_LENGTH(`idempotency_key`) > 0)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE `task_steps` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `task_id` BIGINT UNSIGNED NOT NULL,
  `step_key` VARBINARY(128) NOT NULL,
  `attempt` INT UNSIGNED NOT NULL,
  `node_id` BIGINT UNSIGNED NULL,
  `project_name` VARCHAR(63) CHARACTER SET ascii COLLATE ascii_bin NULL,
  `operation_id` VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
  `status` VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `parameters_redacted` JSON NOT NULL,
  `result_redacted` JSON NULL,
  `error_code` VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NULL,
  `started_at` DATETIME(6) NULL,
  `finished_at` DATETIME(6) NULL,
  `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  `updated_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uq_task_steps_attempt` (`task_id`,`step_key`,`attempt`),
  KEY `ix_task_steps_operation` (`node_id`,`project_name`,`operation_id`),
  CONSTRAINT `fk_task_steps_task` FOREIGN KEY (`task_id`) REFERENCES `tasks` (`id`),
  CONSTRAINT `fk_task_steps_node` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`),
  CONSTRAINT `ck_task_steps_attempt` CHECK (`attempt` > 0),
  CONSTRAINT `ck_task_steps_key` CHECK (OCTET_LENGTH(`step_key`) > 0),
  CONSTRAINT `ck_task_steps_status` CHECK (`status` IN ('queued','running','waiting','unknown','succeeded','failed','cancelled')),
  CONSTRAINT `ck_task_steps_operation_context` CHECK (`operation_id` IS NULL OR (`node_id` IS NOT NULL AND `project_name` IS NOT NULL)),
  CONSTRAINT `ck_task_steps_finished_state` CHECK ((`status` IN ('succeeded','failed','cancelled') AND `finished_at` IS NOT NULL) OR (`status` IN ('queued','running','waiting','unknown') AND `finished_at` IS NULL)),
  CONSTRAINT `ck_task_steps_time_order` CHECK (`finished_at` IS NULL OR `started_at` IS NULL OR `finished_at` >= `started_at`)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE `instance_init_runs` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `instance_id` BIGINT UNSIGNED NOT NULL,
  `task_id` BIGINT UNSIGNED NOT NULL,
  `init_template_version_id` BIGINT UNSIGNED NOT NULL,
  `run_number` INT UNSIGNED NOT NULL,
  `status` VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `parameters_redacted` JSON NOT NULL,
  `result_redacted` JSON NULL,
  `exit_code` INT NULL,
  `started_at` DATETIME(6) NULL,
  `finished_at` DATETIME(6) NULL,
  `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  `updated_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (`id`),
  KEY `ix_instance_init_runs_task` (`task_id`),
  UNIQUE KEY `uq_instance_init_runs_number` (`instance_id`,`run_number`),
  CONSTRAINT `fk_instance_init_runs_instance` FOREIGN KEY (`instance_id`) REFERENCES `instances` (`id`),
  CONSTRAINT `fk_instance_init_runs_task_instance` FOREIGN KEY (`task_id`,`instance_id`) REFERENCES `tasks` (`id`,`instance_id`),
  CONSTRAINT `fk_instance_init_runs_template` FOREIGN KEY (`init_template_version_id`) REFERENCES `init_template_versions` (`id`),
  CONSTRAINT `ck_instance_init_runs_number` CHECK (`run_number` > 0),
  CONSTRAINT `ck_instance_init_runs_status` CHECK (`status` IN ('queued','running','unknown','succeeded','failed','cancelled')),
  CONSTRAINT `ck_instance_init_runs_success_exit` CHECK (`status` <> 'succeeded' OR (`exit_code` IS NOT NULL AND `exit_code` = 0)),
  CONSTRAINT `ck_instance_init_runs_finished` CHECK ((`status` IN ('succeeded','failed','cancelled') AND `finished_at` IS NOT NULL) OR (`status` IN ('queued','running','unknown') AND `finished_at` IS NULL)),
  CONSTRAINT `ck_instance_init_runs_time` CHECK (`finished_at` IS NULL OR `started_at` IS NULL OR `finished_at` >= `started_at`)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE `credit_accounts` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `user_id` BIGINT UNSIGNED NOT NULL,
  `credit_policy_version_id` BIGINT UNSIGNED NOT NULL,
  `status` VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `version` BIGINT UNSIGNED NOT NULL DEFAULT 0,
  `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  `updated_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uq_credit_accounts_user` (`user_id`),
  UNIQUE KEY `uq_credit_accounts_id_user` (`id`,`user_id`),
  CONSTRAINT `fk_credit_accounts_user` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`),
  CONSTRAINT `fk_credit_accounts_policy` FOREIGN KEY (`credit_policy_version_id`) REFERENCES `credit_policy_versions` (`id`),
  CONSTRAINT `ck_credit_accounts_status` CHECK (`status` IN ('active','frozen','closed'))
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Buckets represent separate validity windows; remaining includes reserved points.
-- Daily grants and reservation settlement must be serialized using account/version
-- and bucket row locks by the application in the same ledger-writing transaction.
CREATE TABLE `credit_buckets` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `account_id` BIGINT UNSIGNED NOT NULL,
  `source_type` VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `business_key` VARBINARY(191) NOT NULL,
  `granted_pt` DECIMAL(20,6) NOT NULL,
  `remaining_pt` DECIMAL(20,6) NOT NULL,
  `reserved_pt` DECIMAL(20,6) NOT NULL,
  `valid_from` DATETIME(6) NOT NULL,
  `expires_at` DATETIME(6) NULL,
  `version` BIGINT UNSIGNED NOT NULL DEFAULT 0,
  `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  `updated_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uq_credit_buckets_business` (`account_id`,`business_key`),
  UNIQUE KEY `uq_credit_buckets_id_account` (`id`,`account_id`),
  KEY `ix_credit_buckets_spend_order` (`account_id`,`expires_at`,`valid_from`,`id`),
  CONSTRAINT `fk_credit_buckets_account` FOREIGN KEY (`account_id`) REFERENCES `credit_accounts` (`id`),
  CONSTRAINT `ck_credit_buckets_source` CHECK (`source_type` IN ('daily','redemption','admin')),
  CONSTRAINT `ck_credit_buckets_amounts` CHECK (`granted_pt` >= 0 AND `remaining_pt` >= 0 AND `reserved_pt` >= 0 AND `reserved_pt` <= `remaining_pt` AND `remaining_pt` <= `granted_pt`),
  CONSTRAINT `ck_credit_buckets_window` CHECK (`expires_at` IS NULL OR `valid_from` < `expires_at`),
  CONSTRAINT `ck_credit_buckets_daily_expiry` CHECK (`source_type` <> 'daily' OR `expires_at` IS NOT NULL),
  CONSTRAINT `ck_credit_buckets_business_key` CHECK (OCTET_LENGTH(`business_key`) > 0)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- A session captures one configuration/price interval. Reconfiguration closes the
-- old interval and creates a new row. Never mutate snapshots for a running interval.
-- Configuration snapshots use memory_bytes/disk_bytes (not MiB/GiB capacities).
-- Convert bytes to GiB using 1073741824 bytes/GiB for the hourly price snapshots.
CREATE TABLE `usage_sessions` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `account_id` BIGINT UNSIGNED NOT NULL,
  `user_id` BIGINT UNSIGNED NOT NULL,
  `instance_id` BIGINT UNSIGNED NOT NULL,
  `node_id` BIGINT UNSIGNED NOT NULL,
  `charge_kind` VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `gpu_allocation_id` BIGINT UNSIGNED NULL,
  `pricing_version_id` BIGINT UNSIGNED NOT NULL,
  `gpu_price_version_id` BIGINT UNSIGNED NULL,
  `billing_mode` VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `config_snapshot` JSON NOT NULL,
  `cpu_hourly_pt` DECIMAL(20,6) NOT NULL,
  `memory_gib_hourly_pt` DECIMAL(20,6) NOT NULL,
  `disk_gib_hourly_pt` DECIMAL(20,6) NOT NULL,
  `gpu_hourly_pt` DECIMAL(20,6) NOT NULL,
  `status` VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `started_at` DATETIME(6) NOT NULL,
  `last_settled_at` DATETIME(6) NOT NULL,
  `ended_at` DATETIME(6) NULL,
  `active_instance_id` BIGINT UNSIGNED GENERATED ALWAYS AS (
    CASE WHEN `ended_at` IS NULL THEN `instance_id` ELSE NULL END
  ) STORED,
  `allocation_key` BIGINT UNSIGNED GENERATED ALWAYS AS (COALESCE(`gpu_allocation_id`,0)) STORED,
  `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  `updated_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uq_usage_sessions_id_account` (`id`,`account_id`),
  UNIQUE KEY `uq_usage_sessions_active` (`active_instance_id`,`charge_kind`,`allocation_key`),
  KEY `ix_usage_sessions_settlement` (`status`,`last_settled_at`),
  KEY `ix_usage_sessions_instance_time` (`instance_id`,`started_at`),
  CONSTRAINT `fk_usage_sessions_account_user` FOREIGN KEY (`account_id`,`user_id`) REFERENCES `credit_accounts` (`id`,`user_id`),
  CONSTRAINT `fk_usage_sessions_instance_owner_node` FOREIGN KEY (`instance_id`,`user_id`,`node_id`) REFERENCES `instances` (`id`,`user_id`,`node_id`),
  CONSTRAINT `fk_usage_sessions_gpu_owner` FOREIGN KEY (`gpu_allocation_id`,`instance_id`,`user_id`) REFERENCES `gpu_allocations` (`id`,`instance_id`,`user_id`),
  CONSTRAINT `fk_usage_sessions_pricing` FOREIGN KEY (`pricing_version_id`) REFERENCES `pricing_versions` (`id`),
  CONSTRAINT `fk_usage_sessions_gpu_pricing` FOREIGN KEY (`gpu_price_version_id`) REFERENCES `gpu_price_versions` (`id`),
  CONSTRAINT `ck_usage_sessions_kind` CHECK (`charge_kind` IN ('base_compute','gpu','storage')),
  CONSTRAINT `ck_usage_sessions_gpu_context` CHECK ((`charge_kind` = 'gpu' AND `gpu_allocation_id` IS NOT NULL AND `gpu_allocation_id` > 0 AND `gpu_price_version_id` IS NOT NULL) OR (`charge_kind` <> 'gpu' AND `gpu_allocation_id` IS NULL AND `gpu_price_version_id` IS NULL)),
  CONSTRAINT `ck_usage_sessions_billing_mode` CHECK (`billing_mode` IN ('credit','admin_exempt')),
  CONSTRAINT `ck_usage_sessions_rates` CHECK (`cpu_hourly_pt` >= 0 AND `memory_gib_hourly_pt` >= 0 AND `disk_gib_hourly_pt` >= 0 AND `gpu_hourly_pt` >= 0),
  CONSTRAINT `ck_usage_sessions_kind_rates` CHECK (
    (`charge_kind` = 'base_compute' AND `disk_gib_hourly_pt` = 0 AND `gpu_hourly_pt` = 0)
    OR (`charge_kind` = 'storage' AND `cpu_hourly_pt` = 0 AND `memory_gib_hourly_pt` = 0 AND `gpu_hourly_pt` = 0)
    OR (`charge_kind` = 'gpu' AND `cpu_hourly_pt` = 0 AND `memory_gib_hourly_pt` = 0 AND `disk_gib_hourly_pt` = 0)
  ),
  CONSTRAINT `ck_usage_sessions_status` CHECK (`status` IN ('active','ended')),
  CONSTRAINT `ck_usage_sessions_end_state` CHECK ((`status` = 'active' AND `ended_at` IS NULL) OR (`status` = 'ended' AND `ended_at` IS NOT NULL)),
  CONSTRAINT `ck_usage_sessions_cursor` CHECK (`last_settled_at` >= `started_at` AND (`ended_at` IS NULL OR (`ended_at` >= `started_at` AND `last_settled_at` <= `ended_at`)))
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE `usage_charges` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `session_id` BIGINT UNSIGNED NOT NULL,
  `account_id` BIGINT UNSIGNED NOT NULL,
  `from_at` DATETIME(6) NOT NULL,
  `to_at` DATETIME(6) NOT NULL,
  `expected_pt` DECIMAL(20,6) NOT NULL,
  `settled_pt` DECIMAL(20,6) NOT NULL,
  `status` VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `calculation_snapshot` JSON NOT NULL,
  `settled_at` DATETIME(6) NULL,
  `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  `updated_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uq_usage_charges_interval` (`session_id`,`from_at`),
  UNIQUE KEY `uq_usage_charges_id_account` (`id`,`account_id`),
  KEY `ix_usage_charges_status` (`account_id`,`status`,`from_at`),
  CONSTRAINT `fk_usage_charges_session_account` FOREIGN KEY (`session_id`,`account_id`) REFERENCES `usage_sessions` (`id`,`account_id`),
  CONSTRAINT `ck_usage_charges_window` CHECK (`from_at` < `to_at`),
  CONSTRAINT `ck_usage_charges_amounts` CHECK (`expected_pt` >= 0 AND `settled_pt` >= 0 AND `settled_pt` <= `expected_pt`),
  CONSTRAINT `ck_usage_charges_status` CHECK (`status` IN ('pending','settled','insufficient','cancelled')),
  CONSTRAINT `ck_usage_charges_settled` CHECK (`status` <> 'settled' OR (`settled_pt` = `expected_pt` AND `settled_at` IS NOT NULL)),
  CONSTRAINT `ck_usage_charges_insufficient` CHECK (`status` <> 'insufficient' OR `settled_pt` < `expected_pt`)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- One reservation group may span several bucket rows, including midnight. Its
-- window is not clipped to bucket expiry: held funds remain traceable until the
-- application settles/releases them. No automatic midnight forfeiture is encoded.
CREATE TABLE `credit_reservations` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `reservation_key` VARBINARY(191) NOT NULL,
  `account_id` BIGINT UNSIGNED NOT NULL,
  `session_id` BIGINT UNSIGNED NOT NULL,
  `bucket_id` BIGINT UNSIGNED NOT NULL,
  `window_from` DATETIME(6) NOT NULL,
  `window_to` DATETIME(6) NOT NULL,
  `reserved_at` DATETIME(6) NOT NULL,
  `expires_at` DATETIME(6) NOT NULL,
  `reserved_pt` DECIMAL(20,6) NOT NULL,
  `settled_pt` DECIMAL(20,6) NOT NULL,
  `released_pt` DECIMAL(20,6) NOT NULL,
  `status` VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `closed_at` DATETIME(6) NULL,
  `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  `updated_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uq_credit_reservations_group_bucket` (`reservation_key`,`bucket_id`),
  UNIQUE KEY `uq_credit_reservations_id_account_bucket` (`id`,`account_id`,`bucket_id`),
  KEY `ix_credit_reservations_session` (`session_id`,`account_id`),
  KEY `ix_credit_reservations_expiry` (`status`,`expires_at`),
  CONSTRAINT `fk_credit_reservations_account` FOREIGN KEY (`account_id`) REFERENCES `credit_accounts` (`id`),
  CONSTRAINT `fk_credit_reservations_session` FOREIGN KEY (`session_id`,`account_id`) REFERENCES `usage_sessions` (`id`,`account_id`),
  CONSTRAINT `fk_credit_reservations_bucket` FOREIGN KEY (`bucket_id`,`account_id`) REFERENCES `credit_buckets` (`id`,`account_id`),
  CONSTRAINT `ck_credit_reservations_key` CHECK (OCTET_LENGTH(`reservation_key`) > 0),
  CONSTRAINT `ck_credit_reservations_window` CHECK (`window_from` < `window_to` AND `reserved_at` < `expires_at`),
  CONSTRAINT `ck_credit_reservations_amounts` CHECK (`reserved_pt` > 0 AND `settled_pt` >= 0 AND `released_pt` >= 0 AND `settled_pt` + `released_pt` <= `reserved_pt`),
  CONSTRAINT `ck_credit_reservations_status` CHECK (`status` IN ('held','settled','released')),
  CONSTRAINT `ck_credit_reservations_state_amounts` CHECK (
    (`status` = 'held' AND `settled_pt` + `released_pt` < `reserved_pt` AND `closed_at` IS NULL)
    OR (`status` = 'settled' AND `settled_pt` = `reserved_pt` AND `released_pt` = 0 AND `closed_at` IS NOT NULL)
    OR (`status` = 'released' AND `settled_pt` + `released_pt` = `reserved_pt` AND `released_pt` > 0 AND `closed_at` IS NOT NULL)
  ),
  CONSTRAINT `ck_credit_reservations_closed_time` CHECK (`closed_at` IS NULL OR `closed_at` >= `reserved_at`)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- code_hash is a 32-byte cryptographic digest computed before persistence; no
-- plaintext or reversibly encrypted redemption code is retained in this schema.
CREATE TABLE `redemption_codes` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `code_hash` BINARY(32) NOT NULL,
  `display_prefix` VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NULL COMMENT 'Optional non-secret hint, never the full redemption code',
  `grant_pt` DECIMAL(20,6) NOT NULL,
  `max_uses` INT UNSIGNED NOT NULL,
  `used_count` INT UNSIGNED NOT NULL DEFAULT 0,
  `bucket_validity_seconds` BIGINT UNSIGNED NULL COMMENT 'NULL grants a non-expiring bucket',
  `valid_from` DATETIME(6) NOT NULL,
  `expires_at` DATETIME(6) NOT NULL,
  `status` VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `created_by` BIGINT UNSIGNED NOT NULL,
  `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  `updated_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uq_redemption_codes_hash` (`code_hash`),
  KEY `ix_redemption_codes_expiry` (`status`,`expires_at`),
  CONSTRAINT `fk_redemption_codes_creator` FOREIGN KEY (`created_by`) REFERENCES `users` (`id`),
  CONSTRAINT `ck_redemption_codes_grant` CHECK (`grant_pt` > 0),
  CONSTRAINT `ck_redemption_codes_uses` CHECK (`max_uses` > 0 AND `used_count` <= `max_uses`),
  CONSTRAINT `ck_redemption_codes_validity` CHECK ((`bucket_validity_seconds` IS NULL OR `bucket_validity_seconds` > 0) AND `valid_from` < `expires_at`),
  CONSTRAINT `ck_redemption_codes_status` CHECK (`status` IN ('active','disabled'))
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Immutable redemption history. A code can be redeemed once by each user.
CREATE TABLE `redemption_records` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `code_id` BIGINT UNSIGNED NOT NULL,
  `user_id` BIGINT UNSIGNED NOT NULL,
  `account_id` BIGINT UNSIGNED NOT NULL,
  `bucket_id` BIGINT UNSIGNED NOT NULL,
  `granted_pt` DECIMAL(20,6) NOT NULL,
  `redeemed_at` DATETIME(6) NOT NULL,
  `expires_at` DATETIME(6) NULL,
  `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uq_redemption_records_code_user` (`code_id`,`user_id`),
  UNIQUE KEY `uq_redemption_records_bucket` (`bucket_id`),
  UNIQUE KEY `uq_redemption_records_id_account_bucket` (`id`,`account_id`,`bucket_id`),
  CONSTRAINT `fk_redemption_records_code` FOREIGN KEY (`code_id`) REFERENCES `redemption_codes` (`id`),
  CONSTRAINT `fk_redemption_records_account_user` FOREIGN KEY (`account_id`,`user_id`) REFERENCES `credit_accounts` (`id`,`user_id`),
  CONSTRAINT `fk_redemption_records_bucket` FOREIGN KEY (`bucket_id`,`account_id`) REFERENCES `credit_buckets` (`id`,`account_id`),
  CONSTRAINT `ck_redemption_records_grant` CHECK (`granted_pt` > 0),
  CONSTRAINT `ck_redemption_records_expiry` CHECK (`expires_at` IS NULL OR `redeemed_at` < `expires_at`)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Append-only ledger. Signed deltas are intentional; snapshots remain nonnegative.
-- Application transactions write ledger entries together with bucket/version changes.
CREATE TABLE `credit_ledger` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `account_id` BIGINT UNSIGNED NOT NULL,
  `bucket_id` BIGINT UNSIGNED NOT NULL,
  `business_key` VARBINARY(191) NOT NULL,
  `entry_type` VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `amount_delta_pt` DECIMAL(20,6) NOT NULL,
  `reserved_delta_pt` DECIMAL(20,6) NOT NULL,
  `remaining_after_pt` DECIMAL(20,6) NOT NULL,
  `reserved_after_pt` DECIMAL(20,6) NOT NULL,
  `charge_id` BIGINT UNSIGNED NULL,
  `reservation_id` BIGINT UNSIGNED NULL,
  `redemption_record_id` BIGINT UNSIGNED NULL,
  `actor_user_id` BIGINT UNSIGNED NULL,
  `metadata_redacted` JSON NULL,
  `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uq_credit_ledger_business_bucket` (`business_key`,`bucket_id`),
  KEY `ix_credit_ledger_account_time` (`account_id`,`created_at`,`id`),
  CONSTRAINT `fk_credit_ledger_bucket_account` FOREIGN KEY (`bucket_id`,`account_id`) REFERENCES `credit_buckets` (`id`,`account_id`),
  CONSTRAINT `fk_credit_ledger_charge_account` FOREIGN KEY (`charge_id`,`account_id`) REFERENCES `usage_charges` (`id`,`account_id`),
  CONSTRAINT `fk_credit_ledger_reservation` FOREIGN KEY (`reservation_id`,`account_id`,`bucket_id`) REFERENCES `credit_reservations` (`id`,`account_id`,`bucket_id`),
  CONSTRAINT `fk_credit_ledger_redemption` FOREIGN KEY (`redemption_record_id`,`account_id`,`bucket_id`) REFERENCES `redemption_records` (`id`,`account_id`,`bucket_id`),
  CONSTRAINT `fk_credit_ledger_actor` FOREIGN KEY (`actor_user_id`) REFERENCES `users` (`id`),
  CONSTRAINT `ck_credit_ledger_entry_type` CHECK (`entry_type` IN ('grant','reserve','settle','release','expire','adjust','refund')),
  CONSTRAINT `ck_credit_ledger_nonzero` CHECK (`amount_delta_pt` <> 0 OR `reserved_delta_pt` <> 0),
  CONSTRAINT `ck_credit_ledger_balances` CHECK (`remaining_after_pt` >= 0 AND `reserved_after_pt` >= 0 AND `reserved_after_pt` <= `remaining_after_pt`),
  CONSTRAINT `ck_credit_ledger_business_key` CHECK (OCTET_LENGTH(`business_key`) > 0)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Addresses are operator-provided inventory. DDL performs no discovery or prefill.
-- VARBINARY stores packed 4-byte IPv4 or 16-byte IPv6, never textual addresses.
CREATE TABLE `ip_addresses` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `network_pool_id` BIGINT UNSIGNED NOT NULL,
  `address_family` TINYINT UNSIGNED NOT NULL,
  `address` VARBINARY(16) NOT NULL,
  `status` VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `node_id` BIGINT UNSIGNED NULL,
  `node_network_binding_id` BIGINT UNSIGNED NULL,
  `instance_id` BIGINT UNSIGNED NULL,
  `reservation_task_id` BIGINT UNSIGNED NULL,
  `reserved_at` DATETIME(6) NULL,
  `reservation_expires_at` DATETIME(6) NULL,
  `bound_at` DATETIME(6) NULL,
  `last_probe_at` DATETIME(6) NULL,
  `last_probe_status` VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL,
  `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  `updated_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uq_ip_addresses_pool_address` (`network_pool_id`,`address`),
  UNIQUE KEY `uq_ip_addresses_id_address` (`id`,`address`),
  KEY `ix_ip_addresses_available` (`network_pool_id`,`status`,`id`),
  KEY `ix_ip_addresses_reservation_expiry` (`status`,`reservation_expires_at`),
  CONSTRAINT `fk_ip_addresses_pool_family` FOREIGN KEY (`network_pool_id`,`address_family`) REFERENCES `network_pools` (`id`,`address_family`),
  CONSTRAINT `fk_ip_addresses_node` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`),
  CONSTRAINT `fk_ip_addresses_binding_pool_node` FOREIGN KEY (`node_network_binding_id`,`node_id`,`network_pool_id`) REFERENCES `node_network_bindings` (`id`,`node_id`,`network_pool_id`),
  CONSTRAINT `fk_ip_addresses_instance_node` FOREIGN KEY (`instance_id`,`node_id`) REFERENCES `instances` (`id`,`node_id`),
  CONSTRAINT `fk_ip_addresses_task` FOREIGN KEY (`reservation_task_id`) REFERENCES `tasks` (`id`),
  CONSTRAINT `ck_ip_addresses_family_bytes` CHECK ((`address_family` = 4 AND OCTET_LENGTH(`address`) = 4) OR (`address_family` = 6 AND OCTET_LENGTH(`address`) = 16)),
  CONSTRAINT `ck_ip_addresses_status` CHECK (`status` IN ('available','reserved','bound','conflict','quarantined')),
  CONSTRAINT `ck_ip_addresses_probe_status` CHECK (`last_probe_status` IS NULL OR `last_probe_status` IN ('occupied','no_conflict_detected','unknown')),
  CONSTRAINT `ck_ip_addresses_probe_pair` CHECK ((`last_probe_at` IS NULL AND `last_probe_status` IS NULL) OR (`last_probe_at` IS NOT NULL AND `last_probe_status` IS NOT NULL)),
  CONSTRAINT `ck_ip_addresses_reservation_window` CHECK (`reservation_expires_at` IS NULL OR (`reserved_at` IS NOT NULL AND `reserved_at` < `reservation_expires_at`)),
  CONSTRAINT `ck_ip_addresses_binding_context` CHECK (`node_network_binding_id` IS NULL OR `node_id` IS NOT NULL),
  CONSTRAINT `ck_ip_addresses_instance_context` CHECK (`instance_id` IS NULL OR (`node_id` IS NOT NULL AND `node_network_binding_id` IS NOT NULL)),
  CONSTRAINT `ck_ip_addresses_state_context` CHECK (
    (`status` = 'available' AND `instance_id` IS NULL AND `reservation_task_id` IS NULL AND `reserved_at` IS NULL AND `reservation_expires_at` IS NULL AND `bound_at` IS NULL)
    OR (`status` = 'reserved' AND `reservation_task_id` IS NOT NULL AND `node_network_binding_id` IS NOT NULL AND `reserved_at` IS NOT NULL AND `reservation_expires_at` IS NOT NULL AND `bound_at` IS NULL)
    OR (`status` = 'bound' AND `instance_id` IS NOT NULL AND `node_network_binding_id` IS NOT NULL AND `bound_at` IS NOT NULL)
    OR `status` IN ('conflict','quarantined')
  ),
  CONSTRAINT `ck_ip_addresses_bound_time` CHECK (`bound_at` IS NULL OR `reserved_at` IS NULL OR `bound_at` >= `reserved_at`)
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Instance and binding composite FKs enforce the forwarding node. The target IP
-- reference retains the stable address identity, not its mutable current occupant;
-- removed forwards must not prevent address release/reassignment to another instance.
-- Application creation transactions must lock/validate the current IP assignment.
CREATE TABLE `port_forwards` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `node_id` BIGINT UNSIGNED NOT NULL,
  `instance_id` BIGINT UNSIGNED NOT NULL,
  `node_network_binding_id` BIGINT UNSIGNED NOT NULL,
  `listen_address` VARBINARY(16) NOT NULL,
  `protocol` VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `listen_port` SMALLINT UNSIGNED NOT NULL,
  `target_ip_address_id` BIGINT UNSIGNED NOT NULL,
  `target_address` VARBINARY(16) NOT NULL,
  `target_port` SMALLINT UNSIGNED NOT NULL,
  `status` VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `task_id` BIGINT UNSIGNED NULL,
  `cleanup_confirmed_at` DATETIME(6) NULL,
  `active_slot` TINYINT UNSIGNED GENERATED ALWAYS AS (
    CASE WHEN `status` = 'removed' OR (`status` = 'failed' AND `cleanup_confirmed_at` IS NOT NULL)
      THEN NULL ELSE 1 END
  ) STORED,
  `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  `updated_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (`id`),
  UNIQUE KEY `uq_port_forwards_listener` (`node_id`,`listen_address`,`protocol`,`listen_port`,`active_slot`),
  KEY `ix_port_forwards_instance` (`instance_id`,`status`),
  CONSTRAINT `fk_port_forwards_node` FOREIGN KEY (`node_id`) REFERENCES `nodes` (`id`),
  CONSTRAINT `fk_port_forwards_instance_node` FOREIGN KEY (`instance_id`,`node_id`) REFERENCES `instances` (`id`,`node_id`),
  CONSTRAINT `fk_port_forwards_binding_node` FOREIGN KEY (`node_network_binding_id`,`node_id`) REFERENCES `node_network_bindings` (`id`,`node_id`),
  CONSTRAINT `fk_port_forwards_target_address` FOREIGN KEY (`target_ip_address_id`,`target_address`) REFERENCES `ip_addresses` (`id`,`address`),
  CONSTRAINT `fk_port_forwards_task` FOREIGN KEY (`task_id`) REFERENCES `tasks` (`id`),
  CONSTRAINT `ck_port_forwards_protocol` CHECK (`protocol` IN ('tcp','udp')),
  CONSTRAINT `ck_port_forwards_addresses` CHECK (OCTET_LENGTH(`listen_address`) IN (4,16) AND OCTET_LENGTH(`target_address`) IN (4,16)),
  CONSTRAINT `ck_port_forwards_ports` CHECK (`listen_port` BETWEEN 1 AND 65535 AND `target_port` BETWEEN 1 AND 65535),
  CONSTRAINT `ck_port_forwards_cleanup_state` CHECK (`cleanup_confirmed_at` IS NULL OR `status` IN ('removed','failed')),
  CONSTRAINT `ck_port_forwards_status` CHECK (`status` IN ('reserved','applying','active','removing','removed','failed','unknown'))
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Append-only audit events; actor may be NULL for system actions. The JSON is a
-- redacted structured result, not an HTTP body, credential dump or raw shell log.
CREATE TABLE `audit_logs` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `actor_user_id` BIGINT UNSIGNED NULL,
  `task_id` BIGINT UNSIGNED NULL,
  `event_type` VARCHAR(96) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `resource_type` VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `resource_id` BIGINT UNSIGNED NULL,
  `request_id` VARBINARY(128) NULL,
  `result` VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `result_redacted` JSON NOT NULL,
  `occurred_at` DATETIME(6) NOT NULL,
  `created_at` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (`id`),
  KEY `ix_audit_logs_actor_time` (`actor_user_id`,`occurred_at`,`id`),
  KEY `ix_audit_logs_resource_time` (`resource_type`,`resource_id`,`occurred_at`),
  KEY `ix_audit_logs_task` (`task_id`),
  KEY `ix_audit_logs_request` (`request_id`),
  CONSTRAINT `fk_audit_logs_actor` FOREIGN KEY (`actor_user_id`) REFERENCES `users` (`id`),
  CONSTRAINT `fk_audit_logs_task` FOREIGN KEY (`task_id`) REFERENCES `tasks` (`id`),
  CONSTRAINT `ck_audit_logs_result` CHECK (`result` IN ('succeeded','failed','denied','unknown'))
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Container metric history is a privacy-safe snapshot, not billing or release authorization.
-- Host Agent only exposes snapshots. A future backend collector must resolve instance IDs,
-- ingest/deduplicate samples, build rollups, and implement retention; this DDL does none of these.
-- Raw and rollup retention days and bucket sizes are independently configurable, never hardcoded.
-- All metric timestamps use UTC+8 (+08:00); convert Agent timestamps before insertion. Missing/unsupported observations remain NULL, never synthetic zero.
-- No PID, process name, command line, filesystem path, or unredacted payload JSON is stored here.
-- Historical instance_id and node_id references are independent: moving an instance must not
-- rewrite its history or be blocked by a composite FK to instances' current node_id.
-- No partitioning: MySQL InnoDB partitioning is incompatible with these foreign keys.
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
    last_sampled_at DATETIME(6) NULL COMMENT 'UTC+8 sample time; NULL before any sample has been ingested',
    last_ingested_at DATETIME(6) NOT NULL COMMENT 'UTC+8 time of the last committed ingestion checkpoint',
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (node_id, project_scope),
    CONSTRAINT fk_nmc_node FOREIGN KEY (node_id) REFERENCES nodes (id),
    CONSTRAINT ck_nmc_scope CHECK (CHAR_LENGTH(project_scope) > 0)
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
