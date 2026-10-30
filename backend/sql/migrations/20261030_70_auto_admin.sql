-- Record first administrator initialization independently of account role/status.
ALTER TABLE auth_admin_guard ADD COLUMN initialized_at DATETIME(6) NULL;
UPDATE auth_admin_guard SET initialized_at=CURRENT_TIMESTAMP(6)
WHERE id=1 AND (
 EXISTS(SELECT 1 FROM users WHERE role='admin')
 OR EXISTS(SELECT 1 FROM audit_logs WHERE event_type='auth.bootstrap')
);
