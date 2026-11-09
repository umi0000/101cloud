-- Reporting intersects [from_at,to_at); a lower to_at bound avoids scanning
-- every historical charge for a current calendar year. Existing settlement
-- indexes are retained for their financial processing access paths.
ALTER TABLE usage_charges
  ADD KEY ix_usage_charges_report (account_id,status,to_at,from_at);
