# Design: elasticsearchWrite node

## Boundary (first principles)

The capability: "send a bounded batch of write operations to one named
Elasticsearch client and report the per-item outcome." That is the only
lifecycle this node owns.

- Index selection, `_id` derivation, event parsing, dedup policy, LWW, and
  which op (`create`/`index`) to use are all facts the caller supplies per
  message. They live in the rule graph (JS/expr nodes) — the plugin must be
  usable by any producer without code changes.
- Retry/abort policy is graph policy (`retry`/`delay`/`filter` nodes). The
  node reports structured diagnoses; it does not loop.
- Durable ingestion (queue, DLQ) is a transport concern with its own process
  and invariants; if it is ever needed it becomes a separate plugin repo
  (`rulego-beanstalkd`), never a second node type in this `.so`.

Contract validity check: "elasticsearchWrite" is nameable without Budva; the
owner (this repo) can test the whole claimed behavior with an httptest
server.

## ABI / build contract (facts, re-verified 2026-10-27)

- Host: `killbus/rulego-server-docker` @ `006436e34d1fa2cd811f840547724fb0ace3605b`
  (main), `plugin-abi.lock` recomputed digest
  `sha256:70e937ace5bd5cce7271ef141267caa28397bd1932de6e97c8d3b9f48d27bc34`,
  `abi_id = abi-3e0c9d46dddaf6235cfe609d543222f07f8e1abf3374f0669bf703df85063885`.
- RuleGo Core `v1.0.0` @ `45d1a6c4e1af594d7319dd11df9fa58bd48fd0e0`;
  Go 1.25.0, CGO=1, linux/amd64+arm64, glibc>=2.34.
- Release record consumed by this repo: `plugin-abi-release.json` copied from
  the published v1.0.0-ABI record (currently held by
  `rulego-youtube-vod`): sdk
  `ghcr.io/killbus/rulego-plugin-sdk@sha256:c5fadb57...d7f2b8d01`, runtime
  `ghcr.io/killbus/rulego-server@sha256:53719b71...419`.
- Plugin contract (verified in RuleGo source): package `main`, exported var
  `Plugins` implementing `types.PluginRegistry{Init() error; Components()
  []types.Node}`; node types registered via
  `rulego.Registry.RegisterPlugin` at server boot from
  `/app/data/plugins/*.so`. `Init()` is defined by the interface but the
  host loader does not call it — keep it no-op; real setup happens in node
  `Init`/first use.

## Node configuration (stable instance policy)

```json
{
  "server": "ref://es",
  "timeoutMs": 30000,
  "maxRequestBytes": 31457280
}
```

Inline form (no pool): `"server": "https://es.internal:9200"` plus
`"auth": {"type": "apiKey"|"basic", ...}` and optional
`"tls": {"caFile": "/app/data/certs/es-ca.pem"}`.

- `server` — `ref://<id>` (node_pool owner) or direct base URL. Required.
- `auth` — apiKey sends `Authorization: ApiKey <key>`; basic sends
  `Authorization: Basic <b64>`. Secret values come from the deployment's
  secret mechanism (env expansion in node_pool config at deploy time);
  never hardcoded in examples.
- `tls.caFile` — optional extra CA; verification is always ON. There is no
  `insecureSkipVerify` knob, deliberately (the `restApiCall` default is a
  known footgun we are replacing).
- `timeoutMs` — per-request deadline, default 30000.
- `maxRequestBytes` — hard cap on serialized bulk body, default 30 MiB;
  exceeding it fails fast as terminal (graph can split batches; the node
  never splits).

`clientConfig` for the `node_pool.json` owner:

```json
{ "id": "es", "type": "elasticsearchWrite",
  "configuration": { "server": "https://es.internal:9200",
    "auth": { "type": "apiKey", "apiKey": "${ES_API_KEY}" },
    "timeoutMs": 30000 } }
```

Client construction (http.Client with timeout, TLS config) is shared through
`base.SharedNode[*esClient]`; borrowers use `ref://es` and hold no
credentials. Client init is lazy + soft-fail (Reconnecting status) so an ES
outage at boot does not kill the RuleGo process; failures surface per
message.

## Message contract

Input (per invocation), JSON:

```json
{
  "index": "budva-events-v1",
  "operations": [
    { "op": "create", "id": "9f2c...", "doc": { "event": "new", "chat_id": 1 } },
    { "op": "index",  "id": "1:-100", "doc": { "tombstone": true } }
  ]
}
```

- `index` — target index name (caller validates/chooses; node does not
  interpret it). Required, non-empty, must match `^[a-z0-9._-]+$` (ES index
  name rules) — cheap fail-fast before network.
- `operations` — non-empty array, length <= `maxOperations` (config,
  default 500).
- `op` — `create` or `index`. (`update`/`delete` are deliberately excluded
  for v1: delete is not needed (tombstones) and update adds LWW logic that
  belongs in the graph. Adding later is an additive change.)
- `id` — required for both ops (the whole point: idempotent writes).
- `doc` — arbitrary JSON object; node serializes it opaquely.

Output on Success:

```json
{ "accepted": 2, "duplicates": 1, "total": 3,
  "results": [ {"status": 201}, {"status": 409}, {"status": 201} ] }
```

The original input message is replaced (standard RuleGo node behavior:
`msg.SetData`), downstream graph nodes see only the outcome summary. Raw
document bodies are never echoed back (log-leak hygiene).

Output on Error (routed to Error connection):

```json
{ "classification": "retryable|terminal|auth",
  "reason": "bulk item status 429 on index budva-events-v1 op #3",
  "results": [ ... per-item statuses ... ] }
```

