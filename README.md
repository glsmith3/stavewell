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

## Maintainer

[@glsmith3](https://github.com/glsmith3)
