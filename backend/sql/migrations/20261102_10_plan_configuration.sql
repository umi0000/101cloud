SET time_zone = '+08:00';

-- Placement remains selected by the network binding; plan_nodes makes the
-- corresponding node boundary explicit without relocating existing instances.
CREATE TABLE plan_nodes (
 plan_id BIGINT UNSIGNED NOT NULL,
 node_id BIGINT UNSIGNED NOT NULL,
 PRIMARY KEY(plan_id,node_id),
 KEY ix_plan_nodes_node(node_id,plan_id),
 CONSTRAINT fk_plan_nodes_plan FOREIGN KEY(plan_id) REFERENCES instance_plans(id),
 CONSTRAINT fk_plan_nodes_node FOREIGN KEY(node_id) REFERENCES nodes(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
INSERT INTO plan_nodes(plan_id,node_id)
 SELECT DISTINCT p.plan_id,b.node_id
 FROM plan_network_options p JOIN node_network_bindings b ON b.id=p.network_option_id;

ALTER TABLE instance_plans
 ADD COLUMN gpu_policy JSON NULL,
 ADD COLUMN allowed_user_actions JSON NULL,
 ADD COLUMN gpu_user_permissions JSON NULL;

-- Existing offerings keep the legacy action/permission behavior. New catalog
-- writes instead default to start/stop/restart/console and no editable GPU fields.
-- VM offerings cannot gain GPU actions or editable GPU fields through migration.
UPDATE instance_plans p
 LEFT JOIN policy_versions idle ON idle.id=CAST(JSON_UNQUOTE(JSON_EXTRACT(p.policy_versions,'$."gpu-idle"')) AS UNSIGNED) AND idle.kind='gpu-idle'
 SET p.gpu_max_hourly_pt=COALESCE(p.gpu_max_hourly_pt,0),
 p.gpu_policy=JSON_OBJECT(
  'idle',JSON_OBJECT('enabled',JSON_EXTRACT('false','$'),
   'timeout_seconds',COALESCE(CAST(JSON_UNQUOTE(JSON_EXTRACT(idle.config,'$.default_timeout_seconds')) AS UNSIGNED),1800),
   'action',CASE WHEN JSON_UNQUOTE(JSON_EXTRACT(idle.config,'$.allowed_actions[0]'))='stop_instance' THEN 'stop_instance' ELSE 'detach_gpu' END),
  'rebalance',JSON_OBJECT('enabled',JSON_EXTRACT('false','$'),'allow_live',JSON_EXTRACT('false','$'),'max_hourly_pt',CAST(COALESCE(p.gpu_max_hourly_pt,0) AS CHAR))),
 p.allowed_user_actions=CASE WHEN p.image_type='virtual-machine'
  THEN JSON_ARRAY('start','stop','restart','resize','delete','console')
  ELSE JSON_ARRAY('start','stop','restart','resize','delete','gpu_attach','gpu_detach','gpu_rebind','gpu_policy','console') END,
 p.gpu_user_permissions=CASE WHEN p.image_type='virtual-machine' THEN JSON_OBJECT()
  ELSE JSON_OBJECT('idle',JSON_EXTRACT('true','$'),'idle_timeout',JSON_EXTRACT('true','$'),'idle_action',JSON_EXTRACT('true','$'),'rebalance',JSON_EXTRACT('true','$'),'allow_live',JSON_EXTRACT('true','$'),'max_hourly_pt',JSON_EXTRACT('true','$')) END;

ALTER TABLE instance_plans
 MODIFY COLUMN gpu_policy JSON NOT NULL,
 MODIFY COLUMN allowed_user_actions JSON NOT NULL,
 MODIFY COLUMN gpu_user_permissions JSON NOT NULL,
 ADD CONSTRAINT ck_plan_gpu_policy CHECK(JSON_TYPE(gpu_policy)='OBJECT'),
 ADD CONSTRAINT ck_plan_user_actions CHECK(JSON_TYPE(allowed_user_actions)='ARRAY'),
 ADD CONSTRAINT ck_plan_gpu_permissions CHECK(JSON_TYPE(gpu_user_permissions)='OBJECT');

-- Existing instances retain their own frozen configuration, including explicit
-- empty arrays/maps and enabled policies. Repair only absent legacy JSON values;
-- do not copy a plan's current policy onto an already-running instance.
UPDATE instances SET allowed_user_actions=JSON_ARRAY('start','stop','restart','resize','delete','gpu_attach','gpu_detach','gpu_rebind','gpu_policy','console')
 WHERE product_type='elastic' AND (allowed_user_actions IS NULL OR JSON_TYPE(allowed_user_actions)='NULL');
UPDATE instances SET gpu_user_permissions=JSON_OBJECT('idle',JSON_EXTRACT('true','$'),'idle_timeout',JSON_EXTRACT('true','$'),'idle_action',JSON_EXTRACT('true','$'),'rebalance',JSON_EXTRACT('true','$'),'allow_live',JSON_EXTRACT('true','$'),'max_hourly_pt',JSON_EXTRACT('true','$'))
 WHERE product_type='elastic' AND (gpu_user_permissions IS NULL OR JSON_TYPE(gpu_user_permissions)='NULL');
UPDATE instances SET gpu_policy=JSON_OBJECT('idle',JSON_OBJECT('enabled',JSON_EXTRACT('false','$'),'timeout_seconds',1800,'action','detach_gpu'),'rebalance',JSON_OBJECT('enabled',JSON_EXTRACT('false','$'),'allow_live',JSON_EXTRACT('false','$'),'max_hourly_pt',CAST(COALESCE(max_gpu_hourly_pt,0) AS CHAR)))
 WHERE product_type='elastic' AND (gpu_policy IS NULL OR JSON_TYPE(gpu_policy)='NULL');
