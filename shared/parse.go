package shared

import (
	"encoding/json"
)

type messageEnvelope struct {
	Type string `json:"type"`
}

// ParseBinaryIntoMessageTypes decodes msg by its "type" envelope field into
// a Heartbeat, UrlRequest, or UrlResponse. Returns msg unchanged, as []byte,
// if it isn't valid JSON, its type isn't recognized, or it fails to
// unmarshal into the type it claims to be -- callers type-switching on the
// result must treat a []byte case as "not a recognized message", not as an
// error.
func ParseBinaryIntoMessageTypes(msg []byte) any {
	var envelope messageEnvelope

	err := json.Unmarshal(msg, &envelope)
	if err != nil {
		return msg
	}

	switch envelope.Type {
	case "heartbeat":
		var hearbeat Heartbeat
		err := json.Unmarshal(msg, &hearbeat)
		if err != nil {
			return msg
		}
		return Heartbeat{}
	case "url_response":
		var response UrlResponse
		err := json.Unmarshal(msg, &response)
		if err != nil {
			return msg
		} else {
			return response
		}

	case "url_request":
		var request UrlRequest
		err := json.Unmarshal(msg, &request)
		if err != nil {
			return msg
		} else {
			return request
		}
	default:
		return msg
	}
}
