ALTER TABLE tasks
  MODIFY COLUMN active_mutation_key BIGINT UNSIGNED GENERATED ALWAYS AS (
    CASE WHEN is_instance_mutation = 1
      AND (status IN ('queued','running','waiting','unknown')
        OR (status = 'cancelled' AND error_code = 'task_force_stopped'))
    THEN instance_id ELSE NULL END
  ) STORED;
