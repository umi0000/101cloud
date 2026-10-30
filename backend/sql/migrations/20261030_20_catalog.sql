SET time_zone = '+08:00';

ALTER TABLE nodes
 ADD COLUMN administrative_state VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'maintenance',
 ADD COLUMN health VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'unknown',
 ADD COLUMN connection_verified_at DATETIME(6) NULL,
 ADD CONSTRAINT ck_nodes_administrative_state CHECK (administrative_state IN ('active','disabled','draining','maintenance')),
 ADD CONSTRAINT ck_nodes_health CHECK (health IN ('unknown','online','offline'));
UPDATE nodes SET administrative_state=CASE WHEN status='offline' THEN 'maintenance' ELSE status END,health=CASE WHEN status='offline' THEN 'offline' ELSE 'unknown' END;
ALTER TABLE node_gpus MODIFY COLUMN memory_mib BIGINT UNSIGNED NULL;
ALTER TABLE node_network_bindings
 ADD COLUMN validation_status VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'pending',
 ADD COLUMN validated_at DATETIME(6) NULL,
 ADD COLUMN validation_details JSON NULL,
 ADD CONSTRAINT ck_binding_validation CHECK (validation_status IN ('pending','verified','failed'));
UPDATE node_network_bindings SET status='disabled';
ALTER TABLE instance_plans
 DROP CHECK ck_instance_plans_status,
 ADD COLUMN allowed_overrides JSON NULL,
 ADD COLUMN policy_versions JSON NULL,
 ADD COLUMN gpu_access_mode VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NULL,
 ADD COLUMN gpu_max_hourly_pt DECIMAL(20,6) NULL,
 ADD CONSTRAINT ck_instance_plans_status CHECK (status IN ('draft','active','disabled','retired')),
 ADD CONSTRAINT ck_plan_overrides CHECK (allowed_overrides IS NULL OR JSON_TYPE(allowed_overrides)='OBJECT'),
 ADD CONSTRAINT ck_plan_policy_versions CHECK (policy_versions IS NULL OR JSON_TYPE(policy_versions)='OBJECT'),
 ADD CONSTRAINT ck_plan_gpu_mode CHECK (gpu_access_mode IS NULL OR gpu_access_mode IN ('shared','exclusive')),
 ADD CONSTRAINT ck_plan_gpu_price_limit CHECK (gpu_max_hourly_pt IS NULL OR gpu_max_hourly_pt >= 0);

CREATE TABLE plan_images (
 plan_id BIGINT UNSIGNED NOT NULL,
 image_id BIGINT UNSIGNED NOT NULL,
 PRIMARY KEY(plan_id,image_id),
 CONSTRAINT fk_plan_images_plan FOREIGN KEY(plan_id) REFERENCES instance_plans(id),
 CONSTRAINT fk_plan_images_image FOREIGN KEY(image_id) REFERENCES images(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
CREATE TABLE plan_network_options (
 plan_id BIGINT UNSIGNED NOT NULL,
 network_option_id BIGINT UNSIGNED NOT NULL,
 PRIMARY KEY(plan_id,network_option_id),
 CONSTRAINT fk_plan_network_plan FOREIGN KEY(plan_id) REFERENCES instance_plans(id),
 CONSTRAINT fk_plan_network_binding FOREIGN KEY(network_option_id) REFERENCES node_network_bindings(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
CREATE TABLE node_unmanaged_instances (
 id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
 node_id BIGINT UNSIGNED NOT NULL,
 project_name VARCHAR(63) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 incus_name VARCHAR(63) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 incus_uuid CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
 instance_type VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 architecture VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 observed_state VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 config JSON NOT NULL,
 observed_at DATETIME(6) NOT NULL,
 revision BIGINT UNSIGNED NOT NULL DEFAULT 1,
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
 PRIMARY KEY(id),
 UNIQUE KEY uq_node_unmanaged_name(node_id,project_name,incus_name),
 KEY ix_node_unmanaged_page(node_id,created_at,id),
 CONSTRAINT fk_unmanaged_node FOREIGN KEY(node_id) REFERENCES nodes(id),
 CONSTRAINT ck_unmanaged_config CHECK(JSON_TYPE(config)='OBJECT')
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
