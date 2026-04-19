# OpenAPI integration

`rx serve` generates a full OpenAPI 3.1 specification at runtime and
publishes it at three URLs:

- `/openapi.json` — JSON spec
- `/openapi.yaml` — YAML spec (same content)
- `/docs` — Swagger UI rendered from the spec
- `/redoc` — ReDoc alternative rendering

## Consuming the spec

The spec is stable across releases within a major version. Consume it
programmatically for:

- Client generation (`openapi-generator`, `oapi-codegen`, `prism`, etc.)
- Contract testing (match request/response against the schema)
- API gateway import (Kong, Apigee, AWS API Gateway, etc.)

### Fetching

```bash
curl -sfo rx-openapi.json http://127.0.0.1:7777/openapi.json

# Or YAML
curl -sfo rx-openapi.yaml http://127.0.0.1:7777/openapi.yaml
```

### Swagger UI

Open <http://127.0.0.1:7777/docs> in a browser. You'll see a
collapsible list of every endpoint with:

- Expected parameters and types
- Request/response schemas
- Try-it-out buttons that fire real requests against your running
  server

### ReDoc

<http://127.0.0.1:7777/redoc> — cleaner read-only view, better for
sharing with someone who just wants to read the API surface.

## Spec content

The `/openapi.json` output includes:

- `openapi: "3.1.0"` — the OpenAPI version
- `info` — title, version, description, license, contact
- `tags` — endpoint groupings (`General`, `Search`, `Context`,
  `Indexing`, `Operations`, `FileTree`, `Analysis`, `Monitoring`)
- `paths` — every `/health`, `/v1/*` route
- `components.schemas` — JSON schema for every request and response
  type

### Tags

Endpoints are grouped by `tag` in the spec and UI:

| Tag | Endpoints |
|---|---|
| General | `/health` |
| Search | `/v1/trace` |
| Context | `/v1/samples` |
| Indexing | `GET /v1/index`, `POST /v1/index` |
| Operations | `POST /v1/compress`, `GET /v1/tasks/*` |
| FileTree | `/v1/tree` |
| Analysis | `/v1/detectors` |
| Monitoring | `/metrics` (though `/metrics` is documented outside the OpenAPI spec because it's not JSON) |

## Code generation

### Go client (oapi-codegen)

```bash
go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@latest

oapi-codegen -generate client,types \
    -o rxclient.go \
    -package rxclient \
    rx-openapi.json
```

The generated `rxclient` package will have typed methods for every
endpoint, matching the Go type conventions used internally by `rx`
(the spec is generated from the Go structs in `pkg/rxtypes/`).

### Python client (openapi-python-client)

```bash
pipx install openapi-python-client
openapi-python-client generate --url http://127.0.0.1:7777/openapi.json
```

### TypeScript client (openapi-typescript)

```bash
npx openapi-typescript http://127.0.0.1:7777/openapi.json --output rx-api.d.ts
```

## Contract testing

### Prism (mock server / validator)

```bash
# Run rx in one terminal.
rx serve --port=7777 &

# Run prism in proxy mode to validate real requests against the spec.
npx @stoplight/prism-cli proxy http://127.0.0.1:7777/openapi.json http://127.0.0.1:7777

# Drive test traffic through http://127.0.0.1:4010
```

### Dredd (HTTP smoke tests)

```bash
dredd http://127.0.0.1:7777/openapi.json http://127.0.0.1:7777
```

## Error envelope in the spec

All error responses in the spec reference a shared `Error` schema:

```json
{ "type": "object", "properties": { "detail": { "type": "string" } } }
```

Generated clients will expose `.Detail` (or equivalent) as the single
error field. The extended path-sandbox error is an additional schema
used for `403 path_outside_search_root`. See [conventions](conventions.md).

## Server information

The spec's `info` block includes:

- **Title**: `rx-tool API`
- **Version**: reflects the binary version
- **License**: MIT
- **Contact**: links to the `rx-tool` GitHub project

The `servers` array is empty in the spec — clients should supply the
server URL themselves (useful when running behind a reverse proxy that
adds a path prefix).

## Known limitations

- The spec does not include `/metrics` (it's a Prometheus text
  endpoint, not JSON)
- The spec does not include the SPA catch-all (`/`, `/assets/*`,
  `/favicon.*`) — these aren't part of the API surface
- Webhook callbacks are documented in the [webhooks](webhooks.md) page
  rather than as `callbacks` in the OpenAPI spec

## See also

- [API overview](index.md)
- [Conventions](conventions.md)
- [Webhooks](webhooks.md)
- [`rx serve`](../cli/serve.md)
