package agnes

import (
	"regexp"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

var (
	keyPattern    = regexp.MustCompile(`sk-[A-Za-z0-9._\-]{6,}`)
	bearerPattern = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]+`)
)

func redactSecrets(text string) string {
	if text == "" {
		return text
	}
	out := keyPattern.ReplaceAllString(text, "sk-[redacted]")
	out = bearerPattern.ReplaceAllString(out, "Bearer [redacted]")
	return out
}

// Classify maps Agnes HTTP errors onto the internal taxonomy. The platform's
// documented status codes follow the OpenAI conventions (401 auth, 402
// payment required, 429 rate limit, 4xx request shape, 5xx upstream).
func Classify(status int, body string) providers.ClassifiedError {
	safeBody := redactSecrets(strings.TrimSpace(body))
	text := strings.ToLower(safeBody)
	switch {
	case status == 401 || strings.Contains(text, "unauthorized") ||
		strings.Contains(text, "invalid api key") || strings.Contains(text, "token not provided"):
		return providers.ClassifiedError{
			Kind:    accounts.KindAuth,
			Status:  status,
			Message: firstNonEmpty(safeBody, "invalid agnes API key; paste a fresh key"),
		}
	case status == 402 || strings.Contains(text, "insufficient") ||
		strings.Contains(text, "balance") || strings.Contains(text, "payment required"):
		return providers.ClassifiedError{
			Kind:    accounts.KindQuota,
			Status:  status,
			Message: firstNonEmpty(safeBody, "agnes balance exhausted"),
		}
	case status == 429 || strings.Contains(text, "rate limit") ||
		strings.Contains(text, "too many requests"):
		return providers.ClassifiedError{
			Kind:    accounts.KindRateLimit,
			Status:  status,
			Message: firstNonEmpty(safeBody, "rate limited"),
		}
	case status == 404:
		return providers.ClassifiedError{Kind: accounts.KindModelNotAvailable, Status: 404, Message: safeBody}
	case status == 400 || status == 403 || status == 413 || status == 415 ||
		status == 422 || status == 431 ||
		strings.Contains(text, "invalid_request_error") ||
		accounts.IsInvalidRequestText(safeBody) ||
		accounts.IsPromptLimitText(safeBody):
		return providers.ClassifiedError{
			Kind:    accounts.KindInvalidRequest,
			Status:  status,
			Message: safeBody,
		}
	case status == 499 || strings.Contains(text, "canceled"):
		return providers.ClassifiedError{Kind: accounts.KindCanceled, Status: 499, Message: safeBody}
	case status >= 500 || status == 0:
		return providers.ClassifiedError{
			Kind:    accounts.KindUnavailable,
			Status:  status,
			Message: firstNonEmpty(safeBody, "agnes upstream unavailable"),
		}
	}
	if safeBody != "" && status >= 400 {
		return providers.ClassifiedError{Kind: accounts.KindUnavailable, Status: status, Message: safeBody}
	}
	return providers.ClassifiedError{}
}

// newProviderError builds the executor-facing error used by the adapter.
func newProviderError(status int, body string) error {
	classified := Classify(status, body)
	if classified.Kind == "" {
		classified = providers.ClassifiedError{
			Kind:    accounts.KindUnavailable,
			Status:  status,
			Message: firstNonEmpty(redactSecrets(strings.TrimSpace(body)), "agnes upstream error"),
		}
	}
	return &providers.Error{
		Kind:    classified.Kind,
		Status:  classified.Status,
		Message: classified.Message,
	}
}
