-- Registration is opt-in on both fresh installations and upgrades.
-- This reference is the public policy_versions ID, not the allowance source ID.
CREATE TABLE registration_settings (
    id TINYINT UNSIGNED NOT NULL,
    revision BIGINT UNSIGNED NOT NULL DEFAULT 1,
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    credit_policy_version_id BIGINT UNSIGNED NULL,
    max_instances INT UNSIGNED NOT NULL DEFAULT 0,
    max_vcpus INT UNSIGNED NOT NULL DEFAULT 0,
    max_memory_bytes BIGINT UNSIGNED NOT NULL DEFAULT 0,
    max_disk_bytes BIGINT UNSIGNED NOT NULL DEFAULT 0,
    max_gpu_allocations INT UNSIGNED NOT NULL DEFAULT 0,
    max_resident_instances INT UNSIGNED NOT NULL DEFAULT 0,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    PRIMARY KEY (id),
    CONSTRAINT ck_registration_settings_singleton CHECK (id = 1),
    CONSTRAINT ck_registration_settings_enabled CHECK (enabled IN (0, 1)),
    CONSTRAINT ck_registration_settings_policy CHECK (enabled = 0 OR credit_policy_version_id IS NOT NULL),
    CONSTRAINT ck_registration_settings_resident CHECK (max_resident_instances <= max_instances),
    CONSTRAINT fk_registration_settings_policy FOREIGN KEY (credit_policy_version_id) REFERENCES policy_versions (id)
) ENGINE=InnoDB;

INSERT INTO registration_settings (id) VALUES (1);
