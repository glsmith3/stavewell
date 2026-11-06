-- Initial schema: organizations, API keys, ledgers, records and usage.
--
-- Conventions:
--   - Every table is STRICT, so a column only accepts values of its type.
--   - Internal row numbers (id columns) never leave the server. Public IDs
--     (org_..., key_...) are separate columns.
--   - Times are text in the format the API returns: UTC, three fractional
--     digits, for example 2026-11-06T19:22:31.123Z.
--   - Hashes are 64 lowercase hexadecimal characters.

CREATE TABLE organizations (
    id                INTEGER PRIMARY KEY,
    public_id         TEXT    NOT NULL UNIQUE
                              CHECK (length(public_id) = 30 AND public_id GLOB 'org_*' AND substr(public_id, 5) NOT GLOB '*[^a-z2-7]*'),
    name              TEXT    NOT NULL
                              CHECK (length(name) BETWEEN 1 AND 64),
    created_at        TEXT    NOT NULL
                              CHECK (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9][0-9][0-9]Z'),
    -- The total canonical size of the data in this organization's records,
    -- in bytes, kept up to date in the same transaction as each record.
    stored_data_bytes INTEGER NOT NULL DEFAULT 0
                              CHECK (stored_data_bytes >= 0)
) STRICT;

CREATE TABLE api_keys (
    id              INTEGER PRIMARY KEY,
    public_id       TEXT    NOT NULL UNIQUE
                            CHECK (length(public_id) = 30 AND public_id GLOB 'key_*' AND substr(public_id, 5) NOT GLOB '*[^a-z2-7]*'),
    organization_id INTEGER NOT NULL REFERENCES organizations (id),
    name            TEXT    NOT NULL
                            CHECK (length(name) BETWEEN 1 AND 64),
    -- The SHA-256 hash of the full key. The key itself is never stored.
    key_hash        TEXT    NOT NULL UNIQUE
                            CHECK (length(key_hash) = 64 AND key_hash NOT GLOB '*[^0-9a-f]*'),
    created_at      TEXT    NOT NULL
                            CHECK (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9][0-9][0-9]Z'),
    expires_at      TEXT    NOT NULL
                            CHECK (expires_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9][0-9][0-9]Z'),
    -- Empty while the key is active.
    revoked_at      TEXT
                            CHECK (revoked_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9][0-9][0-9]Z')
) STRICT;

CREATE INDEX api_keys_organization ON api_keys (organization_id);

CREATE TABLE api_key_scopes (
    api_key_id INTEGER NOT NULL REFERENCES api_keys (id),
    scope      TEXT    NOT NULL
                       CHECK (scope IN ('ledger:read', 'ledger:write')),
    PRIMARY KEY (api_key_id, scope)
) STRICT;

CREATE TABLE ledgers (
    id              INTEGER PRIMARY KEY,
    organization_id INTEGER NOT NULL REFERENCES organizations (id),
    -- 1 to 64 lowercase letters, digits and hyphens, starting with a letter.
    name            TEXT    NOT NULL
                            CHECK (length(name) BETWEEN 1 AND 64 AND name GLOB '[a-z]*' AND name NOT GLOB '*[^a-z0-9-]*'),
    created_at      TEXT    NOT NULL
                            CHECK (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9][0-9][0-9]Z'),
    UNIQUE (organization_id, name)
) STRICT;

CREATE TABLE records (
    ledger_id       INTEGER NOT NULL REFERENCES ledgers (id),
    sequence        INTEGER NOT NULL
                            CHECK (sequence >= 1),
    created_at      TEXT    NOT NULL
                            CHECK (created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9][0-9][0-9]Z'),
    -- The record's data as canonical JSON. Stavewell always stores data;
    -- the column allows NULL so that a later schema version can remove a
    -- record's content while keeping its hashes.
    data            TEXT
                            CHECK (json_valid(data)),
    data_hash       TEXT    NOT NULL
                            CHECK (length(data_hash) = 64 AND data_hash NOT GLOB '*[^0-9a-f]*'),
    previous_hash   TEXT    NOT NULL
                            CHECK (length(previous_hash) = 64 AND previous_hash NOT GLOB '*[^0-9a-f]*'),
    hash            TEXT    NOT NULL
                            CHECK (length(hash) = 64 AND hash NOT GLOB '*[^0-9a-f]*'),
    -- Optional. Not part of the record or its hash.
    idempotency_key TEXT
                            CHECK (length(idempotency_key) BETWEEN 1 AND 128 AND idempotency_key NOT GLOB '*[^A-Za-z0-9_-]*'),
    PRIMARY KEY (ledger_id, sequence),
    UNIQUE (ledger_id, idempotency_key)
) STRICT;

-- Records are append-only. These triggers reject any change or removal, as
-- protection against a bug in Stavewell's code. They don't stop someone who
-- can edit the database file directly; chain verification detects that.

-- REPLACE (INSERT OR REPLACE, or REPLACE INTO) resolves a conflict by
-- deleting the existing row, and SQLite doesn't fire DELETE triggers for
-- that unless the connection turns on recursive_triggers. So this trigger
-- rejects any insert that conflicts with an existing record, whatever the
-- connection's settings.
CREATE TRIGGER records_no_replace BEFORE INSERT ON records
WHEN EXISTS (SELECT 1 FROM records WHERE ledger_id = NEW.ledger_id AND sequence = NEW.sequence)
  OR EXISTS (SELECT 1 FROM records WHERE ledger_id = NEW.ledger_id AND idempotency_key = NEW.idempotency_key)
BEGIN
    SELECT RAISE(ABORT, 'records are append-only');
END;

CREATE TRIGGER records_no_update BEFORE UPDATE ON records
BEGIN
    SELECT RAISE(ABORT, 'records are append-only');
END;

CREATE TRIGGER records_no_delete BEFORE DELETE ON records
BEGIN
    SELECT RAISE(ABORT, 'records are append-only');
END;

CREATE TABLE daily_usage (
    organization_id  INTEGER NOT NULL REFERENCES organizations (id),
    -- A UTC date, for example 2026-11-06.
    day              TEXT    NOT NULL
                             CHECK (day GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'),
    records_appended INTEGER NOT NULL DEFAULT 0
                             CHECK (records_appended >= 0),
    PRIMARY KEY (organization_id, day)
) STRICT;
