-- Credentials are deliberately separate from Incus/task/audit/DTO snapshots.
-- Only a salted SHA-512 crypt password hash and normalized public keys are sealed
-- with the existing application MasterKey (AES-GCM); no plaintext password.
-- NULL preserves the existing API and guest initialization behavior.
ALTER TABLE instances
 ADD COLUMN login_config_ciphertext MEDIUMBLOB NULL COMMENT 'MasterKey-encrypted guest login configuration; sensitive, never expose in DTOs',
 ADD COLUMN login_initialized_at DATETIME(6) NULL COMMENT 'First successful cloud-init completion, independent of administrator templates';
