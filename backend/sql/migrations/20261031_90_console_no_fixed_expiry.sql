-- Remove only the console-specific fixed lifetime; login and billing expiry remain intact.
-- Historical statuses, including expired, and all existing rows are preserved.
ALTER TABLE console_sessions
 DROP INDEX ix_console_user_status_expiry,
 DROP INDEX ix_console_status_expiry,
 DROP COLUMN expires_at,
 ADD KEY ix_console_user_status(user_id,status);
