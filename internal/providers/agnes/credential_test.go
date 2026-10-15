package agnes

import (
	"strings"
	"testing"
)

func TestFormatAPIKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  sk-abc  ", "sk-abc"},
		{"Bearer sk-abc", "sk-abc"},
		{"bearer sk-abc", "sk-abc"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := FormatAPIKey(tc.in); got != tc.want {
			t.Fatalf("FormatAPIKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDecodeCredentialAliases(t *testing.T) {
	cases := []string{
		`{"api_key":"sk-abc"}`,
		`{"apiKey":"sk-abc"}`,
		`{"key":"sk-abc"}`,
		`{"token":"Bearer sk-abc"}`,
	}
	for _, payload := range cases {
		credential, err := DecodeCredential([]byte(payload))
		if err != nil {
			t.Fatalf("DecodeCredential(%s): %v", payload, err)
		}
		if credential.APIKey != "sk-abc" {
			t.Fatalf("DecodeCredential(%s) key = %q", payload, credential.APIKey)
		}
		if credential.Format != CredentialFormat {
			t.Fatalf("DecodeCredential(%s) format = %q", payload, credential.Format)
		}
	}
	if _, err := DecodeCredential([]byte(`{"nope":1}`)); err == nil {
		t.Fatal("DecodeCredential accepted a payload without a key")
	}
}

func TestValidateCredential(t *testing.T) {
	if err := ValidateCredential([]byte(`{"api_key":"sk-abc"}`)); err != nil {
		t.Fatalf("ValidateCredential valid: %v", err)
	}
	if err := ValidateCredential([]byte(`{}`)); err == nil {
		t.Fatal("ValidateCredential accepted an empty credential")
	}
}

func TestCredentialRoundTrip(t *testing.T) {
	cred := Credential{APIKey: " sk-abc ", BaseURL: "https://example.com/v1/"}
	payload, err := cred.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.Contains(string(payload), `"format":"agnes-key-v1"`) {
		t.Fatalf("Encode lost the format: %s", payload)
	}
	decoded, err := DecodeCredential(payload)
	if err != nil {
		t.Fatalf("DecodeCredential: %v", err)
	}
	if decoded.APIKey != "sk-abc" || decoded.BaseURL != "https://example.com/v1/" {
		t.Fatalf("round trip mismatch: %+v", decoded)
	}
	if !decoded.Ready() {
		t.Fatal("ready credential reported not ready")
	}
}
