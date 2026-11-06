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

The server listens on `127.0.0.1:8080`. To use a different address, set the `STAVEWELL_ADDR` environment variable. It provides `GET /health` and `GET /version`, which need no API key, and the ledger and record endpoints described under [Ledgers](#ledgers) and [Records](#records).

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
{"name":"orders","created_at":"2026-11-06T23:18:37.767Z","latest_sequence":null,"latest_hash":null}
```

A ledger name is 1 to 64 lowercase letters, digits and hyphens, starting with a letter. It's unique within your organization and never changes. An organization can have up to 100 ledgers.

List your ledgers, or get one by name. Both need the `ledger:read` scope:

```bash
curl http://127.0.0.1:8080/v1/ledgers \
  -H "Authorization: Bearer sw_exampleexampleexampleexampleexampleexampleexamplekey"
```

```json
{"ledgers":[{"name":"orders","created_at":"2026-11-06T23:18:37.767Z","latest_sequence":null,"latest_hash":null}],"next_cursor":"eyJ2IjoxLCJsIjoibGVkZ2VycyIsIm8iOiJvcmdfd2c1cm1vN3B1bnozbmd6andiZTRqbXZjcXciLCJhIjoib3JkZXJzIn0","has_more":false}
```

```bash
curl http://127.0.0.1:8080/v1/ledgers/orders \
  -H "Authorization: Bearer sw_exampleexampleexampleexampleexampleexampleexamplekey"
```

```json
{"name":"orders","created_at":"2026-11-06T23:18:37.767Z","latest_sequence":null,"latest_hash":null}
```

`latest_sequence` and `latest_hash` are the sequence number and record hash of the ledger's latest record. Both are `null` until you append a record.

The list returns up to 100 ledgers, or fewer if you set `limit`. To get the next page, pass `next_cursor` as the `cursor` parameter. A request without a valid key gets a `401` error:

```json
{"type":"https://docs.stavewell.io/errors/unauthorized","title":"Unauthorized","status":401,"detail":"This request needs a valid API key in the Authorization header.","request_id":"req_xpyvomsfc4pwj4csbtdrl72qu2"}
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
{"ledger":"orders","sequence":1,"created_at":"2026-11-06T23:18:37.786Z","data":{"amount":"12.50","order_id":"A-1001","quantity":2},"data_hash":"c8b92db4384c15e38afd338748232773fc295b5192f6736e24896742ba31d01c","previous_hash":"0000000000000000000000000000000000000000000000000000000000000000","hash":"cef54f7aac35a2d11ff884364b18d7d3b58173de26eba6d1b2ad544712138ad7"}
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
{"type":"https://docs.stavewell.io/errors/invalid-record-data","title":"Invalid record data","status":400,"detail":"Numbers in data must be whole numbers from -9007199254740991 to 9007199254740991, written without a decimal point or exponent. Send other numbers as strings.","request_id":"req_5pbce2ote36sdoi2wntg3zos4c"}
```

`data` can be up to 16 KiB in canonical form, nested up to 32 levels deep. The "Record data" section of `api/openapi.yaml` lists every rule.

After a second record is appended, list the ledger's records, or get one by its sequence number. Both need the `ledger:read` scope:

```bash
curl http://127.0.0.1:8080/v1/ledgers/orders/records \
  -H "Authorization: Bearer sw_exampleexampleexampleexampleexampleexampleexamplekey"
```

```json
{"records":[{"ledger":"orders","sequence":1,"created_at":"2026-11-06T23:18:37.786Z","data":{"amount":"12.50","order_id":"A-1001","quantity":2},"data_hash":"c8b92db4384c15e38afd338748232773fc295b5192f6736e24896742ba31d01c","previous_hash":"0000000000000000000000000000000000000000000000000000000000000000","hash":"cef54f7aac35a2d11ff884364b18d7d3b58173de26eba6d1b2ad544712138ad7"},{"ledger":"orders","sequence":2,"created_at":"2026-11-06T23:18:37.794Z","data":{"amount":"7.25","order_id":"A-1002","quantity":1},"data_hash":"86658e9cc2039fe716129d5f2f34d78d29d0efbaa929f07c8e7569e08c4f9fe5","previous_hash":"cef54f7aac35a2d11ff884364b18d7d3b58173de26eba6d1b2ad544712138ad7","hash":"0990eb658ef02189095bf37575c55fe48798f3de399bc61a7447ad719f25d848"}],"next_cursor":"eyJ2IjoxLCJsIjoicmVjb3Jkcy9vcmRlcnMiLCJvIjoib3JnX3dnNXJtbzdwdW56M25nemp3YmU0am12Y3F3IiwiYSI6IjIifQ","has_more":false}
```

```bash
curl http://127.0.0.1:8080/v1/ledgers/orders/records/1 \
  -H "Authorization: Bearer sw_exampleexampleexampleexampleexampleexampleexamplekey"
```

```json
{"ledger":"orders","sequence":1,"created_at":"2026-11-06T23:18:37.786Z","data":{"amount":"12.50","order_id":"A-1001","quantity":2},"data_hash":"c8b92db4384c15e38afd338748232773fc295b5192f6736e24896742ba31d01c","previous_hash":"0000000000000000000000000000000000000000000000000000000000000000","hash":"cef54f7aac35a2d11ff884364b18d7d3b58173de26eba6d1b2ad544712138ad7"}
```

The list returns records in sequence order, up to 100 at a time, or fewer if you set `limit`. Pages work as they do for ledgers, and `next_cursor` is present even on the last page. To follow a ledger, keep the last page's `next_cursor` and use it later: you get only the records appended since.

The ledger now reports its latest record:

```bash
curl http://127.0.0.1:8080/v1/ledgers/orders \
  -H "Authorization: Bearer sw_exampleexampleexampleexampleexampleexampleexamplekey"
```

```json
{"name":"orders","created_at":"2026-11-06T23:18:37.767Z","latest_sequence":2,"latest_hash":"0990eb658ef02189095bf37575c55fe48798f3de399bc61a7447ad719f25d848"}
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
{"ledger":"orders","sequence":3,"created_at":"2026-11-06T23:18:37.825Z","data":{"amount":"30.00","order_id":"A-1003","quantity":3},"data_hash":"c537947d489bf2a1e01fa8de5ba517366164e4a713af6c25ec3786c87bdfad7e","previous_hash":"0990eb658ef02189095bf37575c55fe48798f3de399bc61a7447ad719f25d848","hash":"6ba7ad99b7f5b6ac5d41c64c0983a5fedc431cb3433269729dae03f3093cc13c"}
```

Sending the same request again appends nothing. The response is the same record, with status `200 OK`:

```json
{"ledger":"orders","sequence":3,"created_at":"2026-11-06T23:18:37.825Z","data":{"amount":"30.00","order_id":"A-1003","quantity":3},"data_hash":"c537947d489bf2a1e01fa8de5ba517366164e4a713af6c25ec3786c87bdfad7e","previous_hash":"0990eb658ef02189095bf37575c55fe48798f3de399bc61a7447ad719f25d848","hash":"6ba7ad99b7f5b6ac5d41c64c0983a5fedc431cb3433269729dae03f3093cc13c"}
```

Sending the same key with different `data` appends nothing and gets an error, with status `422 Unprocessable Entity`:

```json
{"type":"https://docs.stavewell.io/errors/idempotency-key-reused","title":"Idempotency key reused","status":422,"detail":"This idempotency key was already used in this ledger with different data, so nothing was appended.","request_id":"req_togdvey32pnppr2e4baxypek4p"}
```

A key is 1 to 128 letters, digits, hyphens and underscores, and it's case-sensitive. An ID from your own system, such as an order number, makes a good key. Stavewell remembers a key for as long as its record exists. Each key belongs to one ledger, and reading records doesn't return it. An append without the header always appends a new record.

`api/openapi.yaml` describes every endpoint, parameter and error.

## Maintainer

[@glsmith3](https://github.com/glsmith3)
