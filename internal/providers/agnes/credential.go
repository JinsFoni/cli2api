package agnes

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Credential is the canonical storage payload for provider=agnes.
//
// Agnes AI authenticates with a single Bearer API key created in the developer
// dashboard. There is no refresh token or profile binding; BaseURL is optional
// so a credential can point at a compatible self-hosted hub.
type Credential struct {
	Format  string `json:"format"`
	APIKey  string `json:"api_key"`
	BaseURL string `json:"base_url,omitempty"`
}

// FormatAPIKey trims a pasted key: surrounding whitespace and an inline
// "Bearer " prefix. Agnes keys have no published shape, so only emptiness is
// rejected.
func FormatAPIKey(rawKey string) string {
	key := strings.TrimSpace(rawKey)
	key = strings.TrimPrefix(key, "Bearer ")
	key = strings.TrimPrefix(key, "bearer ")
	return strings.TrimSpace(key)
}

// DecodeCredential accepts the canonical flat form or an aliased bundle (so a
// pasted {"apiKey":…} or {"token":…} still imports).
func DecodeCredential(payload []byte) (Credential, error) {
	var flat Credential
	if err := json.Unmarshal(payload, &flat); err == nil && strings.TrimSpace(flat.APIKey) != "" {
		flat.APIKey = FormatAPIKey(flat.APIKey)
		if flat.Format == "" {
			flat.Format = CredentialFormat
		}
		return flat, nil
	}
	var alias struct {
		Format      string `json:"format"`
		APIKey      string `json:"api_key"`
		APIKeyCamel string `json:"apiKey"`
		Key         string `json:"key"`
		Token       string `json:"token"`
		BaseURL     string `json:"base_url"`
		BaseURLCam  string `json:"baseUrl"`
	}
	if err := json.Unmarshal(payload, &alias); err == nil {
		key := firstNonEmpty(alias.APIKey, alias.APIKeyCamel, alias.Key, alias.Token)
		if strings.TrimSpace(key) != "" {
			return Credential{
				Format:  firstNonEmpty(alias.Format, CredentialFormat),
				APIKey:  FormatAPIKey(key),
				BaseURL: firstNonEmpty(alias.BaseURL, alias.BaseURLCam),
			}, nil
		}
	}
	return Credential{}, fmt.Errorf("agnes credential requires an api key")
}

func (c Credential) Encode() ([]byte, error) {
	c.Format = CredentialFormat
	c.APIKey = FormatAPIKey(c.APIKey)
	return json.Marshal(c)
}

// Ready reports whether the credential can be used.
func (c Credential) Ready() bool {
	return strings.TrimSpace(c.APIKey) != ""
}

// ValidateCredential is the CredentialCodec entry point.
func ValidateCredential(payload []byte) error {
	credential, err := DecodeCredential(payload)
	if err != nil {
		return err
	}
	if !credential.Ready() {
		return fmt.Errorf("agnes credential requires an api key")
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
