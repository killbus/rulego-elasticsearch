# PRD: reusable elasticsearchWrite RuleGo plugin

## Outcome

A reusable RuleGo Go-plugin node, `elasticsearchWrite`, that writes documents
to Elasticsearch (target release line: 9.x, first consumer verified against
9.5.5) using only the Go standard library over HTTP `_bulk`. The node owns one
capability: sending a bounded batch of write operations to a named client and
classifying the per-item outcome. Nothing about any producer (Budva or
otherwise) appears in the node's configuration, message contract, or code.

The first consumer is a Budva webhook persistence profile, delivered as
example/config content in this repository (child task
`10-27-budva-example`), not as plugin semantics.

## Why a plugin at all

RuleGo's built-in `x/elasticsearch`-style needs are not met by
`restApiCall`:

- `restApiCall` (components/external/rest_api_call_node.go) defaults
  `InsecureSkipVerify: true` (line 233) and treats only HTTP 200 as success
  (lines 296/310);
- `_bulk` returns 200 even when individual items fail, and `create`/`index`
  single-doc writes return 201, so status-code-only judgment silently drops
  or double-counts writes;
- per-item `errors`/`items[*].status` parsing is a capability invariant of
  reliable writes and belongs in code, not in per-consumer JS.

## Requirements

1. One node type, `elasticsearchWrite`, delivered as a `.so` plugin that loads
   in the `rulego-server-docker` plugin runtime (RuleGo v1.0.0, ABI
   `abi-3e0c9d46dddaf6235cfe609d543222f07f8e1abf3374f0669bf703df85063885`).
2. The node writes documents via `POST _bulk` using stdlib `net/http` only.
   No third-party ES client dependency.
3. Client connection policy (URL, auth, TLS, timeout) is node configuration
   that supports both an inline owner instance and `ref://` sharing through a
   `node_pool.json` owner (`base.SharedNode`).
4. Per-item response classification is the node's core contract
   (see design.md): accepted / idempotent-duplicate / retryable / terminal /
   auth-failure. Duplicate `409` on `create` is accepted as idempotent
   success.
5. The node never retries by itself. Retry/abort policy is graph policy. The
   node routes to Success only when nothing is retryable or auth-failed;
   otherwise it routes to Error with a structured diagnostic message.
6. Node contract tests run under plain `go test ./...` with `httptest`
   fixtures; no live Elasticsearch, no production URL, no network.
7. CI builds the `.so` per-architecture in the digest-pinned SDK, verifies
   sidecars and embedded module identity, smoke-loads the node in the pinned
   runtime, and registers the `elasticsearchWrite` component (mirroring the
   `rulego-youtube-vod` ci.yml shape).
8. No Beanstalkd, no durable queue, no DLQ, no inbound webhook logic in this
   repository. Durability, if ever required, is a separate transport
   artifact per `rulego-engineering` capability-design rules.

## Non-goals

- Budva parsing, index selection, document-id derivation, LWW, HMAC
  verification (example/profile, child task).
- Mapping/index template management (deployment content; shipped as example
  JSON only).
- Multi-index fan-out, query/update-by-query, aliases, ILM.
- Exactly-once delivery guarantees (ES writes are idempotent-safe; at-least
  once is the chain's property, not this node's).

## Acceptance

- `go test ./...` green on linux/amd64; `gofmt`/`go vet` clean.
- Node tests prove each classification bucket with fixture bulk responses,
  including: all-accepted -> Success; one retryable -> Error with structured
  diagnosis; one 409 create -> Success with duplicate counted; one 400 ->
  Error marked terminal (no retry signal); top-level 401 -> Error marked
  auth (no retry signal); HTTP 429 on the request -> Error marked retryable;
  malformed bulk response -> Error marked terminal with raw error text.
- CI green on a feature branch: test job + build job (amd64/arm64) with
  sidecar verification, module-identity check, and matching-runtime smoke
  load proving `elasticsearchWrite` registration.
- Example profile (child task) composes the node against Budva fixtures
  without the node knowing Budva exists.
