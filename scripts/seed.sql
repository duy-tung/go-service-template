-- Deterministic development seed. The static dev token maps to this
-- account; see ORDER_ENGINE_AUTH_ACCOUNT_ID. Safe to re-run.
INSERT INTO accounts (id, currency, balance_minor)
VALUES ('acct-demo', 'USD', 1000000)
ON CONFLICT (id) DO NOTHING;
