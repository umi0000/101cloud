-- Remove availability zones: administrators place instances by selecting a node.
-- Node, network pool and plan rows are preserved; only their zone reference is dropped.
-- Network pool names become globally unique. Refuse BEFORE any DDL if existing
-- pools in different zones share a name, rather than renaming operator data.
-- JSON_EXTRACT deliberately raises a SQL error on the unsafe branch.
SELECT JSON_EXTRACT(CASE WHEN EXISTS(
 SELECT 1 FROM network_pools GROUP BY name HAVING COUNT(*) > 1
) THEN 'Network pool names must be unique before availability zones are removed; rename duplicates explicitly'
 ELSE '{}' END, '$');

-- MySQL requires dropping a foreign key before the index that supports it.
ALTER TABLE nodes DROP FOREIGN KEY fk_nodes_zone;
ALTER TABLE network_pools DROP FOREIGN KEY fk_network_pools_zone;
ALTER TABLE instance_plans DROP FOREIGN KEY fk_instance_plans_zone;

ALTER TABLE nodes
 DROP INDEX ix_nodes_zone_status,
 DROP COLUMN availability_zone_id,
 ADD KEY ix_nodes_status(status);
ALTER TABLE network_pools
 DROP INDEX uq_network_pools_zone_name,
 DROP INDEX ix_network_pools_zone_status,
 DROP COLUMN availability_zone_id,
 ADD UNIQUE KEY uq_network_pools_name(name),
 ADD KEY ix_network_pools_status(status);
ALTER TABLE instance_plans
 DROP INDEX ix_instance_plans_zone_name,
 DROP COLUMN availability_zone_id,
 ADD KEY ix_instance_plans_name(name);

DROP TABLE availability_zones;
