# rulego-elasticsearch

A [RuleGo](https://github.com/rulego/rulego) Go plugin providing one node,
`elasticsearchWrite`, that writes a bounded batch of documents to
Elasticsearch over HTTP `_bulk` using only the Go standard library. No
third-party ES client dependency.

Target: the `rulego-server-docker` plugin runtime (RuleGo `v1.0.0`, ABI
`abi-3e0c9d46dddaf6235cfe609d543222f07f8e1abf3374f0669bf703df85063885`),
linux/amd64 + linux/arm64. The `_bulk` wire contract (NDJSON + per-item
status codes) is stable across ES 7–9; tested against the 9.x line (9.5.5).

## Node: `elasticsearchWrite`

Sends one bounded batch of `create`/`index` operations to one named
Elasticsearch client and reports the per-item outcome. Nothing about any
specific producer is encoded in the node: index name, document id, event
parsing, and which op to use are supplied per message by the rule graph.

### Configuration

```json
{
  "server": "ref://es",
  "timeoutMs": 30000,
  "maxRequestBytes": 31457280
}
```

- `server` (required) — `ref://<id>` to borrow a shared client from
  `node_pool.json`, or an inline owner: a base URL plus `auth` and optional
  `tls`.
- `auth` — `{"type": "apiKey", "apiKey": "..."}` sends
  `Authorization: ApiKey <key>`; `{"type": "basic", "username": "...",
  "password": "..."}` sends `Authorization: Basic <b64>`. Secrets come from
  the deployment's secret mechanism (environment expansion in the node_pool
  config at deploy time); never hardcode them.
- `tls` — `{"caFile": "/app/data/certs/es-ca.pem"}` adds an extra CA. TLS
  verification is always on; there is deliberately no `insecureSkipVerify`
  knob.
- `timeoutMs` — per-request deadline, default 30000.
- `maxRequestBytes` — hard cap on the serialized NDJSON body (default
  30 MiB). Exceeding it fails fast as terminal; the node never splits
  batches, the graph decides how to size them.

`node_pool.json` owner:

```json
{
  "id": "es",
  "type": "elasticsearchWrite",
  "configuration": {
    "server": "https://es.internal:9200",
    "auth": { "type": "apiKey", "apiKey": "${ES_API_KEY}" },
    "timeoutMs": 30000
  }
}
```

Borrower nodes use `"server": "ref://es"` and hold no credentials. Client
construction is lazy and soft-fails: an ES outage at RuleGo boot leaves the
node in Reconnecting rather than killing the process; failures surface per
message.

### Message contract

Input (per invocation), JSON:

```json
{
  "index": "budva-events-v1",
  "operations": [
    { "op": "create", "id": "9f2c0a1e", "doc": { "event": "new", "chat_id": 1 } },
    { "op": "index", "id": "1:-100", "doc": { "tombstone": true } }
  ]
}
```

- `index` — required, non-empty, must match `^[a-z0-9._-]+$` (ES index name
  rules); checked before any network I/O.
- `operations` — non-empty, length at most 500.
- `op` — `create` or `index` only (deliberately no `update`/`delete` in v1).
- `id` — required for both ops: writes are idempotent by construction.
- `doc` — arbitrary JSON object, serialized opaquely and never echoed back.

Output on Success (replaces the input message):

```json
{
  "accepted": 2,
  "duplicates": 1,
  "total": 3,
  "results": [ { "status": 201 }, { "status": 409 }, { "status": 201 } ]
}
```

`409` on a `create` op counts as an idempotent duplicate, not an error.

Output on Error (routed to the Error connection):

```json
{
  "classification": "retryable",
  "reason": "bulk item status 429 on index budva-events-v1 op #3",
  "results": [ { "status": 201 }, { "status": 429 } ]
}
```

### Outcome classification

| Condition | Bucket |
|---|---|
| network error, timeout, context cancel | `retryable` |
| HTTP 408 / 429 / 5xx on the request | `retryable` |
| HTTP 401 / 403 (request or per-item) | `auth` — stop retrying, alert |
| HTTP 400 / 404 (request or per-item), non-bulk error shape | `terminal` |
| per-item 409 on `create` | duplicate (success) |
| per-item 409 on `index` | `terminal` |
| per-item 400 / 404 | `terminal` |
| per-item 429 / 5xx | `retryable` |
| per-item missing or malformed | `terminal` |

Overall request outcome = worst bucket (auth > terminal > retryable);
duplicates and 2xx are ok. The node routes to Success only when nothing is
retryable or auth-failed; otherwise it routes to Error with the structured
diagnosis above. **The node never retries by itself** — retry/abort is graph
policy (RuleGo `retry`/`delay`/`filter` nodes), which is observable and
tunable without holding the chain executor.

## Installing

1. Copy the built artifact (see CI release artifacts,
   `elasticsearch-write-rulego-v<VERSION>-linux-<arch>.so`) to the server's
   `data/plugins/` directory.
2. Restart the `rulego-server` container (plugins load at boot from
   `data/plugins/*.so`).
3. Verify registration: `GET /api/v1/components` lists `elasticsearchWrite`.

The ABI contract is proven only in the digest-pinned CI environment; the
artifact's sidecar files (`.sha256`, `.abi.json`) record the exact SDK,
runtime, and host contract the `.so` was built against. Verify them before
installing:

```sh
sha256sum --check elasticsearch-write-rulego-v0.1.0-linux-amd64.so.sha256
```

## Deployment notes

- **Inbound body cap**: the RuleGo server's `MaxBodySize` (MiB, 0 =
  unlimited) caps rule-trigger request bodies. A small cap can truncate a
  large `operations` array; size it to your batch size.
- **Idempotency**: with `op: create` and a caller-derived `_id`, redelivered
  events are safely absorbed (409 → duplicate). This is what makes
  at-most-once producers safe.
- **Auth**: API key is the assumed mechanism for ES 9.x; basic is supported.
- **TLS**: verification is always enabled. To trust an internal CA, mount
  the certificate and point `tls.caFile` at it.

## Repository layout

| File | Purpose |
|---|---|
| `plugin.go` | `Plugins` symbol (host ABI entrypoint) |
| `node.go` | `elasticsearchWrite` node + SharedNode wiring |
| `client.go` | `esClient`: HTTP client, auth, TLS, NDJSON bulk builder |
| `classify.go` | bulk response parsing + outcome classification |
| `compatibility.json` / `plugin-abi-release.json` / `VERSION` | release metadata (CI-verified) |
| `testdata/` | smoke node_pool + borrower chain used by CI |
| `examples/` | consumer profile examples (see `examples/` README) |
