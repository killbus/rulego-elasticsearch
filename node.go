package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/rulego/rulego/api/types"
	"github.com/rulego/rulego/components/base"
	"github.com/rulego/rulego/utils/maps"
)

const (
	componentType    = "elasticsearchWrite"
	componentVersion = "0.1.0"
)

// elasticsearchWriteNode owns exactly one capability: send a bounded batch
// of write operations to one named Elasticsearch client and report the
// per-item outcome. Retry/abort is graph policy; the node never loops.
type elasticsearchWriteNode struct {
	base.SharedNode[*esClient]
	Config clientConfig
}

func (*elasticsearchWriteNode) Type() string { return componentType }

// New returns a node prototype whose Config carries the real built-in
// defaults. These values feed the editor form's DefaultValue so the UI
// shows 30000/31457280/500 instead of 0; they do NOT change wire behavior,
// because Map2Struct (ZeroFields) resets the struct before decoding in
// Init and normalizeClientConfig re-applies the same defaults.
func (*elasticsearchWriteNode) New() types.Node {
	return &elasticsearchWriteNode{Config: clientConfig{
		TimeoutMs:       defaultTimeoutMs,
		MaxRequestBytes: defaultMaxRequestBytes,
		MaxOperations:   defaultMaxOperations,
	}}
}

func (*elasticsearchWriteNode) Def() types.ComponentForm {
	relations := []string{types.Success, types.Failure}
	return types.ComponentForm{
		Type:          componentType,
		Category:      "external",
		Label:         "Elasticsearch Write",
		Desc:          "Write a bounded batch of create/index operations to Elasticsearch via _bulk",
		Version:       componentVersion,
		ComponentKind: types.ComponentKindNative,
		RelationTypes: &relations,
	}
}

func (n *elasticsearchWriteNode) Init(ruleConfig types.Config, configuration types.Configuration) error {
	if err := maps.Map2Struct(configuration, &n.Config); err != nil {
		return errors.New("elasticsearchWrite: invalid configuration")
	}
	if strings.TrimSpace(n.Config.Server) == "" {
		return errors.New("elasticsearchWrite: server is required")
	}
	// normalizeClientConfig validates an inline owner policy without parsing
	// the URL, so it is also safe for ref:// borrowers (whose config is
	// usually just {"server":"ref://es"}; stray policy on a borrower is
	// rejected rather than silently ignored).
	if err := normalizeClientConfig(&n.Config); err != nil {
		return fmt.Errorf("elasticsearchWrite: %w", err)
	}
	// Soft-fail startup: an ES outage at boot must not kill the RuleGo
	// process. newESClient performs no network I/O, so in practice only
	// configuration errors reach the Reconnecting path.
	if err := n.SharedNode.InitWithCloseSoftFail(ruleConfig, n.Type(), n.Config.Server, true,
		func() (*esClient, error) { return newESClient(n.Config) },
		func(c *esClient) error { return c.Close() }); err != nil {
		return err
	}
	n.SharedNode.BindChain(configuration)
	return nil
}

func (n *elasticsearchWriteNode) Destroy() { _ = n.SharedNode.Close() }

