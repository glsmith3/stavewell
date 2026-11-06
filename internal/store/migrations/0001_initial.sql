-- Initial schema: organizations, API keys, ledgers, records and usage.
--
-- Conventions:
--   - Every table is STRICT, so a column only accepts values of its type.
--   - Internal row numbers (id columns) never leave the server. Public IDs
--     (org_..., key_...) are separate columns.
--   - Times are text in the format the API returns: UTC, three fractional
--     digits, for example 2026-11-06T19:22:31.123Z. Each time column checks
--     that its value is a real date and time in exactly that format.
--   - Hashes are 64 lowercase hexadecimal characters.
--
-- Protected rows. Triggers enforce these rules, as protection against a bug
-- in Stavewell's code. They don't stop someone who can edit the database
-- file directly; chain verification detects changes to records.
--   - Columns that identify a row, tie it to an organization, or are part of
--     a record hash never change.
--   - Organizations, API keys, scopes, ledgers and records are never
--     deleted.
--   - REPLACE (INSERT OR REPLACE, or REPLACE INTO) can't get around either
--     rule. It resolves a conflict by deleting the existing row, and SQLite
--     fires DELETE triggers for that only if the connection turns on
--     recursive_triggers. So each protected table also rejects an insert
--     that conflicts with an existing row, whatever the connection's
--     settings.
--   - A revoked API key stays revoked.

CREATE TABLE organizations (
    id                INTEGER PRIMARY KEY,
    public_id         TEXT    NOT NULL UNIQUE
                              CHECK (length(public_id) = 30 AND public_id GLOB 'org_*' AND substr(public_id, 5) NOT GLOB '*[^a-z2-7]*'),
    name              TEXT    NOT NULL
                              CHECK (length(name) BETWEEN 1 AND 64),
    created_at        TEXT    NOT NULL
                              CHECK (created_at IS strftime('%Y-%m-%dT%H:%M:%fZ', created_at)),
    -- The total canonical size of the data in this organization's records,
    -- in bytes, kept up to date in the same transaction as each record.
    stored_data_bytes INTEGER NOT NULL DEFAULT 0
                              CHECK (stored_data_bytes >= 0)
) STRICT;

CREATE TRIGGER organizations_fixed BEFORE UPDATE OF id, public_id, created_at ON organizations
BEGIN
    SELECT RAISE(ABORT, 'an organization''s ID and creation time never change');
END;

CREATE TRIGGER organizations_no_delete BEFORE DELETE ON organizations
BEGIN
    SELECT RAISE(ABORT, 'organizations are never deleted');
END;

CREATE TRIGGER organizations_no_replace BEFORE INSERT ON organizations
WHEN EXISTS (SELECT 1 FROM organizations WHERE id = NEW.id)
  OR EXISTS (SELECT 1 FROM organizations WHERE public_id = NEW.public_id)
BEGIN
    SELECT RAISE(ABORT, 'organizations are never replaced');
END;

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
                            CHECK (created_at IS strftime('%Y-%m-%dT%H:%M:%fZ', created_at)),
    expires_at      TEXT    NOT NULL
                            CHECK (expires_at IS strftime('%Y-%m-%dT%H:%M:%fZ', expires_at)),
    -- Empty while the key is active.
    revoked_at      TEXT
                            CHECK (revoked_at IS strftime('%Y-%m-%dT%H:%M:%fZ', revoked_at))
) STRICT;

CREATE INDEX api_keys_organization ON api_keys (organization_id);

-- Only revoked_at can change, and only from empty to a time.
CREATE TRIGGER api_keys_fixed BEFORE UPDATE OF id, public_id, organization_id, name, key_hash, created_at, expires_at ON api_keys
BEGIN
    SELECT RAISE(ABORT, 'only an API key''s revocation can change');
END;

CREATE TRIGGER api_keys_revocation_is_final BEFORE UPDATE OF revoked_at ON api_keys
WHEN OLD.revoked_at IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'a revoked API key stays revoked');
END;

CREATE TRIGGER api_keys_no_delete BEFORE DELETE ON api_keys
BEGIN
    SELECT RAISE(ABORT, 'API keys are never deleted');
END;

