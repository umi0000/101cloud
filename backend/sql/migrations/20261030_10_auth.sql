-- Authentication state only. Base migration supplies revision to original tables.
-- Existing accounts without quotas receive explicit deny-all limits, never unlimited access.
INSERT INTO user_limits (user_id, max_instances, max_vcpus, max_memory_bytes, max_disk_bytes, max_gpu_allocations, max_resident_instances, max_port_forwards)
SELECT u.id, 0, 0, 0, 0, 0, 0, 0 FROM users u
LEFT JOIN user_limits l ON l.user_id = u.id WHERE l.id IS NULL;

CREATE TABLE auth_admin_guard (
    id TINYINT UNSIGNED NOT NULL,
    PRIMARY KEY (id),
    CONSTRAINT ck_auth_admin_guard_singleton CHECK (id = 1)
) ENGINE=InnoDB;
INSERT INTO auth_admin_guard (id) VALUES (1);

-- Hashed dimensions protect usernames/IPs; fixed windows survive process restarts.
CREATE TABLE auth_rate_limits (
    bucket_key BINARY(32) NOT NULL,
    window_start DATETIME(6) NOT NULL,
    attempts INT UNSIGNED NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    PRIMARY KEY (bucket_key),
    KEY ix_auth_rate_limits_expiry (expires_at)
) ENGINE=InnoDB;
