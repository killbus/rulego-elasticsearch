# Research: verified ground facts (2026-10-27)

Re-verified against local sources, not prior conversation. Re-inspect
before relying on a volatile value.

## RuleGo source (local checkout /home/agent/Src/rulego)

- Head commit `45d1a6c`, `git describe` = `v1.0.0`. Core `go.mod`:
  `module github.com/rulego/rulego`, `go 1.20`.
- JS engine is **plain goja** (`utils/js/js_engine.go`: `goja.New()`,
  `vm.Set` of fromVars / global / UDFs). No `crypto` builtin, no nodejs
  builtins. UDFs are either compiled JS programs or Go funcs set on the
  VM. (Earlier assumption of a custom `ln` engine was wrong.)
- Plugin loading (server boot): `server/internal/engine/manager.go`
  `loadPlugins()` globs `data/plugins/*.so` and calls
  `rulego.Registry.RegisterPlugin(path.Base(file), file)`.
- `RegisterPlugin` -> `PluginComponentRegistry.Init()` -> `loadPlugin(file)`:
  `plugin.Open(file)` + `Lookup(PluginsSymbol)`.
  `PluginsSymbol = "Plugins"` (engine/registry.go:36). Type-asserts the
  symbol to `types.PluginRegistry`.
- `types.PluginRegistry` (api/types/types.go:286): `Init() error`,
  `Components() []Node`. **`Init()` is NOT called by the loader** — only
  `Components()`. Keep `Init()` no-op.
- `types.Node` (api/types/types.go:528) is a factory/prototype:
  `New() types.Node`, `Type() string`, `Init(ruleConfig, configuration)
  error`, `OnMsg(ctx, msg)`, `Destroy()`. `New()` returns a fresh instance
  per chain (data isolation). Register the *prototype*; per-chain state
  goes on the instance returned by `New()`.
- `base.SharedNode[T]` (components/base/base.go:200) provides
  `Init(ruleConfig, nodeType, resourcePath, initNow, initInstanceFunc)`,
  `InitWithClose...`, `InitWithCloseSoftFail` (soft-fail => Reconnecting,
  retries after cooldown — right for an ES client that may be down at
  boot). `resourcePath` of form `ref://<id>` borrows from the node pool;
  otherwise it constructs its own client. `NodeUtils.GetInstanceId`
  parses the `ref://` prefix.
- `restApiCall` footguns being replaced (components/external/
  rest_api_call_node.go): `InsecureSkipVerify` default `true` (line 233),
  success only on `response.StatusCode == 200` (lines 296/310), default
  `ReadTimeoutMs: 2000` (line 231). No per-item bulk parsing.

## HTTP endpoint inbound (server -> chain)

- `endpoint/rest/rest.go` `RequestMessage.GetMsg()`: for non-GET, if
  `Content-Type` starts with a JSON context type then `dataType=JSON` else
  TEXT; `data = string(r.Body())`; `r.Body()` lazily `io.ReadAll`s and
  caches. `r.Headers()` returns `textproto.MIMEHeader`. `r.Metadata` is a
  `*types.Metadata` carried into `types.NewMsg(0, r.From(), dataType,
  r.Metadata, data)`.
- So the chain receives: message **data = raw body string** (verbatim),
  and the request `Metadata` (not the full header map by default). Custom
  headers like `X-Budva-Signature` are reachable via `r.Headers()` at the
  endpoint layer, but whether they land in the chain message metadata
  needs verification from the endpoint wiring (see example design). If
  they do not, HMAC is enforced by a fronting proxy or a pre-chain node.

## Host / Plugin ABI (killbus/rulego-server-docker)

- `plugin-abi.lock` recomputed: `lock_digest = sha256:e19c9657ede2e9da86aae38c2ab972d32ff22aa2f2c8f1aa327714967baf8f80`
  (canonical manifest: `jq -S -c . plugin-abi.lock | tr -d '\r' | sha256sum`,
  host `scripts/plugin-abi.sh lock_digest`; this is the value in
  `plugin-abi-release.json` and in the SDK sidecars / runtime image label
  `io.rulego.abi.lock.digest`). The raw file's `sha256sum` is
  `sha256:70e937ace5bd5cce7271ef141267caa28397bd1932de6e97c8d3b9f48d27bc34` —
  a byte-layout digest, not the contract value.
  `abi_id = abi-3e0c9d46dddaf6235cfe609d543222f07f8e1abf3374f0669bf703df85063885`.
  Identical at working tree and at packaging revision
  `006436e34d1fa2cd811f840547724fb0ace3605b` (main).
- Contract: Go 1.25.0, CGO=1, linux/amd64+arm64, glibc>=2.34,
  `GOTOOLCHAIN=local`, `GOENV=off`, `GOFLAGS=-mod=readonly`,
  `plugin_symbol=Plugins`, `host_mode=exe`, `plugin_mode=plugin`.
- SDK entrypoint: `rulego-plugin-build <pkg> <out>`. It creates a go.work of
  plugin + host source, enforces `CGO=1`, checks the plugin's resolved
  deps do not change any host-owned module identity, then `go build
  -buildmode=plugin`. **The plugin must be a Go module with a `go.mod`.**
- Reference plugin repo layout = `rulego-youtube-vod`: single root module
  `package main`, `plugin.go` = `var Plugins`, `node.go`,
  `VERSION`, `compatibility.json`, `plugin-abi-release.json`,
  `.github/workflows/{ci,release}.yml`, `testdata/`, `tests/`.
- The release record consumed by a plugin repo is `plugin-abi-release.json`
  (sdk+runtime+abi_id+lock_digest+packaging_revision digests), held today
  by `rulego-youtube-vod` for the v1.0.0-ABI.

## Budva push contract (from /home/agent/Src/budva, schema v2)

- Files: `internal/service/push/doc.go`, `README.md`,
  `messagev2/serializer_test.go`, `.trellis/tasks/10-11-external-push-sink/`.
- `schema_version: 2`; events `new|edited|deleted`; `occurred_at` = Budva
  observation time; no event id; at-most-once (1 attempt, no retry,
  in-memory 256 buffer); deleted = `chat_id`+`message_id`+details only;
  optional `X-Budva-Signature: sha256=<hex>` over exact raw body; entity
  offsets are UTF-16 code units.

## Open items to verify during implementation (do NOT guess)

1. Does the deployed HTTP endpoint surface custom request headers (e.g.
   `X-Budva-Signature`) into the chain message metadata, or only the body?
   Determine from `server/internal/endpoint/*` routing to
   `RequestMessage.Metadata`. Drives where HMAC enforcement lives.
2. HTTP endpoint `wait`/sync semantics: does the route block until the
   chain completes so the response reflects the ES write outcome? Verify
   the exact route attribute for synchronous response.
3. Confirm `ref://` node-pool borrower + owner registration shape for this
   RuleGo version (mirror `rulego-youtube-vod/testdata/smoke-node-pool.json`).
