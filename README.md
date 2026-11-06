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

The server listens on `127.0.0.1:8080`. To use a different address, set the `STAVEWELL_ADDR` environment variable. It currently provides two endpoints: `GET /health` and `GET /version`.

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

Stavewell has no sign-up. You create organizations and API keys with admin commands, run where the server's database is. They use the same `STAVEWELL_DB` setting as the server, and they work while the server is running. They don't create a database, so start the server once first. In this version, no API endpoint uses API keys.

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

## Maintainer

[@glsmith3](https://github.com/glsmith3)
