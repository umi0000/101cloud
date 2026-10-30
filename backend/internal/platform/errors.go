package platform

// EDetails attaches public, non-secret business context to a stable API error.
// Callers must not include upstream credentials, raw responses, SQL or script output.
func EDetails(status int, code, message string, details Row) error {
	return &HTTPError{Status: status, Code: code, Message: message, Details: details}
}
