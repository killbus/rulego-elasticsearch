package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/rulego/rulego/api/types"
	"github.com/rulego/rulego/utils/maps"
	ruleReflect "github.com/rulego/rulego/utils/reflect"
)

// formFields indexes the extracted component form fields by name.
// It goes through New() exactly like the server does: engine
// RuleComponentRegistry.GetComponentForms calls
// reflect.GetComponentForm(component.New()), so the form (including
// DefaultValue) is derived from the prototype New() returns.
func formFields(t *testing.T) map[string]types.ComponentFormField {
	t.Helper()
	form := ruleReflect.GetComponentForm((&elasticsearchWriteNode{}).New())
	if form.Type != "elasticsearchWrite" {
		t.Fatalf("form type = %q", form.Type)
	}
	fields := make(map[string]types.ComponentFormField, len(form.Fields))
	for _, f := range form.Fields {
		fields[f.Name] = f
	}
	return fields
}

// hasRequiredRule reports whether the field carries the frontend
// required-validation rule. NOTE: in the locked ABI (rulego v1.0.0)
// GetFields routes the required tag into Rules only; the Required bool is
// never populated. Anchor to the ABI that is actually compiled in.
func hasRequiredRule(field types.ComponentFormField) bool {
	for _, rule := range field.Rules {
		if rule["required"] == true {
			return true
		}
	}
	return false
}

// TestEditorFormMarksServerPrimaryAndRequired anchors the shared-node editor
// contract: server must be the ref:// primary field and carry the required
// validation rule, exactly like the built-in SharedNode components
// (mqtt_client, net).
func TestEditorFormMarksServerPrimaryAndRequired(t *testing.T) {
	fields := formFields(t)
	server := fields["server"]
	if server.Ref != "primary" || !hasRequiredRule(server) || server.Label == "" {
		t.Fatalf("server field = %+v, want ref=primary, required rule, labeled", server)
	}
}

// TestEditorFormMarksPolicyFieldsShared anchors that connection policy is
// pool-owner-owned: auth/tls/timeouts are ref:"shared", so a borrower's
// editor form hides them (the owner node supplies them).
func TestEditorFormMarksPolicyFieldsShared(t *testing.T) {
	fields := formFields(t)
	for _, name := range []string{"auth", "tls", "timeoutMs", "maxRequestBytes", "maxOperations"} {
		if got := fields[name].Ref; got != "shared" {
			t.Fatalf("field %s ref = %q, want shared", name, got)
		}
	}
}

