package main

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
)

// All bulk-response fixtures follow the real ES 9.x _bulk wire format.
// Request-level error envelopes (400/404) carry {"error":{...},"status":N}
// with no "items" array; per-item results are keyed by the op name.

func TestParseBulkResponseAllAccepted(t *testing.T) {
	body := `{"took":5,"errors":false,"items":[` +
		`{"create":{"_index":"i","_id":"1","_version":1,"result":"created","status":201}},` +
		`{"index":{"_index":"i","_id":"2","_version":2,"result":"updated","status":200}}]}`
	outcome := parseBulkResponse(200, []byte(body), 2)
	if outcome.Overall != classificationOK {
		t.Fatalf("overall = %q, want ok", outcome.Overall)
	}
	if len(outcome.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(outcome.Items))
	}
	if outcome.Items[0].Class != classificationOK || outcome.Items[0].Status != 201 || outcome.Items[0].Op != "create" {
		t.Fatalf("item0 = %+v", outcome.Items[0])
	}
	if outcome.Items[1].Class != classificationOK || outcome.Items[1].Status != 200 || outcome.Items[1].Op != "index" {
		t.Fatalf("item1 = %+v", outcome.Items[1])
	}
}

func TestParseBulkResponse409OnCreateIsDuplicate(t *testing.T) {
	body := `{"took":3,"errors":true,"items":[
{"create":{"_index":"i","_id":"1","_version":1,"result":"created","status":201}},
{"create":{"_index":"i","_id":"2","status":409,"error":{"type":"version_conflict_engine_exception","reason":"[2]: version conflict"}}}]}`
	outcome := parseBulkResponse(200, []byte(body), 2)
	if outcome.Overall != classificationDuplicate {
		t.Fatalf("overall = %q, want duplicate", outcome.Overall)
	}
	if outcome.Items[1].Class != classificationDuplicate {
		t.Fatalf("item1 class = %q, want duplicate", outcome.Items[1].Class)
	}
	if outcome.Items[1].ErrorType != "version_conflict_engine_exception" {
		t.Fatalf("item1 errorType = %q", outcome.Items[1].ErrorType)
	}
}

func TestParseBulkResponse409OnIndexIsTerminal(t *testing.T) {
	body := `{"took":2,"errors":true,"items":[{"index":{"_index":"i","_id":"1","status":409,"error":{"type":"illegal_argument_exception","reason":"index [i] [1] already exists"}}}]}`
	outcome := parseBulkResponse(200, []byte(body), 1)
	if outcome.Overall != classificationTerminal {
		t.Fatalf("overall = %q, want terminal (409 on index op)", outcome.Overall)
	}
}

func TestParseBulkResponseItem400IsTerminal(t *testing.T) {
	body := `{"took":2,"errors":true,"items":[{"create":{"_index":"i","_id":"1","status":400,"error":{"type":"mapper_parsing_exception","reason":"failed to parse field [chat_id] of type [long]"}}}]}`
	outcome := parseBulkResponse(200, []byte(body), 1)
	if outcome.Overall != classificationTerminal {
		t.Fatalf("overall = %q, want terminal (item 400)", outcome.Overall)
	}
	if outcome.Items[0].ErrorType != "mapper_parsing_exception" {
		t.Fatalf("errorType = %q", outcome.Items[0].ErrorType)
	}
}

func TestParseBulkResponseItem401IsAuth(t *testing.T) {
	body := `{"took":2,"errors":true,"items":[{"index":{"_index":"i","_id":"1","status":401,"error":{"type":"security_exception","reason":"unable to authenticate user [svc]"}}}]}`
	outcome := parseBulkResponse(200, []byte(body), 1)
	if outcome.Overall != classificationAuth {
		t.Fatalf("overall = %q, want auth (item 401)", outcome.Overall)
	}
}

func TestParseBulkResponseItem429IsRetryable(t *testing.T) {
	body := `{"took":2,"errors":true,"items":[{"index":{"_index":"i","_id":"1","status":429,"error":{"type":"es_rejected_execution_exception","reason":"[i][0] rejected execution (queue full)"}}}]}`
	outcome := parseBulkResponse(200, []byte(body), 1)
	if outcome.Overall != classificationRetryable {
		t.Fatalf("overall = %q, want retryable (item 429)", outcome.Overall)
	}
}

func TestParseBulkResponseItem5xxIsRetryable(t *testing.T) {
	for _, status := range []int{500, 503} {
		body := `{"took":2,"errors":true,"items":[{"index":{"_index":"i","_id":"1","status":` + strconv.Itoa(status) + `,"error":{"type":"internal_server_error","reason":"boom"}}}]}`
		outcome := parseBulkResponse(200, []byte(body), 1)
		if outcome.Overall != classificationRetryable {
			t.Fatalf("status %d: overall = %q, want retryable", status, outcome.Overall)
		}
	}
}

