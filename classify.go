package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
)

// Outcome buckets, ordered by severity. The rollup keeps the worst bucket
// present in a request: auth > terminal > retryable > duplicate > ok.
const (
	classificationOK        = "ok"
	classificationDuplicate = "duplicate"
	classificationRetryable = "retryable"
	classificationTerminal  = "terminal"
	classificationAuth      = "auth"
)

// severity orders buckets so the rollup can pick the worst. Higher is worse.
var severity = map[string]int{
	classificationOK:        0,
	classificationDuplicate: 1,
	classificationRetryable: 2,
	classificationTerminal:  3,
	classificationAuth:      4,
}

// itemResult is the per-item outcome as reported downstream.
type itemResult struct {
	Status    int    `json:"status"`
	Op        string `json:"op"`
	Class     string `json:"class"`
	ErrorType string `json:"errorType,omitempty"`
}

// bulkOutcome is the full parsed + classified result of one bulk request.
type bulkOutcome struct {
	// Overall is the rollup: the worst bucket across request and items.
	// classificationOK when every item is ok (classificationDuplicate when a
	// duplicate is present but nothing worse).
	Overall string
	// Items is the per-item classification, aligned 1:1 with the ops sent.
	Items []itemResult
	// RequestError carries the top-level ES error reason for non-bulk
	// responses (e.g. parsing_exception, index_not_found_exception).
	RequestError string
}

// esError is the common envelope for top-level ES errors:
// {"error":{"type":"...","reason":"..."},"status":400}
type esError struct {
	Error struct {
		Type   string `json:"type"`
		Reason string `json:"reason"`
	} `json:"error"`
	Status int `json:"status"`
}

// perItemError mirrors the per-item error object: {"type":"...","reason":"..."}.
type perItemError struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

// classifyTransportError classifies a transport-level (no response) failure.
// Network errors, timeouts, and context cancels are all retryable: the
// request may or may not have reached ES, and ES writes are idempotent-safe
// via the caller-derived _id, so a graph-level retry is safe.
func classifyTransportError(err error) (string, string) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return classificationRetryable, "request deadline exceeded or cancelled"
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return classificationRetryable, fmt.Sprintf("network error: %v", err)
	}
	return classificationRetryable, fmt.Sprintf("transport error: %v", err)
}

// parseBulkResponse classifies an HTTP response from a _bulk POST.
// expectedItems is the number of operations sent; a mismatch means the
// response is truncated or malformed and classifies as terminal.
//
// Request-level table (see design.md):
//
//	HTTP 401/403                -> auth
//	HTTP 408/429/5xx            -> retryable
//	HTTP 400/404 (no items)     -> terminal
//	HTTP 2xx, errors:false      -> ok
//	HTTP 2xx, errors:true       -> worst per-item bucket
//
// A non-2xx response without an items array is a top-level ES error envelope
// (parsing_exception, index_not_found_exception, ...). The 4xx case is
// terminal: the request shape or target index is wrong and retrying the same
// bytes cannot succeed. This includes request-level 404
// (index_not_found_exception), which the design.md request table does not
// list explicitly; it is grouped with the 400 non-bulk row (4xx non-bulk ->
// terminal, do not retry).
func parseBulkResponse(status int, body []byte, expectedItems int) *bulkOutcome {
	outcome := &bulkOutcome{Overall: classificationOK}

	switch {
	case status == 401 || status == 403:
		outcome.Overall = classificationAuth
		outcome.RequestError = summarize(body)
		return outcome
	case status == 408 || status == 429 || status >= 500:
		outcome.Overall = classificationRetryable
		outcome.RequestError = summarize(body)
		return outcome
	case status == 400 || status == 404:
		// Non-bulk 4xx: bad request shape or missing index. Terminal.
		outcome.Overall = classificationTerminal
		outcome.RequestError = summarize(body)
		return outcome
	}

	// Anything not matched above (2xx, 3xx, other 4xx) must be bulk-shaped to
	// be interpreted per item; otherwise it is a malformed/top-level error.
	if !isBulkShaped(body) {
		outcome.Overall = classificationTerminal
		if status >= 200 && status < 300 {
			outcome.RequestError = "malformed bulk response (not bulk-shaped)"
		} else {
			outcome.RequestError = summarize(body)
		}
		return outcome
	}

	var resp struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		outcome.Overall = classificationTerminal
		outcome.RequestError = "malformed bulk response: " + err.Error()
		return outcome
	}
	if len(resp.Items) == 0 {
		outcome.Overall = classificationTerminal
		outcome.RequestError = "bulk response has no items"
		return outcome
	}
	if len(resp.Items) != expectedItems {
		outcome.Overall = classificationTerminal
		outcome.RequestError = fmt.Sprintf("bulk response item count %d does not match %d operations", len(resp.Items), expectedItems)
		return outcome
	}

	for _, raw := range resp.Items {
		item := classifyItem(raw)
		outcome.Items = append(outcome.Items, item)
		if severity[item.Class] > severity[outcome.Overall] {
			outcome.Overall = item.Class
		}
	}
	return outcome
}

// isBulkShaped reports whether body parses as an object carrying a non-null
// "items" array — the wire signal that separates bulk results from the
// top-level ES error envelope.
func isBulkShaped(body []byte) bool {
	var probe struct {
		Items json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	if len(probe.Items) == 0 || strings.TrimSpace(string(probe.Items)) == "null" {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(string(probe.Items)), "[")
}

// classifyItem classifies one per-item result by op and status. A well-formed
// item is an object with exactly one key, the op name (create/index).
//
// Per-item table (see design.md):
//
//	200/201             -> ok
//	409 on create       -> duplicate (idempotent path)
//	409 on index        -> terminal (conflict is unexpected for index)
//	400/404             -> terminal
//	401/403             -> auth
//	429/5xx             -> retryable
//	missing / malformed -> terminal
func classifyItem(raw json.RawMessage) itemResult {
	// Malformed/missing item defaults to terminal.
	item := itemResult{Status: 0, Op: "", Class: classificationTerminal}

	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return item
	}
	if len(wrapper) != 1 {
		return item // a bulk item carries exactly one op key
	}
	for op, payload := range wrapper {
		if op != opCreate && op != opIndex {
			return item // malformed: unknown op key
		}
		var detail struct {
			Status int           `json:"status"`
			Error  *perItemError `json:"error"`
		}
		if err := json.Unmarshal(payload, &detail); err != nil {
			return item
		}
		item.Op = op
		item.Status = detail.Status
		if detail.Error != nil {
			item.ErrorType = detail.Error.Type
		}
		item.Class = classifyItemStatus(op, detail.Status)
	}
	return item
}

func classifyItemStatus(op string, status int) string {
	switch {
	case status == 200 || status == 201:
		return classificationOK
	case status == 409:
		if op == opCreate {
			return classificationDuplicate
		}
		return classificationTerminal
	case status == 400 || status == 404:
		return classificationTerminal
	case status == 401 || status == 403:
		return classificationAuth
	case status == 429 || status >= 500:
		return classificationRetryable
	default:
		return classificationTerminal
	}
}

// summarize extracts a short reason from a top-level ES error envelope,
// falling back to a clipped raw body when the shape is unparseable. It never
// returns document bodies (top-level ES errors carry only type/reason).
func summarize(body []byte) string {
	var env esError
	if err := json.Unmarshal(body, &env); err == nil && (env.Error.Type != "" || env.Error.Reason != "") {
		if env.Error.Reason != "" {
			return env.Error.Type + ": " + env.Error.Reason
		}
		return env.Error.Type
	}
	const max = 256
	s := string(body)
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}