// TestEditorFormAuthTypeIsSelect anchors the editor control for auth.type:
// the field must carry a select component whose options are exactly the
// allowed wire values ("", apiKey, basic) — the same whitelist buildAuthHeader
// enforces at runtime.
func TestEditorFormAuthTypeIsSelect(t *testing.T) {
	fields := formFields(t)
	auth := fields["auth"]
	if auth.Type != "struct" || len(auth.Fields) == 0 {
		t.Fatalf("auth field = %+v, want a struct with nested fields", auth)
	}
	var authType types.ComponentFormField
	for _, f := range auth.Fields {
		if f.Name == "type" {
			authType = f
		}
	}
	if authType.Component == nil || authType.Component["type"] != "select" {
		t.Fatalf("auth.type component = %#v, want a select", authType.Component)
	}
	raw, err := json.Marshal(authType.Component["options"])
	if err != nil {
		t.Fatal(err)
	}
	var options []struct {
		Label string `json:"label"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &options); err != nil {
		t.Fatal(err)
	}
	values := make([]string, 0, len(options))
	for _, o := range options {
		values = append(values, o.Value)
	}
	want := []string{"", "apiKey", "basic"}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("auth.type options = %#v, want values %#v (the buildAuthHeader whitelist)", options, want)
	}
}

// TestEditorFormDoesNotChangeWireContract is the anti-regression anchor:
// adding editor tags must not move the wire contract. Every config field's
// JSON name stays exactly what the runtime decodes, and Map2Struct still
// round-trips a full owner configuration.
func TestEditorFormDoesNotChangeWireContract(t *testing.T) {
	wantTags := map[string]string{
		"Server": "server", "Auth": "auth", "TLS": "tls",
		"TimeoutMs": "timeoutMs", "MaxRequestBytes": "maxRequestBytes", "MaxOperations": "maxOperations",
	}
	for name, want := range wantTags {
		field, _ := reflect.TypeOf(clientConfig{}).FieldByName(name)
		if got := field.Tag.Get("json"); got != want {
			t.Fatalf("clientConfig.%s json tag = %q, want %q", name, got, want)
		}
	}
	cfg := map[string]any{
		"server":          "http://127.0.0.1:9200",
		"auth":            map[string]any{"type": "basic", "username": "u", "password": "p"},
		"tls":             map[string]any{"caFile": "/ca.pem"},
		"timeoutMs":       1500,
		"maxRequestBytes": int64(1 << 20),
		"maxOperations":   10,
	}
	var out clientConfig
	if err := maps.Map2Struct(cfg, &out); err != nil {
		t.Fatal(err)
	}
	if out.Server != "http://127.0.0.1:9200" || out.Auth.Type != "basic" || out.Auth.Username != "u" ||
		out.TLS.CAFile != "/ca.pem" || out.TimeoutMs != 1500 || out.MaxRequestBytes != int64(1<<20) || out.MaxOperations != 10 {
		t.Fatalf("round-trip = %+v", out)
	}
	if err := normalizeClientConfig(&out); err != nil {
		t.Fatal(err)
	}
}

// TestEditorFormShowsRealDefaults anchors that the form displays the real
// built-in defaults (not 0): the extracted DefaultValue of each limit field
// must equal the constant normalizeClientConfig applies to zero/negative
// input. This is display-only — the wire path is covered by
// TestEditorFormDoesNotChangeWireContract.
func TestEditorFormShowsRealDefaults(t *testing.T) {
	fields := formFields(t)
	// maxOperations is an int in clientConfig; the limit bytes/timeout are
	// int64. Normalize whatever numeric type DefaultValue carries to int64.
	want := map[string]int64{
		"timeoutMs":       defaultTimeoutMs,
		"maxRequestBytes": defaultMaxRequestBytes,
		"maxOperations":   int64(defaultMaxOperations),
	}
	for name, wantDefault := range want {
		var got int64
		switch v := fields[name].DefaultValue.(type) {
		case int64:
			got = v
		case int:
			got = int64(v)
		default:
			t.Fatalf("field %s DefaultValue = %#v (type %T), want a numeric default", name, fields[name].DefaultValue, fields[name].DefaultValue)
		}
		if got != wantDefault {
			t.Fatalf("field %s DefaultValue = %d, want %d", name, got, wantDefault)
		}
	}
}

// TestFormDefaultsDoNotLeakIntoWireBehavior is the safety anchor for the
// New() prefill: a configuration that omits the limit fields (what a user
// gets by leaving the form defaults as-is is NOT the wire case — the wire
// case is a config that simply lacks the keys) must still resolve to the
// same defaults through normalizeClientConfig, and a config that does set
// them must keep the user's value.
func TestFormDefaultsDoNotLeakIntoWireBehavior(t *testing.T) {
	var omitted clientConfig
	if err := maps.Map2Struct(map[string]any{"server": "http://x:9200"}, &omitted); err != nil {
		t.Fatal(err)
	}
	if err := normalizeClientConfig(&omitted); err != nil {
		t.Fatal(err)
	}
	if omitted.TimeoutMs != defaultTimeoutMs || omitted.MaxRequestBytes != defaultMaxRequestBytes || omitted.MaxOperations != defaultMaxOperations {
		t.Fatalf("omitted fields = %+v, want built-in defaults", omitted)
	}
	var explicit clientConfig
	if err := maps.Map2Struct(map[string]any{
		"server": "http://x:9200", "timeoutMs": 1500, "maxOperations": 5,
	}, &explicit); err != nil {
		t.Fatal(err)
	}
	if err := normalizeClientConfig(&explicit); err != nil {
		t.Fatal(err)
	}
	if explicit.TimeoutMs != 1500 || explicit.MaxOperations != 5 || explicit.MaxRequestBytes != defaultMaxRequestBytes {
		t.Fatalf("explicit fields = %+v, want user values kept + missing one defaulted", explicit)
	}
}
