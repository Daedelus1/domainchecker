package shared

// DenyMessage* are the exact UrlResponse.DenyMessage strings the client
// and server produce, exported as constants so callers (including tests)
// key off them instead of a copy-pasted literal that would silently drift
// if the wording here ever changes.
const (
	// DenyMessagePreviouslyRemembered is used when a remembered decision
	// (at either URL or domain scope) resolves a request as a denial
	// without prompting a human.
	DenyMessagePreviouslyRemembered = "Denied (previously remembered)"
	// DenyMessageOnce is used for a fresh denial that isn't remembered: an
	// explicit "n" answer, or any unrecognized input.
	DenyMessageOnce = "Denied (not remembered)"
	// DenyMessageRememberedURL is used for a fresh "nu" denial, just
	// recorded for this exact URL.
	DenyMessageRememberedURL = "Denied (remembered for this URL)"
	// DenyMessageRememberedDomain is used for a fresh "nd" denial, just
	// recorded for this domain.
	DenyMessageRememberedDomain = "Denied (remembered for this domain)"
	// DenyMessageTimeout is used when no answer arrives within the
	// client's USER_INPUT_TIMEOUT_DURATION.
	DenyMessageTimeout = "Denied (no response from client)"
	// DenyMessageNoClient is used when the server rejects a request
	// immediately because no client has ever connected.
	DenyMessageNoClient = "Denied (no client connected)"
)
