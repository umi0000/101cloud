-- Retire unused NAT product state without deleting existing forwarding history.
-- A populated legacy table requires an explicit operator archive before upgrade.
SET SESSION time_zone = '+08:00';
CREATE TEMPORARY TABLE backend_nat_retirement_guard (
    existing_rows BIGINT NOT NULL,
    CONSTRAINT ck_backend_nat_history_empty CHECK (existing_rows = 0)
);
INSERT INTO backend_nat_retirement_guard SELECT COUNT(*) FROM port_forwards;
DROP TEMPORARY TABLE backend_nat_retirement_guard;
DROP TABLE port_forwards;
ALTER TABLE user_limits DROP COLUMN max_port_forwards;
