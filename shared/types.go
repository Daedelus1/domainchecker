package shared

import (
	"encoding/json"
	"errors"
	"net/url"
	"time"
)

type (
	RequestID int
	ClientID  time.Time
)

type Heartbeat struct{}

type UrlRequest struct {
	Id        RequestID `json:"id"`
	Url       url.URL   `json:"url"`
	Timestamp time.Time `json:"time"`
	// Attempt distinguishes retries of the same Id: a request that times
	// out server-side gets requeued and resent under a new Attempt, and the
	// client echoes it back on UrlResponse so a stale answer to an earlier
	// attempt (e.g. one prompt's auto-deny arriving after a human already
	// answered a later retry) can be told apart from the current one.
	Attempt int `json:"attempt"`
}

type UrlResponse struct {
	Id         RequestID `json:"id"`
	IsAccepted bool      `json:"is_accepted"`
	Attempt    int       `json:"attempt"`
	// DenyMessage explains a denial -- why it happened, and whether (and
	// at what scope) it was remembered -- for display alongside Squid's
	// ERR response. Always empty when IsAccepted is true.
	DenyMessage string `json:"deny_message,omitempty"`
}

type InboundMessage struct {
	Id  ClientID
	Msg []byte
}

// MarshalJSON always encodes a Heartbeat as {"type":"heartbeat"}; there is
// no other state to serialize.
func (h Heartbeat) MarshalJSON() ([]byte, error) {
	return []byte(`{"type":"heartbeat"}`), nil
}

// UnmarshalJSON only validates that data's "type" field is "heartbeat"; it
// returns an error for anything else and otherwise leaves h unchanged.
func (h *Heartbeat) UnmarshalJSON(data []byte) error {
	var temp struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &temp); err != nil {
		return err
	}
	if temp.Type != "heartbeat" {
		return errors.New("invalid message type: expected 'heartbeat'")
	}
	return nil
}

// MarshalJSON encodes h with an explicit "type":"url_request" discriminator
// so ParseBinaryIntoMessageTypes can route it.
func (h UrlRequest) MarshalJSON() ([]byte, error) {
	temp := struct {
		Type      string    `json:"type"`
		Id        int       `json:"id"`
		Url       url.URL   `json:"url"`
		Timestamp time.Time `json:"time"`
		Attempt   int       `json:"attempt"`
	}{
		"url_request",
		int(h.Id),
		h.Url,
		h.Timestamp,
		h.Attempt,
	}
	return json.Marshal(temp)
}

// UnmarshalJSON requires data's "type" field to be "url_request" and
// returns an error otherwise. Id, Url, Timestamp, and Attempt all
// round-trip.
func (h *UrlRequest) UnmarshalJSON(data []byte) error {
	var temp struct {
		Type      string    `json:"type"`
		Id        int       `json:"id"`
		Url       url.URL   `json:"url"`
		Timestamp time.Time `json:"time"`
		Attempt   int       `json:"attempt"`
	}
	if err := json.Unmarshal(data, &temp); err != nil {
		return err
	}
	if temp.Type != "url_request" {
		return errors.New("invalid message type: expected 'url_request'")
	}
	h.Id = RequestID(temp.Id)
	h.Url = temp.Url
	h.Timestamp = temp.Timestamp
	h.Attempt = temp.Attempt
	return nil
}

// MarshalJSON encodes h with an explicit "type":"url_response"
// discriminator so ParseBinaryIntoMessageTypes can route it.
func (h UrlResponse) MarshalJSON() ([]byte, error) {
	temp := struct {
		Type        string `json:"type"`
		Id          int    `json:"id"`
		IsAccepted  bool   `json:"is_accepted"`
		Attempt     int    `json:"attempt"`
		DenyMessage string `json:"deny_message,omitempty"`
	}{
		"url_response",
		int(h.Id),
		h.IsAccepted,
		h.Attempt,
		h.DenyMessage,
	}
	return json.Marshal(temp)
}

// UnmarshalJSON requires data's "type" field to be "url_response" and
// returns an error otherwise. Id, IsAccepted, Attempt, and DenyMessage
// round-trip.
func (h *UrlResponse) UnmarshalJSON(data []byte) error {
	var temp struct {
		Type        string `json:"type"`
		Id          int    `json:"id"`
		IsAccepted  bool   `json:"is_accepted"`
		Attempt     int    `json:"attempt"`
		DenyMessage string `json:"deny_message"`
	}
	if err := json.Unmarshal(data, &temp); err != nil {
		return err
	}
	if temp.Type != "url_response" {
		return errors.New("invalid message type: expected 'url_response'")
	}
	h.Id = RequestID(temp.Id)
	h.IsAccepted = temp.IsAccepted
	h.Attempt = temp.Attempt
	h.DenyMessage = temp.DenyMessage
	return nil
}

// ToString renders the ID as an RFC3339Nano timestamp string, suitable for
// logging. ClientID values are otherwise opaque time.Time wrappers used
// only for uniqueness and ordering.
func (c ClientID) ToString() string {
	return time.Time(c).Format(time.RFC3339Nano)
}