// OnMsg executes the write contract (design.md):
//
//	size guard -> strict decode -> op-count guard -> build bulk ->
//	owner lookup (retryable on failure) -> Bulk with owner timeout ->
//	classify -> Success | Failure (structured diagnosis)
func (n *elasticsearchWriteNode) OnMsg(ruleContext types.RuleContext, msg types.RuleMsg) {
	data := []byte(msg.GetData())

	if n.Config.MaxRequestBytes > 0 && int64(len(data)) > n.Config.MaxRequestBytes {
		tellWriteFailure(ruleContext, msg, classificationTerminal,
			fmt.Sprintf("request %d bytes exceeds maxRequestBytes %d", len(data), n.Config.MaxRequestBytes), nil)
		return
	}

	req, err := decodeWriteRequest(data)
	if err != nil {
		// Terminal: resending the same bytes cannot fix the shape.
		tellWriteFailure(ruleContext, msg, classificationTerminal, "invalid request: "+err.Error(), nil)
		return
	}

	if len(req.Operations) > n.Config.MaxOperations {
		tellWriteFailure(ruleContext, msg, classificationTerminal,
			fmt.Sprintf("%d operations exceeds maxOperations %d", len(req.Operations), n.Config.MaxOperations), nil)
		return
	}

	body, err := buildBulk(req.Index, req.Operations, n.Config.MaxRequestBytes)
	if err != nil {
		tellWriteFailure(ruleContext, msg, classificationTerminal, err.Error(), nil)
		return
	}

	client, err := n.SharedNode.GetSafely()
	if err != nil {
		// The owner may be mid-reconnect; classification stays retryable.
		class, reason := classifyTransportError(err)
		tellWriteFailure(ruleContext, msg, class, "owner unavailable: "+reason, nil)
		return
	}

	ctx := ruleContext.GetContext()
	if ctx == nil {
		ctx = context.Background()
	}
	// client.timeout (not n.Config.TimeoutMs): a ref:// borrower inherits the
	// owner's deadline, which is the owner's policy.
	ctx, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()

	status, respBody, err := client.Bulk(ctx, req.Index, body)
	if err != nil {
		class, reason := classifyTransportError(err)
		tellWriteFailure(ruleContext, msg, class, reason, nil)
		return
	}

	outcome := parseBulkResponse(status, respBody, len(req.Operations))
	switch outcome.Overall {
	case classificationOK, classificationDuplicate:
		tellWriteSuccess(ruleContext, msg, outcome)
	default:
		tellWriteFailure(ruleContext, msg, outcome.Overall,
			failureReason(req.Index, outcome), outcome.Items)
	}
}

// failureReason picks the worst-bucket reason: the first per-item failure in
// that bucket (stable: wire order), or the top-level ES error summary. Raw
// doc bodies are never echoed.
func failureReason(index string, outcome *bulkOutcome) string {
	for i, item := range outcome.Items {
		if item.Class == outcome.Overall {
			return fmt.Sprintf("bulk item status %d on index %s op #%d", item.Status, index, i+1)
		}
	}
	return outcome.RequestError
}

// writeSummary is the Success message contract (design.md).
type writeSummary struct {
	Accepted   int          `json:"accepted"`
	Duplicates int          `json:"duplicates"`
	Total      int          `json:"total"`
	Results    []itemResult `json:"results"`
}

func tellWriteSuccess(ctx types.RuleContext, input types.RuleMsg, outcome *bulkOutcome) {
	summary := writeSummary{Total: len(outcome.Items), Results: outcome.Items}
	for _, item := range outcome.Items {
		switch item.Class {
		case classificationOK:
			summary.Accepted++
		case classificationDuplicate:
			summary.Duplicates++
		}
	}
	payload, _ := json.Marshal(summary)
	output := input.Copy()
	output.DataType = types.JSON
	output.SetBytes(payload)
	ctx.TellSuccess(output)
}

// writeFailure is the Error message contract (design.md).
type writeFailure struct {
	Classification string       `json:"classification"`
	Reason         string       `json:"reason"`
	Results        []itemResult `json:"results"`
}

func tellWriteFailure(ctx types.RuleContext, input types.RuleMsg, classification, reason string, items []itemResult) {
	if items == nil {
		items = []itemResult{} // wire "results" as [], never null
	}
	payload, _ := json.Marshal(writeFailure{Classification: classification, Reason: reason, Results: items})
	output := input.Copy()
	output.DataType = types.JSON
	output.SetBytes(payload)
	ctx.TellFailure(output, errors.New(reason))
}

// decodeWriteRequest strictly decodes the message contract. Strict means:
// duplicate keys rejected at every object level, unknown fields rejected at
// the message and per-operation level, trailing data rejected. The per-op
// doc is opaque: it must be a single JSON value (validated by json.Compact
// in buildBulk); its internal keys are the caller's business.
func decodeWriteRequest(data []byte) (*writeRequest, error) {
	p := newStrictJSONParser(data)
	if err := p.parseWriteRequest(); err != nil {
		return nil, err
	}
	return p.result, nil
}

