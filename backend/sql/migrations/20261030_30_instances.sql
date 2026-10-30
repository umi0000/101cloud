SET time_zone = '+08:00';
ALTER TABLE instances
 MODIFY instance_plan_id BIGINT UNSIGNED NULL,
 ADD product_type VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'elastic',
 ADD allowed_user_actions JSON NULL,
 ADD gpu_user_permissions JSON NULL,
 ADD custom_reason TEXT NULL,
 ADD policy_versions JSON NULL,
 ADD gpu_policy JSON NULL,
 ADD idle_since DATETIME(6) NULL,
 ADD last_rebound_at DATETIME(6) NULL,
 ADD import_task_id BIGINT UNSIGNED NULL,
 ADD UNIQUE KEY uq_instances_import_task(import_task_id),
 ADD CONSTRAINT fk_instances_import_task FOREIGN KEY(import_task_id) REFERENCES tasks(id),
 ADD CONSTRAINT ck_instances_product CHECK(product_type IN ('elastic','custom')),
 ADD CONSTRAINT ck_instances_product_plan CHECK(product_type='custom' OR instance_plan_id IS NOT NULL);
UPDATE instances SET allowed_user_actions=JSON_ARRAY('start','stop','restart','resize','delete','gpu_attach','gpu_detach','gpu_rebind','gpu_policy','console'),gpu_user_permissions=JSON_OBJECT('idle',true,'idle_timeout',true,'idle_action',true,'rebalance',true,'allow_live',true,'max_hourly_pt',true),policy_versions=JSON_OBJECT(),gpu_policy=JSON_OBJECT('idle',JSON_OBJECT('enabled',false,'timeout_seconds',1800,'action','detach_gpu'),'rebalance',JSON_OBJECT('enabled',false,'allow_live',false,'max_hourly_pt','0.000000'));
UPDATE instances SET product_type='custom',allowed_user_actions=JSON_ARRAY(),gpu_user_permissions=JSON_OBJECT(),custom_reason='Existing administrator-exempt instance' WHERE billing_mode='admin_exempt';
ALTER TABLE instances MODIFY allowed_user_actions JSON NOT NULL,MODIFY gpu_user_permissions JSON NOT NULL,MODIFY policy_versions JSON NOT NULL,MODIFY gpu_policy JSON NOT NULL;
ALTER TABLE gpu_allocations ADD access_mode VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'shared',ADD configured BOOLEAN NULL DEFAULT NULL,ADD observed BOOLEAN NULL DEFAULT NULL,ADD release_verified BOOLEAN NOT NULL DEFAULT FALSE,ADD CONSTRAINT ck_gpu_allocation_access CHECK(access_mode IN ('shared','exclusive'));
UPDATE gpu_allocations a JOIN gpu_price_versions p ON p.id=a.gpu_price_version_id SET a.access_mode=p.access_mode;
CREATE TABLE custom_instance_requests (
 id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
 user_id BIGINT UNSIGNED NOT NULL,
 name VARCHAR(128) NOT NULL,
 reason TEXT NOT NULL,
 requested_config JSON NOT NULL,
 approved_config JSON NULL,
 status VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'pending',
 reviewed_by_user_id BIGINT UNSIGNED NULL,
 review_note TEXT NULL,
 reviewed_at DATETIME(6) NULL,
 instance_id BIGINT UNSIGNED NULL,
 delivery_task_id BIGINT UNSIGNED NULL,
 revision BIGINT UNSIGNED NOT NULL DEFAULT 1,
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
 PRIMARY KEY(id),KEY ix_custom_request_user_status(user_id,status,created_at,id),KEY ix_custom_request_status(status,created_at,id),
 CONSTRAINT fk_custom_request_user FOREIGN KEY(user_id) REFERENCES users(id),
 CONSTRAINT fk_custom_request_reviewer FOREIGN KEY(reviewed_by_user_id) REFERENCES users(id),
 CONSTRAINT fk_custom_request_instance FOREIGN KEY(instance_id,user_id) REFERENCES instances(id,user_id),
 CONSTRAINT fk_custom_request_task FOREIGN KEY(delivery_task_id,instance_id) REFERENCES tasks(id,instance_id),
 CONSTRAINT ck_custom_request_status CHECK(status IN ('pending','approved','rejected','cancelled')),
 CONSTRAINT ck_custom_request_config CHECK(JSON_TYPE(requested_config)='OBJECT' AND (approved_config IS NULL OR JSON_TYPE(approved_config)='OBJECT')),
 CONSTRAINT ck_custom_request_review CHECK(status NOT IN ('approved','rejected') OR (reviewed_at IS NOT NULL AND reviewed_by_user_id IS NOT NULL)),
 CONSTRAINT ck_custom_request_approved CHECK(status<>'approved' OR (approved_config IS NOT NULL AND instance_id IS NOT NULL AND delivery_task_id IS NOT NULL))
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