func TestParseBulkResponseMixedTakesWorstBucket(t *testing.T) {
	body := `{"took":9,"errors":true,"items":[
{"create":{"_index":"i","_id":"1","_version":1,"result":"created","status":201}},
{"index":{"_index":"i","_id":"2","status":429,"error":{"type":"es_rejected_execution_exception","reason":"rejected"}}},
{"index":{"_index":"i","_id":"3","status":400,"error":{"type":"mapper_parsing_exception","reason":"bad field"}}},
{"index":{"_index":"i","_id":"4","status":401,"error":{"type":"security_exception","reason":"no access"}}}]}`
	outcome := parseBulkResponse(200, []byte(body), 4)
	if outcome.Overall != classificationAuth {
		t.Fatalf("overall = %q, want auth (worst of ok/retryable/terminal/auth)", outcome.Overall)
	}
	classes := []string{outcome.Items[0].Class, outcome.Items[1].Class, outcome.Items[2].Class, outcome.Items[3].Class}
	if classes[0] != classificationOK || classes[1] != classificationRetryable || classes[2] != classificationTerminal || classes[3] != classificationAuth {
		t.Fatalf("item classes = %v", classes)
	}
}

func TestParseBulkResponseItem404IsTerminal(t *testing.T) {
	body := `{"took":2,"errors":true,"items":[{"create":{"_index":"i","_id":"1","status":404,"error":{"type":"index_not_found_exception","reason":"no such index [i]"}}}]}`
	outcome := parseBulkResponse(200, []byte(body), 1)
	if outcome.Overall != classificationTerminal {
		t.Fatalf("overall = %q, want terminal (item 404)", outcome.Overall)
	}
}

func TestParseBulkResponseMalformedItemIsTerminal(t *testing.T) {
	// A corrupt item: valid JSON array but the second element is not a
	// single-op-key bulk item (unknown op key "weird").
	body := `{"took":2,"errors":true,"items":[{"create":{"_index":"i","_id":"1","status":201}},{"weird":{"status":201}}]}`
	outcome := parseBulkResponse(200, []byte(body), 2)
	if outcome.Overall != classificationTerminal {
		t.Fatalf("overall = %q, want terminal (malformed item)", outcome.Overall)
	}
	if outcome.Items[1].Class != classificationTerminal {
		t.Fatalf("item1 class = %q, want terminal", outcome.Items[1].Class)
	}
}

func TestParseBulkResponseUnknownOpKeyIsTerminal(t *testing.T) {
	// Item keyed by an op we do not support: malformed per the contract.
	body := `{"took":2,"errors":true,"items":[{"update":{"_index":"i","_id":"1","status":200}}]}`
	outcome := parseBulkResponse(200, []byte(body), 1)
	if outcome.Overall != classificationTerminal {
		t.Fatalf("overall = %q, want terminal (unknown op key)", outcome.Overall)
	}
}

func TestParseBulkResponseMalformedJSONIsTerminal(t *testing.T) {
	outcome := parseBulkResponse(200, []byte(`{"took":5,"items":`), 1)
	if outcome.Overall != classificationTerminal {
		t.Fatalf("overall = %q, want terminal (malformed JSON)", outcome.Overall)
	}
}

func TestParseBulkResponseNonBulk2xxIsTerminal(t *testing.T) {
	body := `{"error":{"type":"parsing_exception","reason":"request body is malformed"},"status":200}`
	outcome := parseBulkResponse(200, []byte(body), 1)
	if outcome.Overall != classificationTerminal {
		t.Fatalf("overall = %q, want terminal (non-bulk 2xx)", outcome.Overall)
	}
}

func TestParseBulkResponseItemCountMismatchIsTerminal(t *testing.T) {
	body := `{"took":2,"errors":false,"items":[{"create":{"_index":"i","_id":"1","status":201}}]}`
	outcome := parseBulkResponse(200, []byte(body), 2)
	if outcome.Overall != classificationTerminal {
		t.Fatalf("overall = %q, want terminal (1 item for 2 ops)", outcome.Overall)
	}
}

func TestParseBulkResponseTopLevel401IsAuth(t *testing.T) {
	body := `{"error":{"type":"security_exception","reason":"unable to authenticate user [svc]"},"status":401}`
	outcome := parseBulkResponse(401, []byte(body), 1)
	if outcome.Overall != classificationAuth {
		t.Fatalf("overall = %q, want auth", outcome.Overall)
	}
}

