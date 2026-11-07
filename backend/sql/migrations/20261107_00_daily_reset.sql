-- Existing rows must be normalized by the Go business-calendar cutover before
-- serving. Persist completion so later policy clock edits cannot rewrite expiry.
ALTER TABLE credit_daily_grants
 ADD COLUMN reset_capped BOOLEAN NOT NULL DEFAULT FALSE,
 ADD CONSTRAINT ck_daily_grant_reset_capped CHECK (reset_capped IN (0,1));
