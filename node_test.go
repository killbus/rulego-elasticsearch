package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rulego/rulego"
	"github.com/rulego/rulego/api/types"
	"github.com/rulego/rulego/node_pool"
)

// nodeResultContext captures what a node tells the chain, mirroring the
// reference repo's test double. It also captures the failure error so tests
// can anchor the human-readable reason (design.md: the error "also receives
// the human-readable reason").
type nodeResultContext struct {
	types.RuleContext
	output   types.RuleMsg
	relation string
	failErr  error
}

func (*nodeResultContext) GetContext() context.Context { return context.Background() }
func (c *nodeResultContext) TellSuccess(msg types.RuleMsg) {
	c.output, c.relation = msg, types.Success
}
func (c *nodeResultContext) TellFailure(msg types.RuleMsg, err error) {
	c.output, c.relation, c.failErr = msg, types.Failure, err
}

// bulkServer is a loopback stand-in for the ES _bulk endpoint: it returns a
// fixed (status, body) and counts hits so tests can prove the ref:// borrow
// path reached the owner's client. Loopback only; no real ES.
type bulkServer struct {
	*httptest.Server
	status int
	body   string
	hits   atomic.Int32
}

func newBulkServer(t *testing.T, status int, body string) *bulkServer {
	t.Helper()
	srv := &bulkServer{status: status, body: body}
	srv.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.hits.Add(1)
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/_bulk") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// inputWith builds one message-contract body (index + operations).
func inputWith(index string, ops ...map[string]any) string {
	body, _ := json.Marshal(map[string]any{"index": index, "operations": ops})
	return string(body)
}

func createOp(id string, doc map[string]any) map[string]any {
	return map[string]any{"op": "create", "id": id, "doc": doc}
}

func indexOp(id string, doc map[string]any) map[string]any {
	return map[string]any{"op": "index", "id": id, "doc": doc}
}

// runNode initializes a direct-owner node against the server, runs one
// message, and returns the captured result context.
func runNode(t *testing.T, srv *bulkServer, input string) *nodeResultContext {
	t.Helper()
	node := &elasticsearchWriteNode{}
	if err := node.Init(rulego.NewConfig(), types.Configuration{"server": srv.URL}); err != nil {
		t.Fatal(err)
	}
	defer node.Destroy()
	ctx := &nodeResultContext{}
	node.OnMsg(ctx, types.NewMsgWithJsonData(input))
	return ctx
}

// TestNodeContract anchors the component's registration surface: type name,
// New() identity, and the Success|Failure relation set the graph can route on.
func TestNodeContract(t *testing.T) {
	node := &elasticsearchWriteNode{}
	if node.Type() != "elasticsearchWrite" {
		t.Fatalf("type = %q, want elasticsearchWrite", node.Type())
	}
	if _, ok := node.New().(*elasticsearchWriteNode); !ok {
		t.Fatalf("New() = %T, want *elasticsearchWriteNode", node.New())
	}
	def := node.Def()
	if def.Type != "elasticsearchWrite" || def.ComponentKind != types.ComponentKindNative {
		t.Fatalf("def = %#v", def)
	}
	want := []string{types.Success, types.Failure}
	if def.RelationTypes == nil || !reflect.DeepEqual(*def.RelationTypes, want) {
		t.Fatalf("relations = %#v, want %#v", *def.RelationTypes, want)
	}
}

// TestNodeInitValidatesOwnerConfig anchors the owner-config gate: missing
// server and half-configured auth fail at Init, before any network I/O.
func TestNodeInitValidatesOwnerConfig(t *testing.T) {
	if err := (&elasticsearchWriteNode{}).Init(rulego.NewConfig(), types.Configuration{}); err == nil ||
		!strings.Contains(err.Error(), "server is required") {
		t.Fatalf("missing server: err = %v", err)
	}
	err := (&elasticsearchWriteNode{}).Init(rulego.NewConfig(), types.Configuration{
		"server": "http://127.0.0.1:9200",
		"auth":   map[string]any{"type": "basic"},
	})
	if err == nil || !strings.Contains(err.Error(), "username") {
		t.Fatalf("basic auth without username: err = %v", err)
	}
}

// PRD acceptance row 1: every item accepted -> Success with the per-item
// summary (design.md success shape: accepted/duplicates/total/results).
func TestNodeAllAcceptedRoutesSuccess(t *testing.T) {
	srv := newBulkServer(t, 200, `{"took":5,"errors":false,"items":[`+
		`{"create":{"_index":"budva-events-v1","_id":"1","_version":1,"result":"created","status":201}},`+
		`{"index":{"_index":"budva-events-v1","_id":"2","_version":2,"result":"updated","status":200}}]}`)
	ctx := runNode(t, srv, inputWith("budva-events-v1",
		createOp("1", map[string]any{"event": "new"}),
		indexOp("2", map[string]any{"event": "two"})))
	if ctx.relation != types.Success {
		t.Fatalf("relation = %q, want Success; output=%s", ctx.relation, ctx.output.GetData())
	}
	var out writeSummary
	if err := json.Unmarshal(ctx.output.GetBytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Accepted != 2 || out.Duplicates != 0 || out.Total != 2 || len(out.Results) != 2 {
		t.Fatalf("summary = %+v", out)
	}
	if out.Results[0].Status != 201 || out.Results[0].Op != "create" || out.Results[0].Class != classificationOK {
		t.Fatalf("results[0] = %+v", out.Results[0])
	}
	if out.Results[1].Status != 200 || out.Results[1].Op != "index" || out.Results[1].Class != classificationOK {
		t.Fatalf("results[1] = %+v", out.Results[1])
	}
}

// PRD acceptance row 2: one retryable item -> Failure with the structured
// diagnosis (classification + reason naming the offending op).
func TestNodeOneRetryableRoutesError(t *testing.T) {
	srv := newBulkServer(t, 200, `{"took":4,"errors":true,"items":[`+
		`{"create":{"_index":"budva-events-v1","_id":"1","status":201}},`+
		`{"index":{"_index":"budva-events-v1","_id":"2","status":429,"error":{"type":"es_rejected_execution_exception","reason":"[budva-events-v1][0] rejected execution (queue full)"}}}]}`)
	ctx := runNode(t, srv, inputWith("budva-events-v1",
		createOp("1", map[string]any{"event": "new"}),
		indexOp("2", map[string]any{"event": "two"})))
	if ctx.relation != types.Failure {
		t.Fatalf("relation = %q, want Failure", ctx.relation)
	}
	var out writeFailure
	if err := json.Unmarshal(ctx.output.GetBytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Classification != classificationRetryable {
		t.Fatalf("classification = %q, want retryable", out.Classification)
	}
	if out.Reason != "bulk item status 429 on index budva-events-v1 op #2" {
		t.Fatalf("reason = %q", out.Reason)
	}
	if len(out.Results) != 2 {
		t.Fatalf("results = %+v, want 2 per-item entries", out.Results)
	}
	if ctx.failErr == nil || !strings.Contains(ctx.failErr.Error(), "status 429") {
		t.Fatalf("failErr = %v, want the human-readable reason", ctx.failErr)
	}
}

// PRD acceptance row 3: 409 on create is the idempotent path -> Success with
// the item counted as a duplicate, not an error.
func TestNode409OnCreateCountsAsDuplicateSuccess(t *testing.T) {
	srv := newBulkServer(t, 200, `{"took":3,"errors":true,"items":[`+
		`{"create":{"_index":"budva-events-v1","_id":"1","status":409,"error":{"type":"version_conflict_engine_exception","reason":"[1]: version conflict, document already exists"}}}]}`)
	ctx := runNode(t, srv, inputWith("budva-events-v1",
		createOp("1", map[string]any{"event": "new"})))
	if ctx.relation != types.Success {
		t.Fatalf("relation = %q, want Success (409 on create is idempotent)", ctx.relation)
	}
	var out writeSummary
	if err := json.Unmarshal(ctx.output.GetBytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Accepted != 0 || out.Duplicates != 1 || out.Total != 1 {
		t.Fatalf("summary = %+v, want accepted=0 duplicates=1 total=1", out)
	}
	if len(out.Results) != 1 || out.Results[0].Status != 409 || out.Results[0].Class != classificationDuplicate {
		t.Fatalf("results = %+v", out.Results)
	}
}

// PRD acceptance row 4: a 400 item is terminal -> Failure with no retry
// signal (the same bytes cannot succeed).
func TestNode400ItemRoutesTerminalError(t *testing.T) {
	srv := newBulkServer(t, 200, `{"took":2,"errors":true,"items":[`+
		`{"create":{"_index":"budva-events-v1","_id":"1","status":400,"error":{"type":"mapper_parsing_exception","reason":"failed to parse field [chat_id] of type [long]"}}}]}`)
	ctx := runNode(t, srv, inputWith("budva-events-v1",
		createOp("1", map[string]any{"event": "new"})))
	if ctx.relation != types.Failure {
		t.Fatalf("relation = %q, want Failure", ctx.relation)
	}
	var out writeFailure
	if err := json.Unmarshal(ctx.output.GetBytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Classification != classificationTerminal {
		t.Fatalf("classification = %q, want terminal (asserted: not %q)", out.Classification, classificationRetryable)
	}
	if out.Reason != "bulk item status 400 on index budva-events-v1 op #1" {
		t.Fatalf("reason = %q", out.Reason)
	}
}

// PRD acceptance row 5: top-level 401 -> Failure marked auth, no retry
// signal, and an empty (not null) results array.
func TestNodeTopLevel401RoutesAuthError(t *testing.T) {
	srv := newBulkServer(t, 401, `{"error":{"type":"security_exception","reason":"unable to authenticate user [budva-svc] for authentication"},"status":401}`)
	ctx := runNode(t, srv, inputWith("budva-events-v1",
		createOp("1", map[string]any{"event": "new"})))
	if ctx.relation != types.Failure {
		t.Fatalf("relation = %q, want Failure", ctx.relation)
	}
	var out writeFailure
	if err := json.Unmarshal(ctx.output.GetBytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Classification != classificationAuth {
		t.Fatalf("classification = %q, want auth (asserted: not %q)", out.Classification, classificationRetryable)
	}
	if out.Results == nil || len(out.Results) != 0 {
		t.Fatalf("results = %#v, want an empty array, never null", out.Results)
	}
}

// PRD acceptance row 6: HTTP 429 on the request itself -> Failure marked
// retryable (queue full is transient by ES semantics).
func TestNodeHTTP429RoutesRetryableError(t *testing.T) {
	srv := newBulkServer(t, 429, "")
	ctx := runNode(t, srv, inputWith("budva-events-v1",
		createOp("1", map[string]any{"event": "new"})))
	if ctx.relation != types.Failure {
		t.Fatalf("relation = %q, want Failure", ctx.relation)
	}
	var out writeFailure
	if err := json.Unmarshal(ctx.output.GetBytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Classification != classificationRetryable {
		t.Fatalf("classification = %q, want retryable", out.Classification)
	}
	if out.Results == nil || len(out.Results) != 0 {
		t.Fatalf("results = %#v, want an empty array, never null", out.Results)
	}
}

// PRD acceptance row 7: a 200 that is not bulk-shaped is terminal with the
// raw diagnostic text (mirrors the M3 parseBulkResponse contract).
func TestNodeMalformedBulkResponseRoutesTerminal(t *testing.T) {
	srv := newBulkServer(t, 200, `{"took":5,"items":`)
	ctx := runNode(t, srv, inputWith("budva-events-v1",
		createOp("1", map[string]any{"event": "new"})))
	if ctx.relation != types.Failure {
		t.Fatalf("relation = %q, want Failure", ctx.relation)
	}
	var out writeFailure
	if err := json.Unmarshal(ctx.output.GetBytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Classification != classificationTerminal {
		t.Fatalf("classification = %q, want terminal", out.Classification)
	}
	if !strings.Contains(out.Reason, "malformed bulk response") {
		t.Fatalf("reason = %q, want the raw malformed-bulk diagnostic", out.Reason)
	}
}

// ref:// borrow path: the borrower carries no credentials; it must resolve
// the owner's esClient through the node pool (base.SharedNode contract), so
// the owner's client is what hits the endpoint.
func TestNodeRefBorrowUsesOwnerClient(t *testing.T) {
	if err := rulego.Registry.Register(&elasticsearchWriteNode{}); err != nil {
		t.Fatal(err)
	}
	defer rulego.Registry.Unregister(componentType)

	srv := newBulkServer(t, 200, `{"took":2,"errors":false,"items":[`+
		`{"create":{"_index":"budva-events-v1","_id":"1","status":201}}]}`)

	ownerDSL := fmt.Sprintf(`{
  "ruleChain":{"id":"es-pool","root":true},
  "metadata":{"nodes":[
    {"id":"es","type":"elasticsearchWrite","configuration":{"server":%q}}
  ]}
}`, srv.URL)
	pool := node_pool.NewNodePool(rulego.NewConfig())
	defer pool.Stop()
	if _, err := pool.Load([]byte(ownerDSL)); err != nil {
		t.Fatal(err)
	}

	cfg := rulego.NewConfig()
	cfg.NodePool = pool
	borrower := &elasticsearchWriteNode{}
	if err := borrower.Init(cfg, types.Configuration{"server": "ref://es"}); err != nil {
		t.Fatal(err)
	}
	defer borrower.Destroy()

	ctx := &nodeResultContext{}
	borrower.OnMsg(ctx, types.NewMsgWithJsonData(inputWith("budva-events-v1",
		createOp("1", map[string]any{"event": "borrowed"}))))
	if ctx.relation != types.Success {
		t.Fatalf("relation = %q, want Success; output=%s", ctx.relation, ctx.output.GetData())
	}
	if got := srv.hits.Load(); got == 0 {
		t.Fatal("borrower OnMsg never reached the owner's ES endpoint")
	}
}

// design.md: "Raw document bodies are never echoed back." A secret inside a
// doc must not appear in any downstream message body or error text.
func TestNodeNeverEchoesDocumentBodies(t *testing.T) {
	const marker = "sk-budva-DO-NOT-LEAK-XYZ123"
	doc := map[string]any{"secret": marker, "event": "new"}

	okServer := newBulkServer(t, 200, `{"took":1,"errors":false,"items":[`+
		`{"create":{"_index":"budva-events-v1","_id":"1","status":201}}]}`)
	assertNoEcho(t, runNode(t, okServer, inputWith("budva-events-v1", createOp("1", doc))), marker)

	errServer := newBulkServer(t, 200, `{"took":1,"errors":true,"items":[`+
		`{"create":{"_index":"budva-events-v1","_id":"1","status":400,"error":{"type":"mapper_parsing_exception","reason":"failed to parse field [chat_id] of type [long]"}}}]}`)
	assertNoEcho(t, runNode(t, errServer, inputWith("budva-events-v1", createOp("1", doc))), marker)
}

func assertNoEcho(t *testing.T, ctx *nodeResultContext, marker string) {
	t.Helper()
	if strings.Contains(ctx.output.GetData(), marker) {
		t.Fatalf("output echoes document content: %s", ctx.output.GetData())
	}
	if ctx.failErr != nil && strings.Contains(ctx.failErr.Error(), marker) {
		t.Fatalf("failure error echoes document content: %v", ctx.failErr)
	}
}
