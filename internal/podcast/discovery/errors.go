package discovery

import "net/http"

// Error is a discovery failure as the API answers it: a status, a machine code,
// a safe message (never a provider's body, header, URL or credential) and, for
// a refusal, the seconds to wait (Retry-After, 0 for none).
type Error struct {
	Status     int
	Code       string
	Message    string
	RetryAfter int
}

func (e *Error) Error() string { return e.Code }

// withRetry is a copy of e carrying Retry-After seconds.
func (e *Error) withRetry(seconds int) *Error {
	c := *e
	c.RetryAfter = seconds
	return &c
}

// The failures (the messages are the shared wire.json's where it names them).
var (
	errInvalidRequest  = &Error{Status: http.StatusBadRequest, Code: "invalid_request", Message: "Invalid request."}
	errInvalidQuery    = &Error{Status: http.StatusBadRequest, Code: "invalid_query", Message: "Search for 2 to 200 characters."}
	errInvalidProvider = &Error{Status: http.StatusBadRequest, Code: "invalid_provider", Message: "Unknown discovery provider."}
	errInvalidLimit    = &Error{Status: http.StatusBadRequest, Code: "invalid_limit", Message: "Limit must be a whole number from 1 to 50."}
	errInvalidCountry  = &Error{Status: http.StatusBadRequest, Code: "invalid_country", Message: "Unknown country."}

	ErrProviderUnavailable = &Error{Status: http.StatusConflict, Code: "provider_unavailable", Message: "This discovery provider is not configured."}
	errUserRateLimited     = &Error{Status: http.StatusTooManyRequests, Code: "discovery_rate_limited", Message: "Too many discovery requests. Try again shortly."}
	errProviderRateLimited = &Error{Status: http.StatusTooManyRequests, Code: "provider_rate_limited", Message: "The directory is busy. Try again shortly."}
	errBusy                = &Error{Status: http.StatusServiceUnavailable, Code: "discovery_busy", Message: "Discovery is busy. Try again shortly.", RetryAfter: 1}
	errTimeout             = &Error{Status: http.StatusGatewayTimeout, Code: "discovery_timeout", Message: "The directory did not answer in time."}
	errRejected            = &Error{Status: http.StatusBadGateway, Code: "provider_rejected", Message: "The directory refused this server's credentials."}
	errUnavailable         = &Error{Status: http.StatusBadGateway, Code: "discovery_unavailable", Message: "The directory could not be reached."}
	errArtNotFound         = &Error{Status: http.StatusNotFound, Code: "artwork_not_found", Message: "Artwork not found."}
	errArtUnavailable      = &Error{Status: http.StatusBadGateway, Code: "artwork_unavailable", Message: "The artwork could not be fetched."}
	// ErrModuleOff is a search or artwork fetch the podcasts module's switch
	// ended while it ran (Reset).
	ErrModuleOff = &Error{Status: http.StatusNotFound, Code: "module_off", Message: "this module is turned off"}
)
