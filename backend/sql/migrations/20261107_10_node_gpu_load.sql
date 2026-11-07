CREATE TABLE node_gpu_load_snapshots (
    node_id BIGINT UNSIGNED NOT NULL,
    sampled_at DATETIME(6) NOT NULL,
    resources JSON NOT NULL,
    PRIMARY KEY (node_id),
    CONSTRAINT fk_node_gpu_load_node FOREIGN KEY (node_id) REFERENCES nodes(id) ON DELETE CASCADE,
    CONSTRAINT ck_node_gpu_load_resources CHECK (JSON_TYPE(resources) = 'OBJECT')
) ENGINE=InnoDB;
