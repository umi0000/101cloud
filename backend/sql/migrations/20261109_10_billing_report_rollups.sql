-- Derived read model only. Financial values, revisions and timestamps stay intact.
ALTER TABLE usage_charges
  ADD COLUMN report_rollup_complete BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN report_rollup_applied BOOLEAN NOT NULL DEFAULT FALSE,
  ADD KEY ix_usage_charges_rollup (account_id,report_rollup_complete,to_at,from_at),
  ADD KEY ix_usage_charges_rollup_pending (report_rollup_complete,account_id,id),
  DROP INDEX ix_usage_charges_report;

CREATE TABLE billing_report_hours (
 account_id BIGINT UNSIGNED NOT NULL,
 instance_id BIGINT UNSIGNED NOT NULL,
 bucket_start DATETIME(6) NOT NULL,
 recorded DECIMAL(40,6) NOT NULL DEFAULT 0,
 cpu DECIMAL(40,6) NOT NULL DEFAULT 0,
 memory DECIMAL(40,6) NOT NULL DEFAULT 0,
 disk DECIMAL(40,6) NOT NULL DEFAULT 0,
 gpu DECIMAL(40,6) NOT NULL DEFAULT 0,
 PRIMARY KEY(account_id,bucket_start,instance_id),
 KEY ix_billing_report_hours_instance(account_id,instance_id,bucket_start)
) ENGINE=InnoDB;
CREATE TABLE billing_report_days LIKE billing_report_hours;

-- Monotonic proof that a partial trailing hour contains no processed future
-- usage. Cancellation need not lower this conservative watermark.
CREATE TABLE billing_report_state (
 account_id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
 max_to_at DATETIME(6) NOT NULL
) ENGINE=InnoDB;