// strictJSONParser is a minimal hand-rolled JSON scanner for the message
// contract. encoding/json silently keeps the LAST value for duplicate keys
// and cannot flag them; a token scan is the only way to reject duplicates
// while keeping doc values fully opaque (no reflection into doc contents).
type strictJSONParser struct {
	data   []byte
	pos    int
	result *writeRequest
}

func newStrictJSONParser(data []byte) *strictJSONParser {
	return &strictJSONParser{data: data}
}

// parseWriteRequest parses exactly one top-level object carrying the keys
// "index" (string) and "operations" (array of op objects), in any order.
func (p *strictJSONParser) parseWriteRequest() error {
	p.skipWS()
	if p.pos >= len(p.data) {
		return errors.New("empty request body")
	}
	if p.data[p.pos] != '{' {
		return errors.New("request must be a JSON object")
	}
	p.pos++

	var index string
	var indexSeen, opsSeen bool
	for {
		p.skipWS()
		if p.pos >= len(p.data) {
			return errors.New("unterminated request object")
		}
		if p.data[p.pos] == '}' {
			p.pos++
			break
		}
		key, err := p.parseKey()
		if err != nil {
			return err
		}
		switch key {
		case "index":
			if indexSeen {
				return errors.New("duplicate key \"index\"")
			}
			p.expectColon()
			val, err := p.parseString()
			if err != nil {
				return fmt.Errorf("index: %w", err)
			}
			if strings.TrimSpace(val) == "" {
				return errors.New("index is required")
			}
			index, indexSeen = val, true
		case "operations":
			if opsSeen {
				return errors.New("duplicate key \"operations\"")
			}
			p.expectColon()
			ops, err := p.parseOps()
			if err != nil {
				return err
			}
			if p.result == nil {
				p.result = &writeRequest{}
			}
			p.result.Operations = ops
			opsSeen = true
		default:
			// Unknown keys are rejected, not silently dropped.
			if _, err := p.captureValue(); err != nil {
				return err
			}
			return fmt.Errorf("unknown field %q", key)
		}
		p.skipWS()
		if p.pos >= len(p.data) {
			return errors.New("unterminated request object")
		}
		if p.data[p.pos] == ',' {
			p.pos++
			continue
		}
	}
	if !indexSeen || !opsSeen {
		return errors.New("index and operations are required")
	}
	if p.result == nil || len(p.result.Operations) == 0 {
		return errors.New("operations must be non-empty")
	}
	p.result.Index = index
	p.skipWS()
	if p.pos != len(p.data) {
		return errors.New("trailing data after request object")
	}
	return nil
}

// parseOps parses the operations array. Each op is an object with exactly
// the keys "op" (string), "id" (string), "doc" (any JSON value, opaque).
func (p *strictJSONParser) parseOps() ([]bulkOp, error) {
	p.skipWS()
	if p.pos >= len(p.data) || p.data[p.pos] != '[' {
		return nil, errors.New("operations must be an array")
	}
	p.pos++

	var ops []bulkOp
	for {
		p.skipWS()
		if p.pos >= len(p.data) {
			return nil, errors.New("unterminated operations array")
		}
		if p.data[p.pos] == ']' {
			p.pos++
			return ops, nil
		}
		op, err := p.parseOp()
		if err != nil {
			return nil, fmt.Errorf("operation %d: %w", len(ops)+1, err)
		}
		ops = append(ops, op)
		p.skipWS()
		if p.pos >= len(p.data) {
			return nil, errors.New("unterminated operations array")
		}
		if p.data[p.pos] == ',' {
			p.pos++
			continue
		}
	}
}

