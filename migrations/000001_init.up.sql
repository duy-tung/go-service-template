-- No BEGIN/COMMIT here: the migration runner (internal/migrate) and psql -1
-- both wrap each file in a single transaction, so the DDL and its version
-- record commit atomically.

CREATE TABLE accounts (
    id            text        NOT NULL,
    currency      text        NOT NULL,
    balance_minor bigint      NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT pk_accounts PRIMARY KEY (id),
    CONSTRAINT ck_accounts_balance_non_negative CHECK (balance_minor >= 0),
    CONSTRAINT ck_accounts_currency_iso CHECK (currency ~ '^[A-Z]{3}$')
);

CREATE TABLE orders (
    id              text        NOT NULL,
    account_id      text        NOT NULL,
    idempotency_key text        NOT NULL,
    amount_minor    bigint      NOT NULL,
    currency        text        NOT NULL,
    created_at      timestamptz NOT NULL,

    CONSTRAINT pk_orders PRIMARY KEY (id),
    CONSTRAINT fk_orders_account FOREIGN KEY (account_id) REFERENCES accounts (id),
    CONSTRAINT ck_orders_amount_positive CHECK (amount_minor > 0),
    CONSTRAINT ck_orders_currency_iso CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT ck_orders_idempotency_key_shape
        CHECK (idempotency_key ~ '^[A-Za-z0-9_-]{1,64}$'),
    -- Named explicitly: the application matches this constraint name when
    -- translating unique violations into idempotency conflicts.
    CONSTRAINT uq_orders_account_idempotency_key
        UNIQUE (account_id, idempotency_key)
);
