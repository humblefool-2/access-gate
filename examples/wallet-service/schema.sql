-- Applied on every startup, so use CREATE TABLE IF NOT EXISTS.
-- While prototyping, change a table by editing it here and deleting data/poc.db.

-- Money is stored as integer minor units (e.g. cents).
CREATE TABLE IF NOT EXISTS wallets (
    wallet_id     TEXT PRIMARY KEY,
    balance_minor INTEGER NOT NULL DEFAULT 0 CHECK (balance_minor >= 0),
    currency      TEXT NOT NULL DEFAULT 'EUR',
    status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'frozen', 'closed')),
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS customers (
    customer_id TEXT PRIMARY KEY,
    email       TEXT NOT NULL UNIQUE,
    phone       TEXT,
    wallet_id   TEXT NOT NULL UNIQUE REFERENCES wallets (wallet_id),
    created_at  TEXT NOT NULL
);

-- Blocked funds: blocked = SUM(active holds), available = balance - blocked.
CREATE TABLE IF NOT EXISTS wallet_holds (
    hold_id      TEXT PRIMARY KEY,
    wallet_id    TEXT NOT NULL REFERENCES wallets (wallet_id),
    amount_minor INTEGER NOT NULL CHECK (amount_minor > 0),
    reason       TEXT,
    status       TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'released')),
    created_at   TEXT NOT NULL,
    released_at  TEXT
);
CREATE INDEX IF NOT EXISTS idx_wallet_holds_wallet ON wallet_holds (wallet_id, status);
