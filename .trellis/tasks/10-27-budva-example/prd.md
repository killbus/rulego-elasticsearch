# PRD: Budva webhook example profile

## Outcome

A complete, deployable example under `examples/budva/` that proves the
generic `elasticsearchWrite` node against the real Budva push contract —
without any Budva knowledge compiled into the plugin. Everything here is
RuleGo-native content: JS transform, chain JSON, index mappings, fixtures.

This task is the consumer-side half of the parent's acceptance: it must
compose with the node as deployed (HTTP endpoint -> chain), not against an
invented API.

## Budva contract (facts from /home/agent/Src/budva, schema v2)

- `schema_version: 2`; events `new | edited | deleted`.
- `occurred_at` = Budva observation time (NOT Telegram send/edit time).
- No event id from Budva; delivery is at-most-once (one attempt, no retry,
  in-memory 256 buffer). Consumers must be idempotent and tolerate missing
  predecessors; last-write-wins keyed by `occurred_at`.
- `deleted` events carry `chat_id`, `message_id`, and deletion details only.
- Optional HMAC: `X-Budva-Signature: sha256=<hex>` over the exact raw body.
- Offsets in entity data are UTF-16 code units.
- Payload shape: `message_id`, `chat_id`, `date`, `text`, optional
  `entities` (offsets + length + type), optional `caption`, sender info,
  album/media fields as documented in
  `budva/internal/service/push/messagev2/`.

## Deliverables

1. `examples/budva/README.md` — deployment walk-through: endpoint route,
   chain, node_pool owner with secret refs, index creation commands,
   HMAC key setup, and the explicit loss model (what is lost when, and why
   the design tolerates it).
2. `examples/budva/parser.js` — jsTransform script (UDF): validates
   `schema_version == 2`, extracts/derives, outputs the generic
   `elasticsearchWrite` message:
   - `budva-events-v1` op: `create`, `_id = sha256(rawBodyHex)` (hex of the
     canonical raw bytes the endpoint received) — duplicate-safe audit
     stream.
   - `budva-messages-v1` op: `index`, `_id = "<chat_id>:<message_id>"`,
     document carries `occurred_at` for LWW and, for `deleted`, a
     `tombstone: true` marker (no physical deletes).
   - HMAC verification: when `X-Budva-Signature` is present, recompute
     sha256 over the raw body and constant-time compare; mismatch -> drop
     with Error. The raw body arrives as the message data string
     (`Content-Type: application/json`), so the sha256 input is the UTF-8
     bytes of that string. The signature header is exposed to the chain via
     the endpoint's metadata/header mechanism (see design.md in parent task
     for the verified mechanism; if v1.0.0 does not surface arbitrary
     request headers into the chain, the example documents that HMAC must
     be enforced by the fronting proxy or a pre-chain node — decide during
     implementation from source, do not guess).
   - JS crypto: the v1.0.0 JS engine is plain goja (`utils/js/js_engine.go`,
     `goja.New()` without nodejs builtins) — there is no `crypto` builtin.
     Therefore sha256 must be implemented as a pure-JS sha256 in `parser.js`
     (well-known public-domain JS implementation, ~150 lines, vendored in
     the example with attribution) or the example documents using a
     pre-chain node. Pure-JS sha256 is preferred: self-contained, testable.
3. `examples/budva/chain.json` — the rule chain: HTTP endpoint trigger ->
   HMAC/validate (jsTransform) -> `elasticsearchWrite` (ref://es) ->
   onError: log (jsLog) ; onSuccess: respond 202. Wait semantics: the
   endpoint route must block until the chain finishes so the HTTP response
   reflects the write outcome (verify the `wait` behavior from source).
4. `examples/budva/mappings/budva-events-v1.json` — append-only audit
   mapping: `dynamic: false`, `raw` disabled object, ids `long`,
   `occurred_at`/`date` `date` with `strict_date_optional_time||epoch_millis`,
   discriminators `keyword`, entities `nested`.
5. `examples/budva/mappings/budva-messages-v1.json` — current-state mapping,
   same discipline, plus `tombstone` boolean.
6. `examples/budva/fixtures/` — `new.json`, `edited.json`, `deleted.json`
   real-shape sample payloads (synthetic ids) used for local testing.
7. LWW: the `index` op overwrites unconditionally; the document's
   `occurred_at` is the ordering fact. (True LWW "only-if-newer" needs a
   script update or compare-and-swap, which ES bulk does not offer in v1;
   README states the accepted model: Budva sends in observation order per
   chat, late duplicates are rare and bounded by the at-most-once window;
   the audit index preserves the full order for reconciliation.)

## Acceptance

- All JSON lints; `parser.js` runs under a goja harness test (node test in
  this repo can compile+run the UDF with fixture payloads and assert the
  two operations, `_id`s, and tombstone flag — no network).
- HMAC test vectors: one valid + one tampered body assert accept/drop.
- README's deployment steps are copy-pasteable against the deployed
  rulego-server-docker plugin runtime (no secrets in any committed file).
- No file under `examples/budva/` is imported by the Go plugin package.