// parseOp parses one op object, rejecting duplicate keys and unknown
// fields. The doc value is captured raw (opaque).
func (p *strictJSONParser) parseOp() (bulkOp, error) {
	p.skipWS()
	if p.pos >= len(p.data) || p.data[p.pos] != '{' {
		return bulkOp{}, errors.New("operation must be an object")
	}
	p.pos++

	var op bulkOp
	seen := map[string]bool{}
	for {
		p.skipWS()
		if p.pos >= len(p.data) {
			return bulkOp{}, errors.New("unterminated operation object")
		}
		if p.data[p.pos] == '}' {
			p.pos++
			break
		}
		key, err := p.parseKey()
		if err != nil {
			return bulkOp{}, err
		}
		if seen[key] {
			return bulkOp{}, fmt.Errorf("duplicate key %q", key)
		}
		// The key's colon must be consumed before the value; the top-level
		// object parse does the same per case.
		p.expectColon()
		switch key {
		case "op":
			val, err := p.parseString()
			if err != nil {
				return bulkOp{}, fmt.Errorf("op: %w", err)
			}
			op.Op = val
		case "id":
			val, err := p.parseString()
			if err != nil {
				return bulkOp{}, fmt.Errorf("id: %w", err)
			}
			op.ID = val
		case "doc":
			raw, err := p.captureValue()
			if err != nil {
				return bulkOp{}, fmt.Errorf("doc: %w", err)
			}
			op.Doc = raw
		default:
			// Unknown fields are rejected, not silently dropped.
			if _, err := p.captureValue(); err != nil {
				return bulkOp{}, err
			}
			return bulkOp{}, fmt.Errorf("unknown field %q", key)
		}
		seen[key] = true
		p.skipWS()
		if p.pos < len(p.data) && p.data[p.pos] == ',' {
			p.pos++
			continue
		}
	}
	return op, nil
}

func (p *strictJSONParser) expectColon() {
	p.skipWS()
	if p.pos < len(p.data) && p.data[p.pos] == ':' {
		p.pos++
	}
}

func (p *strictJSONParser) parseKey() (string, error) {
	if p.pos >= len(p.data) || p.data[p.pos] != '"' {
		return "", errors.New("expected a JSON object key")
	}
	return p.parseString()
}

// parseString reads one JSON string (with escapes) starting at p.pos, which
// must point at the opening quote.
func (p *strictJSONParser) parseString() (string, error) {
	if p.pos >= len(p.data) || p.data[p.pos] != '"' {
		return "", errors.New("expected a JSON string")
	}
	p.pos++
	var sb strings.Builder
	for {
		if p.pos >= len(p.data) {
			return "", errors.New("unterminated string")
		}
		ch := p.data[p.pos]
		if ch == '"' {
			p.pos++
			return sb.String(), nil
		}
		if ch != '\\' {
			sb.WriteByte(ch)
			p.pos++
			continue
		}
		p.pos++
		if p.pos >= len(p.data) {
			return "", errors.New("unterminated string escape")
		}
		switch esc := p.data[p.pos]; esc {
		case '"', '\\', '/':
			sb.WriteByte(esc)
			p.pos++
		case 'b':
			sb.WriteByte('\b')
			p.pos++
		case 'f':
			sb.WriteByte('\f')
			p.pos++
		case 'n':
			sb.WriteByte('\n')
			p.pos++
		case 'r':
			sb.WriteByte('\r')
			p.pos++
		case 't':
			sb.WriteByte('\t')
			p.pos++
		case 'u':
			r, err := p.parseHexRune()
			if err != nil {
				return "", fmt.Errorf("invalid unicode escape: %w", err)
			}
			// A high surrogate immediately followed by \uDCxx-\uDFFF is
			// one astral rune; anything else stands alone (kept verbatim).
			if r >= 0xD800 && r <= 0xDBFF && p.pos+6 <= len(p.data) &&
				p.data[p.pos] == '\\' && p.data[p.pos+1] == 'u' {
				saved := p.pos
				p.pos += 2
				lo, err2 := p.parseHexRune()
				if err2 == nil && lo >= 0xDC00 && lo <= 0xDFFF {
					r = 0x10000 + (r-0xD800)<<10 + (lo - 0xDC00)
				} else {
					p.pos = saved
				}
			}
			sb.WriteRune(r)
		default:
			return "", errors.New("invalid escape character")
		}
	}
}

// parseHexRune consumes four hex digits at p.pos+1..p.pos+4 (p.pos must be
// at the 'u' of a \u escape) and advances past them.
func (p *strictJSONParser) parseHexRune() (rune, error) {
	if p.pos+5 > len(p.data) || p.data[p.pos] != 'u' {
		return 0, errors.New("invalid unicode escape")
	}
	var v rune
	for i := 0; i < 4; i++ {
		ch := p.data[p.pos+1+i]
		var d rune
		switch {
		case ch >= '0' && ch <= '9':
			d = rune(ch - '0')
		case ch >= 'a' && ch <= 'f':
			d = rune(ch-'a') + 10
		case ch >= 'A' && ch <= 'F':
			d = rune(ch-'A') + 10
		default:
			return 0, errors.New("invalid unicode escape")
		}
		v = v<<4 | d
	}
	p.pos += 5
	return v, nil
}

