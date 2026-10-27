package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildBulkEncodesActionAndDocLines(t *testing.T) {
	body, err := buildBulk("budva-events-v1", []bulkOp{
		{Op: "create", ID: "9f2c0a1e", Doc: json.RawMessage(`{"event":"new","chat_id":1}`)},
		{Op: "index", ID: "1:-100", Doc: json.RawMessage(`{"tombstone":true}`)},
	}, defaultMaxRequestBytes)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 4 {
		t.Fatalf("want 4 NDJSON lines (action,doc,action,doc), got %d: %q", len(lines), body)
	}
	var firstAction map[string]json.RawMessage
	if err := json.Unmarshal([]byte(lines[0]), &firstAction); err != nil {
		t.Fatalf("action line 1 not JSON: %v: %s", err, lines[0])
	}
	meta, ok := firstAction["create"]
	if !ok {
		t.Fatalf("action line 1 missing create op: %s", lines[0])
	}
	var metaObj struct {
		Index string `json:"_index"`
		ID    string `json:"_id"`
	}
	if err := json.Unmarshal(meta, &metaObj); err != nil {
		t.Fatal(err)
	}
	if metaObj.Index != "budva-events-v1" || metaObj.ID != "9f2c0a1e" {
		t.Fatalf("action meta = %+v", metaObj)
	}
	var secondAction map[string]json.RawMessage
	if err := json.Unmarshal([]byte(lines[2]), &secondAction); err != nil {
		t.Fatal(err)
	}
	if _, ok := secondAction["index"]; !ok {
		t.Fatalf("action line 2 missing index op: %s", lines[2])
	}
	// Doc lines must round-trip to the exact original objects.
	var doc1 map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &doc1); err != nil {
		t.Fatalf("doc line 1 not JSON: %v", err)
	}
	if doc1["event"] != "new" || doc1["chat_id"] != float64(1) {
		t.Fatalf("doc1 = %v", doc1)
	}
}

func TestBuildBulkCompactsMultilineDoc(t *testing.T) {
	pretty := `{"a":1,
"b":2}`
	body, err := buildBulk("i", []bulkOp{{Op: "index", ID: "1", Doc: json.RawMessage(pretty)}}, defaultMaxRequestBytes)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(strings.TrimSpace(string(body)), "\n"); got != 1 {
		t.Fatalf("multiline doc must compact to a single doc line, got %d newlines: %q", got, body)
	}
}

func TestBuildBulkRejectsUnknownOp(t *testing.T) {
	_, err := buildBulk("i", []bulkOp{{Op: "update", ID: "1", Doc: json.RawMessage(`{}`)}}, defaultMaxRequestBytes)
	if err == nil || !strings.Contains(err.Error(), "unsupported op") {
		t.Fatalf("want unsupported op error, got %v", err)
	}
}

func TestBuildBulkRejectsDeleteOp(t *testing.T) {
	_, err := buildBulk("i", []bulkOp{{Op: "delete", ID: "1", Doc: json.RawMessage(`{}`)}}, defaultMaxRequestBytes)
	if err == nil || !strings.Contains(err.Error(), "unsupported op") {
		t.Fatalf("want unsupported op error for delete, got %v", err)
	}
}

func TestBuildBulkRejectsBadIndexName(t *testing.T) {
	doc := json.RawMessage(`{}`)
	for _, name := range []string{"", "UPPER", "has space", "a/b", "has*star", "with'quote"} {
		if _, err := buildBulk(name, []bulkOp{{Op: "index", ID: "1", Doc: doc}}, defaultMaxRequestBytes); err == nil {
			t.Errorf("index %q must be rejected", name)
		}
	}
}

func TestBuildBulkAcceptsValidIndexNames(t *testing.T) {
	doc := json.RawMessage(`{}`)
	for _, name := range []string{"a", "budva-events-v1", "idx.v2_x-3"} {
		if _, err := buildBulk(name, []bulkOp{{Op: "index", ID: "1", Doc: doc}}, defaultMaxRequestBytes); err != nil {
			t.Errorf("index %q must be accepted, got %v", name, err)
		}
	}
}

func TestBuildBulkRejectsMissingID(t *testing.T) {
	_, err := buildBulk("i", []bulkOp{{Op: "create", ID: "  ", Doc: json.RawMessage(`{}`)}}, defaultMaxRequestBytes)
	if err == nil || !strings.Contains(err.Error(), "id is required") {
		t.Fatalf("want missing id error, got %v", err)
	}
}

func TestBuildBulkRejectsMissingDoc(t *testing.T) {
	_, err := buildBulk("i", []bulkOp{{Op: "create", ID: "1", Doc: nil}}, defaultMaxRequestBytes)
	if err == nil || !strings.Contains(err.Error(), "doc is required") {
		t.Fatalf("want missing doc error, got %v", err)
	}
}

func TestBuildBulkRejectsInvalidDocJSON(t *testing.T) {
	_, err := buildBulk("i", []bulkOp{{Op: "create", ID: "1", Doc: json.RawMessage(`{"a":}`)}}, defaultMaxRequestBytes)
	if err == nil || !strings.Contains(err.Error(), "invalid doc") {
		t.Fatalf("want invalid doc error, got %v", err)
	}
}

