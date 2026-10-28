package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Wire contract for one elasticsearchWrite message. The node decodes the
// input JSON into a writeRequest; buildBulk turns it into the NDJSON body.
const (
	opCreate = "create"
	opIndex  = "index"

	defaultTimeoutMs       = int64(30000)
	defaultMaxRequestBytes = int64(30 << 20) // 30 MiB
	defaultMaxOperations   = 500

	maxBodyBytes = int64(100 << 20) // absolute ceiling for the configurable cap
)

// indexNamePattern mirrors the ES 9.x index-name rule used by the node's
// fail-fast check: lowercase alphanumerics plus dot, underscore, dash.
var indexNamePattern = regexp.MustCompile(`^[a-z0-9._-]+$`)

// writeRequest is the per-invocation message contract (see design.md).
type writeRequest struct {
	Index      string   `json:"index"`
	Operations []bulkOp `json:"operations"`
}

// bulkOp is one write operation. Doc is held as raw JSON and serialized
// opaquely; it is never echoed back downstream.
type bulkOp struct {
	Op  string          `json:"op"`
	ID  string          `json:"id"`
	Doc json.RawMessage `json:"doc"`
}

// clientConfig is the node's stable instance policy (the owner form).
// Borrower nodes carry only Server = ref://<id> and leave the rest zero.
// The label/desc/required/ref tags drive the RuleGo editor form (extracted
// by rulego utils/reflect GetFields); ref:"primary" marks the ref:// field
// that switches the editor to the shared-node UI, ref:"shared" marks fields
// owned by the pool owner (hidden on a borrower's form).
type clientConfig struct {
	Server          string     `json:"server" label:"Server" desc:"Elasticsearch base URL (http/https), or ref://<id> to borrow a shared node" required:"true" ref:"primary"`
	Auth            authConfig `json:"auth" label:"Auth" desc:"Request authentication policy" ref:"shared"`
	TLS             tlsConfig  `json:"tls" label:"TLS" desc:"TLS verification policy (verification is always on)" ref:"shared"`
	TimeoutMs       int64      `json:"timeoutMs" label:"Timeout (ms)" desc:"Per-request deadline in milliseconds (default 30000)" ref:"shared"`
	MaxRequestBytes int64      `json:"maxRequestBytes" label:"Max Request Bytes" desc:"Maximum message body size in bytes (default 31457280)" ref:"shared"`
	MaxOperations   int        `json:"maxOperations" label:"Max Operations" desc:"Maximum write operations per message (default 500)" ref:"shared"`
}

type authConfig struct {
	Type     string `json:"type" label:"Type" desc:"Authentication type (empty disables auth)" component:"{\"type\":\"select\",\"options\":[{\"label\":\"(none)\",\"value\":\"\"},{\"label\":\"API key\",\"value\":\"apiKey\"},{\"label\":\"Basic\",\"value\":\"basic\"}]}"`
	APIKey   string `json:"apiKey" label:"API Key" desc:"API key value, used when type=apiKey"`
	Username string `json:"username" label:"Username" desc:"Basic auth username, used when type=basic"`
	Password string `json:"password" label:"Password" desc:"Basic auth password, used when type=basic"`
}

type tlsConfig struct {
	CAFile string `json:"caFile" label:"CA File" desc:"Optional PEM CA bundle appended to the system trust store"`
}

// esClient is a shared Elasticsearch HTTP client. There is deliberately no
// http.Client.Timeout: the per-request deadline comes from a context so that
// pooled connections are not torn down on every call.
type esClient struct {
	httpClient *http.Client
	baseURL    string // normalized, no trailing slash
	authHeader string // precomputed Authorization value; empty when unauthenticated
	timeout    time.Duration
}