// captureValue consumes one JSON value starting at p.pos (no leading WS
// skipping; callers skipWS first) and returns its raw bytes, validated as
// a single well-formed JSON value. Used for the opaque doc.
func (p *strictJSONParser) captureValue() (json.RawMessage, error) {
	start := p.pos
	if err := p.validateValue(); err != nil {
		return nil, err
	}
	return json.RawMessage(p.data[start:p.pos]), nil
}

// validateValue walks one JSON value (object/array/string/number/true/
// false/null) advancing p.pos, returning an error on malformed input.
func (p *strictJSONParser) validateValue() error {
	p.skipWS()
	if p.pos >= len(p.data) {
		return errors.New("missing value")
	}
	switch ch := p.data[p.pos]; ch {
	case '{':
		p.pos++
		first := true
		for {
			p.skipWS()
			if p.pos >= len(p.data) {
				return errors.New("unterminated object")
			}
			if p.data[p.pos] == '}' {
				p.pos++
				return nil
			}
			if !first {
				if p.data[p.pos] != ',' {
					return errors.New("expected ',' in object")
				}
				p.pos++
				p.skipWS()
				if p.pos >= len(p.data) || p.data[p.pos] == '}' {
					return errors.New("trailing comma in object")
				}
			}
			if _, err := p.parseKey(); err != nil {
				return err
			}
			p.expectColon()
			if err := p.validateValue(); err != nil {
				return err
			}
			first = false
		}
	case '[':
		p.pos++
		first := true
		for {
			p.skipWS()
			if p.pos >= len(p.data) {
				return errors.New("unterminated array")
			}
			if p.data[p.pos] == ']' {
				p.pos++
				return nil
			}
			if !first {
				if p.data[p.pos] != ',' {
					return errors.New("expected ',' in array")
				}
				p.pos++
				p.skipWS()
				if p.pos >= len(p.data) || p.data[p.pos] == ']' {
					return errors.New("trailing comma in array")
				}
			}
			if err := p.validateValue(); err != nil {
				return err
			}
			first = false
		}
	case '"':
		_, err := p.parseString()
		return err
	case 't':
		return p.consumeLiteral("true")
	case 'f':
		return p.consumeLiteral("false")
	case 'n':
		return p.consumeLiteral("null")
	default:
		return p.consumeNumber()
	}
}

func (p *strictJSONParser) consumeLiteral(lit string) error {
	if p.pos+len(lit) > len(p.data) || string(p.data[p.pos:p.pos+len(lit)]) != lit {
		return errors.New("invalid JSON literal")
	}
	p.pos += len(lit)
	return nil
}

func (p *strictJSONParser) consumeNumber() error {
	if p.pos >= len(p.data) || (p.data[p.pos] != '-' && (p.data[p.pos] < '0' || p.data[p.pos] > '9')) {
		return errors.New("invalid JSON value")
	}
	if p.data[p.pos] == '-' {
		p.pos++
	}
	digits := 0
	for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
		p.pos++
		digits++
	}
	if digits == 0 {
		return errors.New("invalid JSON number")
	}
	if p.pos < len(p.data) && p.data[p.pos] == '.' {
		p.pos++
		frac := 0
		for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			p.pos++
			frac++
		}
		if frac == 0 {
			return errors.New("invalid JSON number")
		}
	}
	if p.pos < len(p.data) && (p.data[p.pos] == 'e' || p.data[p.pos] == 'E') {
		p.pos++
		if p.pos < len(p.data) && (p.data[p.pos] == '-' || p.data[p.pos] == '+') {
			p.pos++
		}
		exp := 0
		for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			p.pos++
			exp++
		}
		if exp == 0 {
			return errors.New("invalid JSON number")
		}
	}
	return nil
}

func (p *strictJSONParser) skipWS() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}