func TestParseBulkResponseTopLevel403IsAuth(t *testing.T) {
	body := `{"error":{"type":"security_exception","reason":"no permission for [indices:data/write/index]"},"status":403}`
	outcome := parseBulkResponse(403, []byte(body), 1)
	if outcome.Overall != classificationAuth {
		t.Fatalf("overall = %q, want auth", outcome.Overall)
	}
}

func TestParseBulkResponseTopLevel429IsRetryable(t *testing.T) {
	body := `{"error":{"type":"es_rejected_execution_exception","reason":"rejected execution"},"status":429}`
	outcome := parseBulkResponse(429, []byte(body), 1)
	if outcome.Overall != classificationRetryable {
		t.Fatalf("overall = %q, want retryable", outcome.Overall)
	}
}

func TestParseBulkResponseTopLevel408IsRetryable(t *testing.T) {
	body := `{"error":{"type":"service_unavailable","reason":"request timed out"},"status":408}`
	outcome := parseBulkResponse(408, []byte(body), 1)
	if outcome.Overall != classificationRetryable {
		t.Fatalf("overall = %q, want retryable", outcome.Overall)
	}
}

func TestParseBulkResponseTopLevel5xxIsRetryable(t *testing.T) {
	for _, status := range []int{500, 502, 503} {
		body := `{"error":{"type":"internal_server_error","reason":"boom"},"status":` + strconv.Itoa(status) + `}`
		outcome := parseBulkResponse(status, []byte(body), 1)
		if outcome.Overall != classificationRetryable {
			t.Fatalf("status %d: overall = %q, want retryable", status, outcome.Overall)
		}
	}
}

// Spec-gap handling (see M3 report): design.md's request-level table does not
// list 404 explicitly. Real ES returns index_not_found_exception with the
// same no-items envelope as the 400 parsing case, so it is classified
// terminal (same bucket as "4xx non-bulk"), not retried.
func TestParseBulkResponseTopLevel404IndexNotFoundIsTerminal(t *testing.T) {
	body := `{"error":{"type":"index_not_found_exception","reason":"no such index [i]"},"status":404}`
	outcome := parseBulkResponse(404, []byte(body), 1)
	if outcome.Overall != classificationTerminal {
		t.Fatalf("overall = %q, want terminal (top-level 404 index_not_found)", outcome.Overall)
	}
	if !strings.Contains(outcome.RequestError, "index_not_found_exception") {
		t.Fatalf("requestError = %q, want index_not_found_exception", outcome.RequestError)
	}
}

func TestParseBulkResponseTopLevel400ParsingExceptionIsTerminal(t *testing.T) {
	body := `{"error":{"type":"parsing_exception","reason":"Expected one of START_OBJECT, START_ARRAY"},"status":400}`
	outcome := parseBulkResponse(400, []byte(body), 1)
	if outcome.Overall != classificationTerminal {
		t.Fatalf("overall = %q, want terminal (top-level 400 parsing_exception)", outcome.Overall)
	}
	if !strings.Contains(outcome.RequestError, "parsing_exception") {
		t.Fatalf("requestError = %q", outcome.RequestError)
	}
}

type fakeNetError struct{}

func (fakeNetError) Error() string   { return "connection refused" }
func (fakeNetError) Timeout() bool   { return true }
func (fakeNetError) Temporary() bool { return true }

func TestClassifyTransportErrorNetworkIsRetryable(t *testing.T) {
	class, reason := classifyTransportError(fakeNetError{})
	if class != classificationRetryable {
		t.Fatalf("class = %q, want retryable", class)
	}
	if reason == "" {
		t.Fatal("reason must not be empty")
	}
}

func TestClassifyTransportErrorDeadlineIsRetryable(t *testing.T) {
	class, _ := classifyTransportError(context.DeadlineExceeded)
	if class != classificationRetryable {
		t.Fatalf("class = %q, want retryable", class)
	}
}

func TestClassifyTransportErrorCanceledIsRetryable(t *testing.T) {
	class, _ := classifyTransportError(context.Canceled)
	if class != classificationRetryable {
		t.Fatalf("class = %q, want retryable", class)
	}
}

func TestClassifyTransportErrorWrappedContext(t *testing.T) {
	wrapped := fmt.Errorf("post bulk: %w", context.DeadlineExceeded)
	class, _ := classifyTransportError(wrapped)
	if class != classificationRetryable {
		t.Fatalf("class = %q, want retryable (wrapped)", class)
	}
	var _ net.Error = fakeNetError{}
}
