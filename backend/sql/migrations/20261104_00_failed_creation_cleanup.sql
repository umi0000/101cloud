-- Failed, never-delivered creations remain as internal retirement/audit rows.
ALTER TABLE instances
  ADD COLUMN creation_failed_at DATETIME(6) NULL,
  ADD COLUMN creation_failure_task_id BIGINT UNSIGNED NULL,
  ADD COLUMN creation_cleanup_state VARCHAR(16) NULL,
  ADD KEY ix_instances_creation_failed (creation_failed_at,id),
  ADD CONSTRAINT fk_instances_creation_failure_task FOREIGN KEY (creation_failure_task_id) REFERENCES tasks(id),
  ADD CONSTRAINT ck_instances_creation_cleanup_state CHECK (creation_cleanup_state IS NULL OR creation_cleanup_state IN ('pending','cleaning','unknown','completed'));
