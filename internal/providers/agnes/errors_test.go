package agnes

import (
	"testing"

	"github.com/caigee-cmd/cli2api/internal/accounts"
)

func TestClassifyStatuses(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{401, `{"error":{"message":"Token not provided"}}`, accounts.KindAuth},
		{402, "insufficient balance", accounts.KindQuota},
		{429, "rate limit exceeded", accounts.KindRateLimit},
		{404, "model not found", accounts.KindModelNotAvailable},
		{400, `{"error":{"message":"Invalid request"}}`, accounts.KindInvalidRequest},
		{403, "permission denied", accounts.KindInvalidRequest},
		{499, "client closed request", accounts.KindCanceled},
		{500, "internal error", accounts.KindUnavailable},
		{502, "bad gateway", accounts.KindUnavailable},
		{524, "a timeout occurred", accounts.KindUnavailable},
	}
	for _, tc := range cases {
		got := Classify(tc.status, tc.body)
		if got.Kind != tc.want {
			t.Fatalf("Classify(%d, %q) kind = %q, want %q", tc.status, tc.body, got.Kind, tc.want)
		}
	}
}

func TestClassifyRedactsKeys(t *testing.T) {
	got := Classify(500, "bad key sk-secretkey123456 rejected")
	if got.Kind != accounts.KindUnavailable {
		t.Fatalf("kind = %q", got.Kind)
	}
	if contains(got.Message, "sk-secretkey123456") {
		t.Fatalf("key leaked in message: %s", got.Message)
	}
}

func TestNewProviderErrorFallback(t *testing.T) {
	err := newProviderError(599, "")
	if err == nil {
		t.Fatal("expected an error")
	}
}