CREATE TRIGGER api_keys_no_replace BEFORE INSERT ON api_keys
WHEN EXISTS (SELECT 1 FROM api_keys WHERE id = NEW.id)
  OR EXISTS (SELECT 1 FROM api_keys WHERE public_id = NEW.public_id)
  OR EXISTS (SELECT 1 FROM api_keys WHERE key_hash = NEW.key_hash)
BEGIN
    SELECT RAISE(ABORT, 'API keys are never replaced');
END;

-- A key's scopes are set when the key is created. The schema can't tell a
-- scope added at creation from one added later, so the code that creates
-- keys is the only code that inserts scopes.
CREATE TABLE api_key_scopes (
    api_key_id INTEGER NOT NULL REFERENCES api_keys (id),
    scope      TEXT    NOT NULL
                       CHECK (scope IN ('ledger:read', 'ledger:write')),
    PRIMARY KEY (api_key_id, scope)
) STRICT;

CREATE TRIGGER api_key_scopes_no_update BEFORE UPDATE ON api_key_scopes
BEGIN
    SELECT RAISE(ABORT, 'an API key''s scopes never change');
END;

CREATE TRIGGER api_key_scopes_no_delete BEFORE DELETE ON api_key_scopes
BEGIN
    SELECT RAISE(ABORT, 'an API key''s scopes never change');
END;

CREATE TABLE ledgers (
    id              INTEGER PRIMARY KEY,
    organization_id INTEGER NOT NULL REFERENCES organizations (id),
    -- 1 to 64 lowercase letters, digits and hyphens, starting with a letter.
    -- Part of every record hash in the ledger, so it never changes.
    name            TEXT    NOT NULL
                            CHECK (length(name) BETWEEN 1 AND 64 AND name GLOB '[a-z]*' AND name NOT GLOB '*[^a-z0-9-]*'),
    created_at      TEXT    NOT NULL
                            CHECK (created_at IS strftime('%Y-%m-%dT%H:%M:%fZ', created_at)),
    UNIQUE (organization_id, name)
) STRICT;

CREATE TRIGGER ledgers_no_update BEFORE UPDATE ON ledgers
BEGIN
    SELECT RAISE(ABORT, 'ledgers never change');
END;

CREATE TRIGGER ledgers_no_delete BEFORE DELETE ON ledgers
BEGIN
    SELECT RAISE(ABORT, 'ledgers are never deleted');
END;

CREATE TRIGGER ledgers_no_replace BEFORE INSERT ON ledgers
WHEN EXISTS (SELECT 1 FROM ledgers WHERE id = NEW.id)
  OR EXISTS (SELECT 1 FROM ledgers WHERE organization_id = NEW.organization_id AND name = NEW.name)
BEGIN
    SELECT RAISE(ABORT, 'ledgers are never replaced');
END;

CREATE TABLE records (
    ledger_id       INTEGER NOT NULL REFERENCES ledgers (id),
    sequence        INTEGER NOT NULL
                            CHECK (sequence >= 1),
    created_at      TEXT    NOT NULL
                            CHECK (created_at IS strftime('%Y-%m-%dT%H:%M:%fZ', created_at)),
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

-- Records are append-only.
--
-- Because these insert triggers run before SQLite's own constraint checks,
-- a reused sequence number or idempotency key always fails with one of the
-- messages below, even with ON CONFLICT DO NOTHING or INSERT OR IGNORE. So
-- an append must look up its idempotency key itself, inside its write
-- transaction, before it inserts.
CREATE TRIGGER records_sequence_used BEFORE INSERT ON records
WHEN EXISTS (SELECT 1 FROM records WHERE ledger_id = NEW.ledger_id AND sequence = NEW.sequence)
BEGIN
    SELECT RAISE(ABORT, 'records are append-only: sequence number already used');
END;

CREATE TRIGGER records_idempotency_key_used BEFORE INSERT ON records
WHEN EXISTS (SELECT 1 FROM records WHERE ledger_id = NEW.ledger_id AND idempotency_key = NEW.idempotency_key)
BEGIN
    SELECT RAISE(ABORT, 'records are append-only: idempotency key already used in this ledger');
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
                             CHECK (day IS strftime('%Y-%m-%d', day)),
    records_appended INTEGER NOT NULL DEFAULT 0
                             CHECK (records_appended >= 0),
    PRIMARY KEY (organization_id, day)
) STRICT;
