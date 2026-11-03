SET time_zone = '+08:00';

-- Durable account-local retry marker. The account row lock serializes grants,
-- spending and settlement; no task lease can strand a repayment after a crash.
ALTER TABLE credit_accounts
 ADD COLUMN settlement_pending BOOLEAN NOT NULL DEFAULT FALSE,
 ADD KEY ix_credit_accounts_settlement(settlement_pending,id);
ALTER TABLE usage_charges
 ADD KEY ix_usage_charges_account_status_id(account_id,status,id);
UPDATE credit_accounts a SET settlement_pending = TRUE
 WHERE EXISTS (SELECT 1 FROM usage_charges c WHERE c.account_id=a.id AND c.status='insufficient'
 AND (EXISTS (SELECT 1 FROM credit_buckets b WHERE b.account_id=a.id AND b.remaining_pt>b.reserved_pt AND b.valid_from<=NOW(6) AND (b.expires_at IS NULL OR b.expires_at>NOW(6)))
 OR EXISTS (SELECT 1 FROM credit_reservations r WHERE r.account_id=a.id AND r.session_id=c.session_id AND r.status='held' AND r.reserved_pt>r.settled_pt+r.released_pt)));