// newESClient builds a client from an inline owner configuration. It performs
// no network I/O, so an ES outage at boot cannot block RuleGo startup; only
// configuration errors (bad URL, unreadable CA file) fail here.
func newESClient(cfg clientConfig) (*esClient, error) {
	u, err := url.Parse(cfg.Server)
	if err != nil {
		return nil, fmt.Errorf("invalid server URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported server scheme %q (want http or https)", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("server URL is missing a host")
	}

	tlsCfg, err := buildTLSConfig(cfg.TLS)
	if err != nil {
		return nil, err
	}
	authHeader, err := buildAuthHeader(cfg.Auth)
	if err != nil {
		return nil, err
	}

	timeout := time.Duration(cfg.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = time.Duration(defaultTimeoutMs) * time.Millisecond
	}

	return &esClient{
		httpClient: &http.Client{
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
			// Timeout left zero: deadline is applied per request via context.
		},
		baseURL:    strings.TrimSuffix(u.String(), "/"),
		authHeader: authHeader,
		timeout:    timeout,
	}, nil
}

// Close releases idle pooled connections. The client itself is owned by the
// SharedNode lifecycle; nothing here talks to the network.
func (c *esClient) Close() error {
	c.httpClient.CloseIdleConnections()
	return nil
}

// Bulk POSTs the serialized NDJSON body to /{index}/_bulk. It returns the
// HTTP status and response body; err is non-nil only for transport-level
// failures (network, timeout, context cancel), never for 4xx/5xx responses.
func (c *esClient) Bulk(ctx context.Context, index string, body []byte) (int, []byte, error) {
	endpoint := c.baseURL + "/" + index + "/_bulk"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	if c.authHeader != "" {
		req.Header.Set("Authorization", c.authHeader)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, respBody, nil
}

// normalizeClientConfig fills defaults and validates an owner configuration.
// It runs only for inline owners (not ref:// borrowers), whose policy is
// carried by the pool owner.
func normalizeClientConfig(cfg *clientConfig) error {
	const maxMs = int64(time.Duration(math.MaxInt64) / time.Millisecond)
	if cfg.TimeoutMs <= 0 {
		cfg.TimeoutMs = defaultTimeoutMs
	}
	if cfg.TimeoutMs > maxMs {
		return errors.New("timeoutMs is out of range")
	}
	if cfg.MaxRequestBytes <= 0 {
		cfg.MaxRequestBytes = defaultMaxRequestBytes
	}
	if cfg.MaxRequestBytes > maxBodyBytes {
		return errors.New("maxRequestBytes exceeds the supported ceiling")
	}
	if cfg.MaxOperations <= 0 {
		cfg.MaxOperations = defaultMaxOperations
	}
	if _, err := buildAuthHeader(cfg.Auth); err != nil {
		return err
	}
	return nil
}

// buildBulk serializes a writeRequest's operations into an NDJSON bulk body.
// It validates the index name, the op whitelist, the required id, and the
// byte budget, failing fast (as a terminal error) before any network I/O.
func buildBulk(index string, ops []bulkOp, limit int64) ([]byte, error) {
	if !indexNamePattern.MatchString(index) {
		return nil, fmt.Errorf("invalid index name %q (must match %s)", index, indexNamePattern.String())
	}
	if len(ops) == 0 {
		return nil, errors.New("operations must be non-empty")
	}
	var buf bytes.Buffer
	for i, op := range ops {
		if err := validateBulkOp(op); err != nil {
			return nil, fmt.Errorf("operation %d: %w", i+1, err)
		}
		if err := appendOp(&buf, index, op); err != nil {
			return nil, fmt.Errorf("operation %d: %w", i+1, err)
		}
		if limit > 0 && int64(buf.Len()) > limit {
			return nil, fmt.Errorf("bulk body %d bytes exceeds limit %d", buf.Len(), limit)
		}
	}
	return buf.Bytes(), nil
}

func validateBulkOp(op bulkOp) error {
	switch op.Op {
	case opCreate, opIndex:
	default:
		return fmt.Errorf("unsupported op %q (allowed: %s, %s)", op.Op, opCreate, opIndex)
	}
	if strings.TrimSpace(op.ID) == "" {
		return errors.New("id is required")
	}
	if len(op.Doc) == 0 {
		return errors.New("doc is required")
	}
	return nil
}

// appendOp writes the action line then the doc line. The doc is re-serialized
// with json.Compact so it is always a single-line JSON value, which is what
// NDJSON requires (embedded newlines would corrupt the stream).
func appendOp(buf *bytes.Buffer, index string, op bulkOp) error {
	meta := struct {
		Index string `json:"_index"`
		ID    string `json:"_id"`
	}{Index: index, ID: op.ID}
	action, err := json.Marshal(map[string]any{op.Op: meta})
	if err != nil {
		return fmt.Errorf("encode action: %w", err)
	}
	buf.Write(action)
	buf.WriteByte('\n')

	var compacted bytes.Buffer
	if err := json.Compact(&compacted, op.Doc); err != nil {
		return fmt.Errorf("invalid doc: %w", err)
	}
	buf.Write(compacted.Bytes())
	buf.WriteByte('\n')
	return nil
}

func buildAuthHeader(auth authConfig) (string, error) {
	switch strings.ToLower(strings.TrimSpace(auth.Type)) {
	case "":
		return "", nil
	case "apikey":
		if strings.TrimSpace(auth.APIKey) == "" {
			return "", errors.New("apiKey auth requires an apiKey value")
		}
		return "ApiKey " + auth.APIKey, nil
	case "basic":
		if auth.Username == "" {
			return "", errors.New("basic auth requires a username")
		}
		cred := auth.Username + ":" + auth.Password
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(cred)), nil
	default:
		return "", fmt.Errorf("unsupported auth type %q (allowed: apiKey, basic)", auth.Type)
	}
}

// buildTLSConfig returns a tls.Config with system trust plus any caller
// supplied CA. Verification is always on: InsecureSkipVerify is never set,
// deliberately replacing the restApiCall footgun.
func buildTLSConfig(cfg tlsConfig) (*tls.Config, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if strings.TrimSpace(cfg.CAFile) != "" {
		pem, readErr := os.ReadFile(cfg.CAFile)
		if readErr != nil {
			return nil, fmt.Errorf("read caFile: %w", readErr)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("caFile contains no usable certificates")
		}
	}
	return &tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
		// InsecureSkipVerify intentionally omitted (false).
	}, nil
}
