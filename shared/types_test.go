package shared_test

import (
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"ethanstewart.net/domainchecker/shared"
)

func TestHeartbeatMarshalJSON(t *testing.T) {
	data, err := shared.Heartbeat{}.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON() error = %v", err)
	}

	want := `{"type":"heartbeat"}`
	if string(data) != want {
		t.Errorf("MarshalJSON() = %s, want %s", data, want)
	}
}

func TestHeartbeatUnmarshalJSON(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		var h shared.Heartbeat
		if err := h.UnmarshalJSON([]byte(`{"type":"heartbeat"}`)); err != nil {
			t.Fatalf("UnmarshalJSON() error = %v", err)
		}
	})

	t.Run("wrong type", func(t *testing.T) {
		var h shared.Heartbeat
		if err := h.UnmarshalJSON([]byte(`{"type":"url_request"}`)); err == nil {
			t.Fatal("UnmarshalJSON() error = nil, want error for mismatched type")
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		var h shared.Heartbeat
		if err := h.UnmarshalJSON([]byte(`not json`)); err == nil {
			t.Fatal("UnmarshalJSON() error = nil, want error for malformed JSON")
		}
	})
}

func TestUrlRequestMarshalJSONDiscriminator(t *testing.T) {
	data, err := shared.UrlRequest{}.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON() error = %v", err)
	}

	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if envelope.Type != "url_request" {
		t.Errorf("type = %q, want %q", envelope.Type, "url_request")
	}
}

func TestUrlRequestRoundTrip(t *testing.T) {
	u, err := url.Parse("https://example.com/path?q=1#frag")
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}

	want := shared.UrlRequest{
		Id:        shared.RequestID(42),
		Url:       *u,
		Timestamp: time.Date(2024, 3, 15, 10, 30, 0, 123456789, time.UTC),
		Attempt:   2,
	}

	data, err := want.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON() error = %v", err)
	}

	var got shared.UrlRequest
	if err := got.UnmarshalJSON(data); err != nil {
		t.Fatalf("UnmarshalJSON() error = %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

func TestUrlRequestUnmarshalJSONWrongType(t *testing.T) {
	var req shared.UrlRequest
	if err := req.UnmarshalJSON([]byte(`{"type":"heartbeat"}`)); err == nil {
		t.Fatal("UnmarshalJSON() error = nil, want error for mismatched type")
	}
}

func TestUrlResponseMarshalJSONDiscriminator(t *testing.T) {
	data, err := shared.UrlResponse{}.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON() error = %v", err)
	}

	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if envelope.Type != "url_response" {
		t.Errorf("type = %q, want %q", envelope.Type, "url_response")
	}
}

func TestUrlResponseRoundTrip(t *testing.T) {
	want := shared.UrlResponse{
		Id:          shared.RequestID(7),
		IsAccepted:  false,
		Attempt:     3,
		DenyMessage: shared.DenyMessageRememberedDomain,
	}

	data, err := want.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON() error = %v", err)
	}

	var got shared.UrlResponse
	if err := got.UnmarshalJSON(data); err != nil {
		t.Fatalf("UnmarshalJSON() error = %v", err)
	}

	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

// TestUrlResponseDenyMessageOmittedWhenEmpty confirms DenyMessage's
// omitempty tag actually takes effect on the wire -- an approval should
// not carry a visible, empty "deny_message" field at all.
func TestUrlResponseDenyMessageOmittedWhenEmpty(t *testing.T) {
	data, err := (shared.UrlResponse{Id: 1, IsAccepted: true}).MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON() error = %v", err)
	}
	if strings.Contains(string(data), "deny_message") {
		t.Errorf("MarshalJSON() = %s, want no deny_message field when DenyMessage is empty", data)
	}
}

func TestUrlResponseUnmarshalJSONIgnoresLegacyFailedField(t *testing.T) {
	var resp shared.UrlResponse
	data := []byte(`{"type":"url_response","id":1,"is_accepted":true,"attempt":1,"failed":true}`)
	if err := resp.UnmarshalJSON(data); err != nil {
		t.Fatalf("UnmarshalJSON() error = %v", err)
	}

	want := shared.UrlResponse{Id: shared.RequestID(1), IsAccepted: true, Attempt: 1}
	if resp != want {
		t.Errorf("UnmarshalJSON() = %+v, want %+v (legacy 'failed' field must not affect the result)", resp, want)
	}
}

func TestUrlResponseUnmarshalJSONWrongType(t *testing.T) {
	var resp shared.UrlResponse
	if err := resp.UnmarshalJSON([]byte(`{"type":"url_request"}`)); err == nil {
		t.Fatal("UnmarshalJSON() error = nil, want error for mismatched type")
	}
}

func TestClientIDToString(t *testing.T) {
	ts := time.Date(2024, 3, 15, 10, 30, 0, 123456789, time.UTC)
	id := shared.ClientID(ts)

	got := id.ToString()

	parsed, err := time.Parse(time.RFC3339Nano, got)
	if err != nil {
		t.Fatalf("ToString() = %q, not parseable as RFC3339Nano: %v", got, err)
	}
	if !parsed.Equal(ts) {
		t.Errorf("ToString() round-trips to %v, want %v", parsed, ts)
	}
}
