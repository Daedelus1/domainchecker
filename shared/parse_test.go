package shared_test

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"ethanstewart.net/domainchecker/shared"
)

func TestParseBinaryIntoMessageTypesHeartbeat(t *testing.T) {
	msg := []byte(`{"type":"heartbeat"}`)

	got := shared.ParseBinaryIntoMessageTypes(msg)

	if _, ok := got.(shared.Heartbeat); !ok {
		t.Fatalf("ParseBinaryIntoMessageTypes() = %#v (%T), want Heartbeat", got, got)
	}
}

func TestParseBinaryIntoMessageTypesUrlRequest(t *testing.T) {
	want := shared.UrlRequest{
		Id:        shared.RequestID(5),
		Timestamp: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC),
		Attempt:   1,
	}
	data, err := want.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON() error = %v", err)
	}

	got := shared.ParseBinaryIntoMessageTypes(data)

	req, ok := got.(shared.UrlRequest)
	if !ok {
		t.Fatalf("ParseBinaryIntoMessageTypes() = %#v (%T), want UrlRequest", got, got)
	}
	if !reflect.DeepEqual(req, want) {
		t.Errorf("ParseBinaryIntoMessageTypes() = %+v, want %+v", req, want)
	}
}

func TestParseBinaryIntoMessageTypesUrlResponse(t *testing.T) {
	want := shared.UrlResponse{
		Id:         shared.RequestID(9),
		IsAccepted: true,
		Attempt:    2,
	}
	data, err := want.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON() error = %v", err)
	}

	got := shared.ParseBinaryIntoMessageTypes(data)

	resp, ok := got.(shared.UrlResponse)
	if !ok {
		t.Fatalf("ParseBinaryIntoMessageTypes() = %#v (%T), want UrlResponse", got, got)
	}
	if resp != want {
		t.Errorf("ParseBinaryIntoMessageTypes() = %+v, want %+v", resp, want)
	}
}

// assertFallsBackToRawBytes covers every case where ParseBinaryIntoMessageTypes
// must hand msg back unchanged, as []byte, rather than panicking or
// returning some zero-value typed message.
func assertFallsBackToRawBytes(t *testing.T, msg []byte) {
	t.Helper()

	got := shared.ParseBinaryIntoMessageTypes(msg)

	gotBytes, ok := got.([]byte)
	if !ok {
		t.Fatalf("ParseBinaryIntoMessageTypes(%s) = %#v (%T), want []byte", msg, got, got)
	}
	if !bytes.Equal(gotBytes, msg) {
		t.Errorf("ParseBinaryIntoMessageTypes(%s) = %s, want it unchanged", msg, gotBytes)
	}
}

func TestParseBinaryIntoMessageTypesFallback(t *testing.T) {
	tests := map[string][]byte{
		"empty input":             {},
		"malformed JSON syntax":   []byte(`{not valid json`),
		"valid JSON, wrong shape": []byte(`[1,2,3]`),
		"unrecognized type":       []byte(`{"type":"unknown_thing","foo":"bar"}`),
		"missing type field":      []byte(`{"id":1}`),
		// Envelope decoding succeeds (it only looks at "type"), but the
		// second, full-struct unmarshal fails because url.URL -- which has
		// no custom UnmarshalJSON -- can't decode a JSON number.
		"url_request with malformed url field": []byte(`{"type":"url_request","id":1,"url":12345,"attempt":1}`),
		// Same idea: is_accepted must be a bool.
		"url_response with malformed is_accepted field": []byte(`{"type":"url_response","id":1,"is_accepted":"not-a-bool","attempt":1}`),
	}

	for name, msg := range tests {
		t.Run(name, func(t *testing.T) {
			assertFallsBackToRawBytes(t, msg)
		})
	}
}
