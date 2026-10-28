# Implementation plan: elasticsearchWrite

Work on a dedicated branch `feat/elasticsearch-write` (created after
`task.py start`). No pushes to default branch; CI runs on the branch/PR.
Commit fragmentation is consolidated at the merge gate.

## Ordered checklist

### M1 — Repository skeleton (no logic)
1. `go.mod`: module `github.com/killbus/rulego-elasticsearch`, `go 1.25.0`,
   require `github.com/rulego/rulego v1.0.0`. `go mod tidy`.
2. `VERSION` = `0.1.0`; `compatibility.json`;
   `plugin-abi-release.json` (copy of the current v1.0.0-ABI release record;
   verify `abi_id`/`lock_digest` against the live
   `killbus/rulego-server-docker@006436e` `plugin-abi.lock` — already
   recomputed and matched on 2026-10-27. `lock_digest` is the
   canonical-manifest digest (`jq -S -c . plugin-abi.lock | tr -d '\r' |
   sha256sum`, host `scripts/plugin-abi.sh lock_digest`), not the raw file's
   `sha256sum`; the published record value is
   `sha256:e19c9657ede2e9da86aae38c2ab972d32ff22aa2f2c8f1aa327714967baf8f80`).
3. `plugin.go` with `var Plugins` (no-op `Init`, `Components()` returning
   the node prototype).
4. `README.md` (contract, config reference, install into
   `data/plugins`, deployment notes incl. `MaxBodySize` warning).
5. `.gitignore` already exists (tmp/, dist/, *.so, mytoken.txt).
   Verification: `go build ./...` (compile only; local go 1.25.9 vs ABI
   go 1.25.0 is fine for non-ABI claims — record that ABI proof is CI-only).

### M2 — Client + bulk builder (`client.go`)
6. `esClient`: `http.Client` (no client-level Timeout; per-request ctx),
   auth (apiKey/basic), TLS (system pool + optional `caFile`; no skip-verify),
   base URL normalization (trailing slash).
7. `buildBulk(index string, ops []op, limit int64) ([]byte, error)` — NDJSON
   encoding, index-name regex, op whitelist (`create`/`index`), id required,
   bounded buffer (return terminal error past `maxRequestBytes`), cap on op
   count.
8. `unit tests`: encoding shape (action line + doc line order), reject
   unknown op, reject bad index name, reject oversize, auth header forms.
   Gate: `go test ./...` green.

### M3 — Classification (`classify.go`)
9. `parseBulkResponse(status int, body []byte)` -> structured result:
   per-item status + error reason; top-level `errors` flag.
10. `classify(...)` -> buckets per design.md table; worst-bucket rollup
    (auth > terminal > retryable > duplicate/ok); request-level table
    (network/timeout -> retryable; 401/403 -> auth; 429/5xx -> retryable;
    400 non-bulk -> terminal).
11. `unit tests`: one fixture per table row, incl. malformed JSON, missing
    items, mixed buckets, 409-on-create vs 409-on-index.
    Gate: `go test ./...` green.

### M4 — Node wiring (`node.go`)
12. `elasticsearchWriteNode` with `base.SharedNode[*esClient]`;
    configuration struct per design.md; `Init` via
    `SharedNode.Init` (lazy, soft-fail for pool owner startup tolerance);
    `OnMsg`: decode input (strict JSON, reject duplicate keys), build bulk,
    call with ctx timeout, classify, Success vs TellFailure with structured
    data; `Destroy`.
13. `node contract tests` (httptest): the acceptance matrix from prd.md,
    plus ref:// pool path test (register owner in a temp node_pool, node
    borrows), plus log-hygiene assertion (response never contains doc
    bodies).
    Gate: `go test ./...` + `go test -race ./...` green; `gofmt -l .` empty;
    `go vet ./...` clean.

### M5 — CI + smoke assets
14. `testdata/smoke-node-pool.json` (owner with dummy
    `http://127.0.0.1:1` — load-only) and
    `testdata/smoke-borrower-chain.json` (borrower chain referencing
    `ref://es`).
15. `.github/workflows/ci.yml` — mirror `rulego-youtube-vod/ci.yml`:
    test job (checkout root, go 1.25 via go.mod, metadata verify, gofmt,
    vet, test, race, JSON lint); build job matrix amd64/arm64 (checkout to
    `plugin/`, read `plugin/plugin-abi-release.json`, docker pull pinned
    SDK with retry, `docker run ... "$SDK_REF" . /output/<artifact>`,
    sidecar verify, `go version -m` module-identity, pull pinned runtime,
    verify `io.rulego.server.release` label = `v1.0.0`, smoke-load: copy
    `.so` to `data/plugins`, mount node_pool, poll
    `/api/v1/components`, assert `elasticsearchWrite` present, deploy
    borrower chain, read back); upload artifacts.
    Action pins: same pinned SHAs as the reference workflow.
16. `.github/workflows/release.yml` — tag-gated `workflow_dispatch` over a
    green CI run; `gh release` CI artifacts without rebuild.
    Gate: branch push -> CI green (the authoritative ABI evidence).

### M6 — Example profile (child task, after M5 is green)
17. Execute `10-27-budva-example` checklist (see that task's prd/design).
    Go harness test for `parser.js` using the vendored pure-JS sha256.
    Gate: `go test ./...` green incl. the UDF test; all example JSON lints.

### M7 — Wrap-up (Phase 3)
18. Spec update (`.trellis/spec/backend/` filled for this repo: directory
    structure, error handling = classification contract, quality rules =
    no-ES-client / no-skip-verify / no-secrets-in-examples).
19. Consolidate branch commits into the coherent set:
    (a) skeleton+contract files, (b) client+classify, (c) node+tests,
    (d) ci/release, (e) example profile. Rebase-merge to default only after
    user decision; `release` workflow dispatch after merge.

## Rollback points

- After M4: node is fully tested without CI; can pause here and review
  contract before spending CI.
- After M5: artifact is publishable; example can be deferred.
- Nothing here touches `rulego-server-docker`; no cross-repo rollback.

## Explicitly out of scope (do not expand)

- Beanstalkd/queue node, DLQ, webhook receiver of its own, mapping
  migration tooling, ES 8 compat matrix, exactly-once, update-by-query.