The Error message data carries the structured diagnosis; `ctx.TellFailure`
also receives the human-readable reason.

## Classification rules (the core contract)

Request-level (HTTP status of `POST /{index}/_bulk` or `/_bulk`):

| Condition | Classification |
|---|---|
| network error, timeout, context cancel | `retryable` |
| HTTP 408 / 429 / 5xx | `retryable` |
| HTTP 401 / 403 | `auth` (stop retrying, alert) |
| HTTP 400 with unparseable or non-bulk-shaped body | `terminal` (bad request shape / mapping) |
| HTTP 200 with `errors: true` | see per-item; overall = worst bucket (auth > terminal > retryable; duplicates and 2xx are "ok") |
| HTTP 200 with `errors: false` | Success |

Per-item (`items[i].create|index`):

| Status | Meaning | Bucket |
|---|---|---|
| 200 / 201 | written (index may update; create succeeded) | ok |
| 409 on `create` | already exists — intended idempotent path | duplicate (ok) |
| 409 on `index` | unexpected | terminal (conflict against index op) |
| 400 / 404 (index missing, mapping conflict, validation) | `terminal` |
| 401 / 403 | `auth` |
| 429 / 5xx | `retryable` |
| item missing or malformed | `terminal` (malformed response) |

`409` on `create` counts as `duplicates` in the success summary, not an
error — this is the at-most-once producer's (Budva) dedup safety net.

Rationale for not auto-retrying in-node: the upstream delivery is
at-most-once with no retry; a node-level retry loop would hold the chain
executor, hide backpressure, and duplicate work if the rule is reloaded.
Graph-level retry with delay is observable, tunable, and testable with
existing RuleGo nodes.

## Bulk encoding

- NDJSON: per operation, action line `{"create":{"_index":I,"_id":ID}}` then
  doc line. No routing (v1). No `?refresh=` (default `false`); callers that
  need read-your-writes set it via graph config later, not now.
- Action line `Content-Type: application/x-ndjson`.
- Body built with a `bytes.Buffer` bounded by `maxRequestBytes` before the
  HTTP call (fail fast, terminal).
- Single shared `http.Client` per owner (connection reuse); per-request
  `context.WithTimeout` from `timeoutMs` (not the client Timeout) so pool
  connections are not torn down per call.

## Repository layout

```
rulego-elasticsearch/
  go.mod                      module github.com/killbus/rulego-elasticsearch
  go.sum
  VERSION                     0.1.0
  compatibility.json          { pluginVersion, pluginAbiRelease, rulegoRelease }
  plugin-abi-release.json     digest-pinned SDK/runtime (v1.0.0-ABI record)
  plugin.go                   package main; var Plugins (PluginRegistry)
  node.go                     elasticsearchWrite node + SharedNode wiring
  client.go                   esClient: http.Client, auth, TLS, bulk builder
  classify.go                 bulk response parsing + classification
  node_test.go                node contract tests (httptest)
  classify_test.go
  client_test.go
  testdata/                   smoke node_pool.json, borrower chain.json
  examples/budva/             (child task content)
  .github/workflows/ci.yml    test + per-arch build + sidecar/module/smoke
  .github/workflows/release.yml
  README.md
  AGENTS.md / .trellis/       (already present)
```

Single Go module at repo root, `package main` — the SDK entrypoint expects
`<repo>/go.mod` + a package path (`.`); this matches `rulego-youtube-vod`.
Only dependency: `github.com/rulego/rulego v1.0.0` (stdlib-only otherwise).
The SDK's module-identity gate requires the plugin's resolved deps not drift
any host-owned module identity — keeping the dep set to just RuleGo Core
keeps the gate trivially satisfiable.

## Verification strategy

Local (fast, deterministic):
- `go test ./...` (httptest fixtures; no network beyond 127.0.0.1 loopback).
- `gofmt -l .` empty, `go vet ./...` clean.
- JSON lint of `testdata/*.json`, `examples/**/*.json`.

CI (authoritative for ABI claims):
- test job (ubuntu-24.04): metadata verification, gofmt, vet, test, race.
- build job (amd64 + arm64, pinned SDK by digest): build `.so`, sidecar
  checks (`sha256sum -c`, abi.json fields), `go version -m` module-identity
  check (path+mod = `github.com/killbus/rulego-elasticsearch`), smoke load
  in pinned runtime: `/api/v1/components` must list `elasticsearchWrite` and
  a borrower chain with `ref://es` must deploy and read back.
- Release: `workflow_dispatch` over a green CI run, tag-gated,
  `gh release` of the CI artifacts (no rebuild) — same shape as
  `rulego-youtube-vod/release.yml`.

Live-consumer proof (deployment step, outside this repo's CI):
point a deployed chain at the real ES with a test index; send one fixture
event; read back via `_search`. Per `rulego-engineering/runtime-verification`,
this evidence belongs to the deployment, and it is explicitly NOT a CI gate.

## Risks / open decisions

- `maxBodySize` server config: the RuleGo server caps inbound rule-trigger
  bodies (`MaxBodySize` MiB, 0=unlimited). A deployment that sets a small cap
  can truncate the operations array. Deployment note, not plugin logic —
  document in README.
- `index` name validation: ES 9 still enforces lowercase `[a-z0-9._-]`;
  keep the node check aligned to that. If a consumer needs uppercase
  aliases the graph can use them, and the node check stays — no knob.
- ES 9.x security: API key is the assumed mechanism (no basic auth in the
  examples); both are supported by the node.
- Version pinning: node is compatible with the 9.x line; the contract
  (NDJSON bulk + status codes) is stable across 7-9. README states
  "tested against 9.5.5".
