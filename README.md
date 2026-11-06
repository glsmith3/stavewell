# Stavewell

Stavewell is a developer platform for append-only records with tamper-evident hash chains.

> Stavewell is a non-commercial, experimental platform built for learning and testing. It's closed to the public and will not be offered as a service.

## Status

In development. The server currently provides health and version endpoints only. Not open for sign-ups.

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

The server listens on `127.0.0.1:8080`. To use a different address, set the `STAVEWELL_ADDR` environment variable.

Check that it's running:

```bash
curl http://127.0.0.1:8080/health
```

```json
{"status":"ok"}
```

The API is defined in [`api/openapi.yaml`](api/openapi.yaml). After you change it, regenerate the server code:

```bash
go generate ./...
```

## Maintainer

[@glsmith3](https://github.com/glsmith3)
