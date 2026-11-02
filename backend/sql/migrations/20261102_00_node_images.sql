-- Historical init.sql is checksum-pinned. New installations and upgrades both
-- acquire node availability through this migration, never by assuming readiness.
CREATE TABLE IF NOT EXISTS image_nodes (
 id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
 image_id BIGINT UNSIGNED NOT NULL,
 node_id BIGINT UNSIGNED NOT NULL,
 project_name VARCHAR(63) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 architecture VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 fingerprint BINARY(32) NOT NULL,
 cloud_init BOOLEAN NOT NULL DEFAULT FALSE,
 gpu_ready BOOLEAN NOT NULL DEFAULT FALSE,
 source_id VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL,
 variant VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
 properties JSON NOT NULL,
 status VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'pending',
 task_id BIGINT UNSIGNED NULL,
 last_verified_at DATETIME(6) NULL,
 error JSON NULL,
 revision BIGINT UNSIGNED NOT NULL DEFAULT 1,
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
 PRIMARY KEY (id),
 UNIQUE KEY uq_image_nodes_image_node_project (image_id,node_id,project_name),
 KEY ix_image_nodes_node_status (node_id,project_name,status),
 KEY ix_image_nodes_task (task_id),
 CONSTRAINT fk_image_nodes_image FOREIGN KEY (image_id) REFERENCES images(id),
 CONSTRAINT fk_image_nodes_node FOREIGN KEY (node_id) REFERENCES nodes(id),
 CONSTRAINT fk_image_nodes_task FOREIGN KEY (task_id) REFERENCES tasks(id),
 CONSTRAINT ck_image_nodes_names CHECK (CHAR_LENGTH(project_name)>0 AND CHAR_LENGTH(architecture)>0),
 CONSTRAINT ck_image_nodes_flags CHECK (cloud_init IN (0,1) AND gpu_ready IN (0,1)),
 CONSTRAINT ck_image_nodes_source CHECK (source_id IS NULL OR source_id='images'),
 CONSTRAINT ck_image_nodes_properties CHECK (JSON_TYPE(properties)='OBJECT'),
 CONSTRAINT ck_image_nodes_error CHECK (error IS NULL OR JSON_TYPE(error)='OBJECT'),
 CONSTRAINT ck_image_nodes_status CHECK (status IN ('pending','downloading','ready','failed','unknown'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Legacy local entries have no native evidence yet. Do not migrate old remote
-- URLs into trusted sources or alter frozen instance fingerprints/architectures.
INSERT INTO image_nodes (image_id,node_id,project_name,architecture,fingerprint,cloud_init,gpu_ready,source_id,variant,properties,status)
SELECT i.id,n.id,n.default_project_name,i.architecture,i.fingerprint,
 COALESCE(JSON_EXTRACT(i.properties,'$.cloud_init')=CAST('true' AS JSON),FALSE),
 i.gpu_ready,NULL,'',i.properties,'pending'
FROM images i CROSS JOIN nodes n
WHERE i.source_type='local'
 AND NOT EXISTS (SELECT 1 FROM image_nodes x WHERE x.image_id=i.id AND x.node_id=n.id AND x.project_name=n.default_project_name);

-- Dedicated idempotency scope permits safe resume after a partially applied DDL
-- migration without colliding with administrator requests or existing tasks.
INSERT INTO tasks (task_type,idempotency_scope,idempotency_key,user_id,node_id,instance_id,is_instance_mutation,status,parameters_redacted)
SELECT 'image.prepare','migration:20261102_00_node_images',CAST(x.id AS CHAR),i.created_by_user_id,x.node_id,NULL,0,'queued',JSON_OBJECT('image_node_id',CAST(x.id AS CHAR))
FROM image_nodes x JOIN images i ON i.id=x.image_id
WHERE i.source_type='local' AND x.source_id IS NULL AND x.status='pending' AND x.task_id IS NULL
 AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.idempotency_scope='migration:20261102_00_node_images' AND t.idempotency_key=CAST(x.id AS CHAR));

UPDATE image_nodes x JOIN tasks t
 ON t.idempotency_scope='migration:20261102_00_node_images' AND t.idempotency_key=CAST(x.id AS CHAR)
SET x.task_id=t.id
WHERE x.task_id IS NULL;