func TestBuildBulkRejectsEmptyOperations(t *testing.T) {
	if _, err := buildBulk("i", nil, defaultMaxRequestBytes); err == nil || !strings.Contains(err.Error(), "non-empty") {
		t.Fatalf("want non-empty operations error, got %v", err)
	}
}

func TestBuildBulkRejectsOversizeBody(t *testing.T) {
	// A valid JSON doc, padded to ~512 bytes each.
	doc := json.RawMessage(`{"pad":"` + strings.Repeat("x", 500) + `"}`)
	ops := make([]bulkOp, 0, 20)
	for i := 0; i < 20; i++ {
		ops = append(ops, bulkOp{Op: "index", ID: "1", Doc: doc})
	}
	// Body is ~20*512 + overhead ≈ 11 KiB; a 1 KiB cap must trip.
	if _, err := buildBulk("i", ops, 1024); err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("want oversize error, got %v", err)
	}
}

func TestBuildBulkZeroLimitDisablesCap(t *testing.T) {
	doc := json.RawMessage(`{"pad":"` + strings.Repeat("x", 500) + `"}`)
	ops := make([]bulkOp, 0, 8)
	for i := 0; i < 8; i++ {
		ops = append(ops, bulkOp{Op: "index", ID: "1", Doc: doc})
	}
	if _, err := buildBulk("i", ops, 0); err != nil {
		t.Fatalf("zero limit must disable the cap, got %v", err)
	}
}

func TestBuildAuthHeaderAPIKey(t *testing.T) {
	got, err := buildAuthHeader(authConfig{Type: "apiKey", APIKey: "secret-key"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "ApiKey secret-key" {
		t.Fatalf("auth header = %q", got)
	}
}

func TestBuildAuthHeaderAPIKeyRequiresValue(t *testing.T) {
	if _, err := buildAuthHeader(authConfig{Type: "apiKey"}); err == nil || !strings.Contains(err.Error(), "apiKey") {
		t.Fatalf("want apiKey-required error, got %v", err)
	}
}

func TestBuildAuthHeaderBasic(t *testing.T) {
	got, err := buildAuthHeader(authConfig{Type: "basic", Username: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	// base64("u:p") = "dTpw"
	if got != "Basic dTpw" {
		t.Fatalf("auth header = %q", got)
	}
}

func TestBuildAuthHeaderBasicRequiresUsername(t *testing.T) {
	if _, err := buildAuthHeader(authConfig{Type: "basic", Password: "p"}); err == nil || !strings.Contains(err.Error(), "username") {
		t.Fatalf("want username-required error, got %v", err)
	}
}

func TestBuildAuthHeaderRejectsUnknownType(t *testing.T) {
	if _, err := buildAuthHeader(authConfig{Type: "oauth"}); err == nil || !strings.Contains(err.Error(), "unsupported auth type") {
		t.Fatalf("want unsupported auth type error, got %v", err)
	}
}

func TestBuildAuthHeaderEmptyMeansNone(t *testing.T) {
	got, err := buildAuthHeader(authConfig{})
	if err != nil || got != "" {
		t.Fatalf("no auth type = no header, got %q err=%v", got, err)
	}
}

func TestNewESClientValidatesURL(t *testing.T) {
	for _, server := range []string{"", "ftp://x", "http://", "notaurl"} {
		if _, err := newESClient(clientConfig{Server: server}); err == nil {
			t.Errorf("server %q must be rejected", server)
		}
	}
}

func TestNewESClientNormalizesTrailingSlash(t *testing.T) {
	c, err := newESClient(clientConfig{Server: "https://es.internal:9200/"})
	if err != nil {
		t.Fatal(err)
	}
	if c.baseURL != "https://es.internal:9200" {
		t.Fatalf("baseURL = %q", c.baseURL)
	}
}

func TestNewESClientRejectsBadCAFile(t *testing.T) {
	if _, err := newESClient(clientConfig{Server: "https://es.internal:9200", TLS: tlsConfig{CAFile: "does-not-exist.pem"}}); err == nil || !strings.Contains(err.Error(), "read caFile") {
		t.Fatalf("want caFile read error, got %v", err)
	}
}

func TestNormalizeClientConfigDefaults(t *testing.T) {
	var cfg clientConfig
	if err := normalizeClientConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.TimeoutMs != defaultTimeoutMs {
		t.Fatalf("timeoutMs default = %d", cfg.TimeoutMs)
	}
	if cfg.MaxRequestBytes != defaultMaxRequestBytes {
		t.Fatalf("maxRequestBytes default = %d", cfg.MaxRequestBytes)
	}
	if cfg.MaxOperations != defaultMaxOperations {
		t.Fatalf("maxOperations default = %d", cfg.MaxOperations)
	}
}

func TestNormalizeClientConfigRejectsOversizeCap(t *testing.T) {
	cfg := clientConfig{MaxRequestBytes: maxBodyBytes + 1}
	if err := normalizeClientConfig(&cfg); err == nil || !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("want ceiling error, got %v", err)
	}
}
