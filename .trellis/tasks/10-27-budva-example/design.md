# Design: Budva example profile

## Placement

All content lives under `examples/budva/` and is never imported by the Go
plugin package. The boundary proof is mechanical: `go test ./...` at the
root must stay green with the example deleted.

## Data flow (deployed)

```
Budva (at-most-once, 1 attempt)
  | POST application/json  X-Budva-Signature: sha256=<hex>
  v
RuleGo HTTP endpoint (route /budva/push, wait=true semantics verified from
  source during implementation)
  | raw body as msg data (string), msg id/type = route
  v
jsTransform UDF "budvaParse" (parser.js, registered from data/js at boot)
  | 1. strict-parse; require schema_version == 2
  | 2. HMAC: if header present -> pure-JS sha256(rawBodyBytes) vs claim,
  |    constant-time compare; mismatch or empty claim policy per README
  |    (default: header optional only while key not configured)
  | 3. emit generic message:
  |    index + operations[create events, index messages]
  v
elasticsearchWrite (ref://es)
  |-- Success -> endpoint responds 202
  +-- Error   -> jsLog diagnostic + endpoint responds 502
                  (Budva will NOT retry; 5xx is the honest signal)
```

## _id strategy

- `budva-events-v1` (audit, append-only): `_id = hex(sha256(raw body bytes))`.
  Same body delivered twice -> same `_id` -> 409 on create -> idempotent
  success. Different bodies always collide on nothing. This makes the audit
  stream exactly the set of distinct raw deliveries observed.
- `budva-messages-v1` (current state): `_id = "<chat_id>:<message_id>"`,
  op `index` (unconditional overwrite). Ordering fact is `occurred_at`
  (Budva observation time). `deleted` events write a tombstone doc
  (`tombstone: true`, keep `occurred_at`, drop body fields) — physical
  deletes are banned (audit index is the recovery path).

LWW honesty note (goes in README): Budva is at-most-once and single-
attempt, so within one process lifetime messages for a chat are delivered
in observation order; the failure mode is *missing* updates, not *reversed*
updates. Reversal can only come from a process boundary restart whose
buffer held an older edit; the audit index makes that visible
(`occurred_at` ordering per `_id`), and reconciliation is a query, not a
write-path feature.

## HMAC details

- Header value: `sha256=<64 hex>`. Recompute over the exact raw bytes
  (UTF-8 of the msg data string — the endpoint passes the body verbatim).
- Constant-time compare in JS: xor-accumulate over equal-length inputs,
  reject on length mismatch.
- Key: from the chain's UDF variables/properties at deploy time
  (deployment secret mechanism), never in the committed file. The example
  ships with an empty-key config and README instructions.
- If the v1.0.0 endpoint does not surface custom request headers into the
  chain (verify: `endpoint/rest/rest.go` metadata construction), the
  documented fallback is a thin fronting proxy that rejects bad
  signatures before RuleGo, or a second plugin node later. Decision is
  recorded here during implementation; the chain JSON ships in whichever
  form is verified.

## JS sha256

Plain goja (`goja.New()`) has no `crypto` builtin. Vendor a public-domain
pure-JS sha256 (subtle crypto or js-sha256 style, MIT/public-domain,
attribution header kept). Deterministic test vectors (empty string, "abc",
64-byte block boundary, multi-block >2^32-bit path not needed) in the Go
harness test that runs the UDF through the same `GojaJsEngine` the server
uses.

## Mapping discipline (both indices)

- `dynamic: false` at root; only documented fields mapped.
- ids `long`; `date`/`occurred_at` `date` format
  `strict_date_optional_time||epoch_millis`; discriminators
  (`schema_version`, `event`, entity `type`) `keyword`; `entities`
  `nested`; `raw` `object` with `enabled: false`; unknown extra fields are
  dropped (not indexed, not stored) — the raw object preserves them.
- No analyzers beyond standard; no ILM/aliases in v1 (deployment note).

## Response semantics

- 202 = bulk accepted (incl. duplicates). 502 = classified failure, body
  carries the node's structured diagnosis. 413 if the server's MaxBodySize
  trips (deployment must size it for the expected burst; note in README).
- The chain must be `wait` so the HTTP response reflects the write;
  verify RuleGo HTTP endpoint wait/async semantics from source and pin the
  exact route attribute in chain.json.
