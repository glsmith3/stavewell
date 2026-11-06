# Stavewell

Stavewell is a developer platform for append-only records with tamper-evident hash chains.

> Stavewell is a non-commercial, experimental platform built for learning and testing. It's closed to the public and will not be offered as a service.

## Status

In development. Not open for sign-ups.

## Documentation

Not yet published.

## Development

Requires Go 1.27.3 or later.

Run the tests:

```bash
go test ./...
```

Run the server:

```bash
go run ./cmd/stavewell
```

The server listens on `127.0.0.1:8080`. To use a different address, set the `STAVEWELL_ADDR` environment variable. It provides `GET /health` and `GET /version`, which need no API key, and the endpoints described under [Ledgers](#ledgers), [Records](#records) and [Chain verification](#chain-verification).

The server keeps its data in an SQLite database file, `stavewell.db` in the current directory. To use a different file, set the `STAVEWELL_DB` environment variable. If the file doesn't exist, the server creates it, readable and writable only by your user account. SQLite also keeps two working files beside it, ending in `-wal` and `-shm`. When the server starts, it updates the database's structure to the version it needs, and it won't start if the database was created by a newer version of Stavewell.

Check that it's running:

```bash
curl http://127.0.0.1:8080/health
```

```json
{"status":"ok"}
```

Error responses use the Problem Details format ([RFC 9457](https://www.rfc-editor.org/rfc/rfc9457)), with the content type `application/problem+json`. For example, a path that doesn't exist:

```bash
curl http://127.0.0.1:8080/no-such-path
```

```json
{"type":"https://docs.stavewell.io/errors/not-found","title":"Not found","status":404,"detail":"No endpoint matches this path.","request_id":"req_wd2qfzaazklt2xpco7spkcxy5c"}
```

Use `type` to tell errors apart. Each request gets a different `request_id`, and the server log records the same ID for that request.

The API is defined in [`api/openapi.yaml`](api/openapi.yaml). After you change it, regenerate the server code:

```bash
go generate ./...
```

## Organizations and API keys

Stavewell has no sign-up. You create organizations and API keys with admin commands, run where the server's database is. They use the same `STAVEWELL_DB` setting as the server, and they work while the server is running. They don't create a database, so start the server once first.

Create an organization:

```bash
go run ./cmd/stavewell admin org create --name "Test organization"
```

```
Organization created.

ID:       org_ngztvgyskawogjffukqizkghrh
Name:     Test organization
Created:  2026-11-06T20:42:24.040Z
```

Create an API key for the organization, with one or both of the scopes `ledger:read` and `ledger:write`:

```bash
go run ./cmd/stavewell admin key create --org org_ngztvgyskawogjffukqizkghrh --name "Local testing" --scopes ledger:read,ledger:write
```

```
API key created. This is the only time the key is shown, so copy it now.

Key:           sw_exampleexampleexampleexampleexampleexampleexamplekey
Key ID:        key_abprqbbshjploerfcxoy6stzbw
Organization:  org_ngztvgyskawogjffukqizkghrh
Name:          Local testing
Scopes:        ledger:read, ledger:write
Expires:       2027-02-04T20:42:24.052Z
```

Your IDs, times and key will differ. The key shown here is a placeholder: a real key is random. Stavewell stores only a hash of the key, so it can't show you the key again. A key expires after 90 days, or after the number of days you set with `--expires-in-days` (1 to 365).

Other admin commands:

- `admin org list` lists organizations.
- `admin key list --org ORG_ID` lists an organization's API keys, with their scopes, expiry and status. It never shows the keys themselves.
- `admin key revoke --key KEY_ID` revokes an API key. A revoked key can't be used again.

To see every command, run:

```bash
go run ./cmd/stavewell help
```

## Ledgers

A ledger is a named, append-only sequence of records in your organization. In this version, you can create, list and get ledgers. Every endpoint under `/v1/` needs an API key, sent as `Authorization: Bearer KEY`. Run these commands with your own key in place of the placeholder.

Create a ledger. It needs the `ledger:write` scope:

```bash
curl -X POST http://127.0.0.1:8080/v1/ledgers \
  -H "Authorization: Bearer sw_exampleexampleexampleexampleexampleexampleexamplekey" \
  -H "Content-Type: application/json" \
  -d '{"name":"orders"}'
```

```json
{"name":"orders","created_at":"2026-11-06T23:48:05.696Z","latest_sequence":null,"latest_hash":null}
```

A ledger name is 1 to 64 lowercase letters, digits and hyphens, starting with a letter. It's unique within your organization and never changes. An organization can have up to 100 ledgers.

List your ledgers, or get one by name. Both need the `ledger:read` scope:

```bash
curl http://127.0.0.1:8080/v1/ledgers \
  -H "Authorization: Bearer sw_exampleexampleexampleexampleexampleexampleexamplekey"
```

```json
{"ledgers":[{"name":"orders","created_at":"2026-11-06T23:48:05.696Z","latest_sequence":null,"latest_hash":null}],"next_cursor":"eyJ2IjoxLCJsIjoibGVkZ2VycyIsIm8iOiJvcmdfMmZnaGQ3ZWRxeWFmdmx6aG10czJ3ZjduYWoiLCJhIjoib3JkZXJzIn0","has_more":false}
```

```bash
curl http://127.0.0.1:8080/v1/ledgers/orders \
  -H "Authorization: Bearer sw_exampleexampleexampleexampleexampleexampleexamplekey"
```

```json
{"name":"orders","created_at":"2026-11-06T23:48:05.696Z","latest_sequence":null,"latest_hash":null}
```

`latest_sequence` and `latest_hash` are the sequence number and record hash of the ledger's latest record. Both are `null` until you append a record.

The list returns up to 100 ledgers, or fewer if you set `limit`. To get the next page, pass `next_cursor` as the `cursor` parameter. A request without a valid key gets a `401` error:

```json
{"type":"https://docs.stavewell.io/errors/unauthorized","title":"Unauthorized","status":401,"detail":"This request needs a valid API key in the Authorization header.","request_id":"req_kmji2rxied56gmzsvm23atkrmz"}
```

## Records

A record is one item appended to a ledger. A record never changes after it's written, and it can't be deleted.

Append a record. It needs the `ledger:write` scope. The request body has one field, `data`, which is a JSON object:

```bash
curl -X POST http://127.0.0.1:8080/v1/ledgers/orders/records \
  -H "Authorization: Bearer sw_exampleexampleexampleexampleexampleexampleexamplekey" \
  -H "Content-Type: application/json" \
  -d '{"data":{"order_id":"A-1001","amount":"12.50","quantity":2}}'
```

```json
{"ledger":"orders","sequence":1,"created_at":"2026-11-06T23:48:05.728Z","data":{"amount":"12.50","order_id":"A-1001","quantity":2},"data_hash":"c8b92db4384c15e38afd338748232773fc295b5192f6736e24896742ba31d01c","previous_hash":"0000000000000000000000000000000000000000000000000000000000000000","hash":"5badd44075da23b07a58c699f2527ece72dbb280863b5f42e489915ea8bedd8f"}
```

Stavewell sets the other fields:

- `ledger`: the ledger's name.
- `sequence`: records in a ledger are numbered from 1, in the order they're appended, with no gaps.
- `created_at`: when the record was appended, in UTC. It's never earlier than the previous record's `created_at`.
- `data`: your data as canonical JSON ([RFC 8785](https://www.rfc-editor.org/rfc/rfc8785)): keys sorted and no whitespace. Your key order and spacing aren't kept.
- `data_hash`: the SHA-256 hash of `data`.
- `previous_hash`: the record hash of the previous record, or 64 zeros for record 1.
- `hash`: the record hash. It covers `created_at`, `data_hash`, `ledger`, `previous_hash` and `sequence`. Because each record hash covers the previous one, the record hashes form the ledger's chain.

The "Record hashes" section of `api/openapi.yaml` gives the exact rules, with a worked example, so you can compute the hashes yourself.

Numbers in `data` must be whole numbers from -9007199254740991 to 9007199254740991, with no decimal point or exponent. Send other numbers, such as amounts, as strings. A decimal number gets an error:

```bash
curl -X POST http://127.0.0.1:8080/v1/ledgers/orders/records \
  -H "Authorization: Bearer sw_exampleexampleexampleexampleexampleexampleexamplekey" \
  -H "Content-Type: application/json" \
  -d '{"data":{"order_id":"A-1003","amount":12.50,"quantity":1}}'
```

```json
{"type":"https://docs.stavewell.io/errors/invalid-record-data","title":"Invalid record data","status":400,"detail":"Numbers in data must be whole numbers from -9007199254740991 to 9007199254740991, written without a decimal point or exponent. Send other numbers as strings.","request_id":"req_3rcpyqp23hvtef3tr3ois46oc3"}
```

`data` can be up to 16 KiB in canonical form, nested up to 32 levels deep. The "Record data" section of `api/openapi.yaml` lists every rule.

After a second record is appended, list the ledger's records, or get one by its sequence number. Both need the `ledger:read` scope:

```bash
curl http://127.0.0.1:8080/v1/ledgers/orders/records \
  -H "Authorization: Bearer sw_exampleexampleexampleexampleexampleexampleexamplekey"
```

```json
{"records":[{"ledger":"orders","sequence":1,"created_at":"2026-11-06T23:48:05.728Z","data":{"amount":"12.50","order_id":"A-1001","quantity":2},"data_hash":"c8b92db4384c15e38afd338748232773fc295b5192f6736e24896742ba31d01c","previous_hash":"0000000000000000000000000000000000000000000000000000000000000000","hash":"5badd44075da23b07a58c699f2527ece72dbb280863b5f42e489915ea8bedd8f"},{"ledger":"orders","sequence":2,"created_at":"2026-11-06T23:48:05.735Z","data":{"amount":"7.25","order_id":"A-1002","quantity":1},"data_hash":"86658e9cc2039fe716129d5f2f34d78d29d0efbaa929f07c8e7569e08c4f9fe5","previous_hash":"5badd44075da23b07a58c699f2527ece72dbb280863b5f42e489915ea8bedd8f","hash":"c6f4a2e769a836cbd79355768e35668c256cbd244513722f60391a7437594254"}],"next_cursor":"eyJ2IjoxLCJsIjoicmVjb3Jkcy9vcmRlcnMiLCJvIjoib3JnXzJmZ2hkN2VkcXlhZnZsemhtdHMyd2Y3bmFqIiwiYSI6IjIifQ","has_more":false}
```

```bash
curl http://127.0.0.1:8080/v1/ledgers/orders/records/1 \
  -H "Authorization: Bearer sw_exampleexampleexampleexampleexampleexampleexamplekey"
```

```json
{"ledger":"orders","sequence":1,"created_at":"2026-11-06T23:48:05.728Z","data":{"amount":"12.50","order_id":"A-1001","quantity":2},"data_hash":"c8b92db4384c15e38afd338748232773fc295b5192f6736e24896742ba31d01c","previous_hash":"0000000000000000000000000000000000000000000000000000000000000000","hash":"5badd44075da23b07a58c699f2527ece72dbb280863b5f42e489915ea8bedd8f"}
```

The list returns records in sequence order, up to 100 at a time, or fewer if you set `limit`. Pages work as they do for ledgers, and `next_cursor` is present even on the last page. To follow a ledger, keep the last page's `next_cursor` and use it later: you get only the records appended since.

The ledger now reports its latest record:

```bash
curl http://127.0.0.1:8080/v1/ledgers/orders \
  -H "Authorization: Bearer sw_exampleexampleexampleexampleexampleexampleexamplekey"
```

```json
{"name":"orders","created_at":"2026-11-06T23:48:05.696Z","latest_sequence":2,"latest_hash":"c6f4a2e769a836cbd79355768e35668c256cbd244513722f60391a7437594254"}
```

### Retrying an append safely

If an append times out, you can't tell whether the record was appended. To retry without creating a duplicate, send an `Idempotency-Key` header, and send the same key with every retry of that append:

```bash
curl -X POST http://127.0.0.1:8080/v1/ledgers/orders/records \
  -H "Authorization: Bearer sw_exampleexampleexampleexampleexampleexampleexamplekey" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: order-A-1003" \
  -d '{"data":{"order_id":"A-1003","amount":"30.00","quantity":3}}'
```

The first time, the record is appended, with status `201 Created`:

```json
{"ledger":"orders","sequence":3,"created_at":"2026-11-06T23:48:05.767Z","data":{"amount":"30.00","order_id":"A-1003","quantity":3},"data_hash":"c537947d489bf2a1e01fa8de5ba517366164e4a713af6c25ec3786c87bdfad7e","previous_hash":"c6f4a2e769a836cbd79355768e35668c256cbd244513722f60391a7437594254","hash":"bcbec99a6a37315e1d89217ceaf1809c12c514066ed0c750e47d32d84e00d053"}
```

Sending the same request again appends nothing. The response is the same record, with status `200 OK`:

```json
{"ledger":"orders","sequence":3,"created_at":"2026-11-06T23:48:05.767Z","data":{"amount":"30.00","order_id":"A-1003","quantity":3},"data_hash":"c537947d489bf2a1e01fa8de5ba517366164e4a713af6c25ec3786c87bdfad7e","previous_hash":"c6f4a2e769a836cbd79355768e35668c256cbd244513722f60391a7437594254","hash":"bcbec99a6a37315e1d89217ceaf1809c12c514066ed0c750e47d32d84e00d053"}
```

Sending the same key with different `data` appends nothing and gets an error, with status `422 Unprocessable Entity`:

```json
{"type":"https://docs.stavewell.io/errors/idempotency-key-reused","title":"Idempotency key reused","status":422,"detail":"This idempotency key was already used in this ledger with different data, so nothing was appended.","request_id":"req_ldi5axhwh5i2cy7rgibveqwcq2"}
```

A key is 1 to 128 letters, digits, hyphens and underscores, and it's case-sensitive. An ID from your own system, such as an order number, makes a good key. Stavewell stores a key with its record, and records are never deleted, so the key keeps working. Each key belongs to one ledger, and reading records doesn't return it. An append without the header always appends a new record.

## Chain verification

Chain verification recomputes a ledger's chain to confirm that no record was altered. It needs the `ledger:read` scope:

```bash
curl http://127.0.0.1:8080/v1/ledgers/orders/chain-verification \
  -H "Authorization: Bearer sw_exampleexampleexampleexampleexampleexampleexamplekey"
```

```json
{"ledger":"orders","intact":true,"first_sequence":1,"last_sequence":3,"last_hash":"bcbec99a6a37315e1d89217ceaf1809c12c514066ed0c750e47d32d84e00d053","reached_latest":true,"broken_sequence":null}
```

For each record, it checks that `data_hash` matches `data`, that `hash` matches the record's other fields, and that `previous_hash` matches the record before it. A request checks up to 10,000 records. If `reached_latest` is `false`, check the rest with `?from=` set to `last_sequence` plus 1.

A broken chain isn't an error. The response is still `200`, with `intact` set to `false`, `broken_sequence` set to the first record that failed, and `last_hash` set to `null`. Checking stops at that record.

What chain verification proves:

- **It catches** a record changed in the database without every later record hash being recomputed: for example, by disk corruption, a faulty migration or someone editing the database file.
- **It can't catch** someone with full control of the server who changes a record and then recomputes every record hash after it.
- **To protect against that,** save `last_sequence` and `last_hash` somewhere outside Stavewell. Later, confirm that the record with that sequence number still has that record hash, and that chain verification still reports the chain as intact.

### Running chain verification yourself

To check a chain without trusting the server's answer, run one of the scripts in [`examples/chain-verification/`](examples/chain-verification/). They read the records through the API and run the same three checks themselves. Each uses only its language's standard library. They're tested with Python 3.13 and Node.js 22.

```bash
STAVEWELL_API_KEY=sw_exampleexampleexampleexampleexampleexampleexamplekey python3 examples/chain-verification/verify_chain.py orders
```

```json
{"ledger":"orders","intact":true,"first_sequence":1,"last_sequence":3,"last_hash":"bcbec99a6a37315e1d89217ceaf1809c12c514066ed0c750e47d32d84e00d053","reached_latest":true,"broken_sequence":null}
```

```bash
STAVEWELL_API_KEY=sw_exampleexampleexampleexampleexampleexampleexamplekey node examples/chain-verification/verify_chain.js orders
```

```json
{"ledger":"orders","intact":true,"first_sequence":1,"last_sequence":3,"last_hash":"bcbec99a6a37315e1d89217ceaf1809c12c514066ed0c750e47d32d84e00d053","reached_latest":true,"broken_sequence":null}
```

Both print the same fields as the server's response. They exit with status `0` if the chain is intact, or `1` if it's broken. To use a server at another address, add `--url`, for example `--url http://127.0.0.1:9090`.

`api/openapi.yaml` describes every endpoint, parameter and error.

## Maintainer

[@glsmith3](https://github.com/glsmith3)
