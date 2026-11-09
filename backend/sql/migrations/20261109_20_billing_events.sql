-- Forward-only event history: existing financial records are not backfilled.
-- Instance identity is a snapshot, deliberately without an instance foreign key.
CREATE TABLE billing_events (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    user_id BIGINT UNSIGNED NOT NULL,
    account_id BIGINT UNSIGNED NULL,
    business_key VARCHAR(191) NOT NULL,
    event_type VARCHAR(32) NOT NULL,
    published BOOLEAN NOT NULL DEFAULT TRUE,
    occurred_at DATETIME(6) NOT NULL,
    instance_id BIGINT UNSIGNED NULL,
    instance_name VARCHAR(255) NULL,
    grant_source VARCHAR(32) NULL,
    amount_pt DECIMAL(30,6) NULL,
    available_before_pt DECIMAL(30,6) NULL,
    available_after_pt DECIMAL(30,6) NULL,
    hourly_before_pt DECIMAL(30,6) NULL,
    hourly_after_pt DECIMAL(30,6) NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_billing_events_business (user_id, business_key),
    KEY ix_billing_events_user_time (user_id, published, occurred_at, id)
) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
