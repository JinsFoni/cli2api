package qoder

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
)

// NativeStore is the persistence surface the in-process control-plane paths
// (check-in, quota refresh, probing) need. It is satisfied by accounts.Store;
// the worker keeps serving chat traffic exactly as before.
type NativeStore interface {
	Get(ctx context.Context, accountID string) (accounts.Account, error)
	LoadCredential(ctx context.Context, accountID string) (accounts.NativeCredential, error)
	SaveCredential(ctx context.Context, accountID, authType string, credential accounts.NativeCredential) error
	GetSecret(ctx context.Context, name string) (string, bool, error)
}

// userBlob mirrors the CLI's ~/.qoder*/.auth/user JSON. ExpireTime is epoch
// milliseconds upstream; tokens are prefixed (drt-/jrt-/pt-) so the refresh
// endpoint can be picked from the token itself.
type userBlob struct {
	AccessToken        string `json:"access_token"`
	RefreshToken       string `json:"refresh_token"`
	UID                string `json:"uid"`
	Name               string `json:"name"`
	ExpireTime         int64  `json:"expire_time"`
	SecurityOauthToken string `json:"security_oauth_token"`

	// Raw is the original blob bytes; write-back patches only the token
	// fields and re-encodes in the original form (plaintext stays plaintext).
	Raw []byte `json:"-"`
	// MachineID is the machine id the blob was decoded with (stored value or
	// the deterministic derivation for machine-less imports). Never serialized.
	MachineID string `json:"-"`
}

// machineID returns the machine id bound to this credential read.
func (u userBlob) machineID() string {
	return strings.TrimSpace(u.MachineID)
}

// bearerToken prefers the security_oauth_token the CLI keeps alongside
// access_token, falling back to access_token itself.
func (u userBlob) bearerToken() string {
	if token := strings.TrimSpace(u.SecurityOauthToken); token != "" {
		return token
	}
	return strings.TrimSpace(u.AccessToken)
}

// isEncryptedBlob reports whether the blob is the CLI's AES form (base64
// ciphertext) rather than the plaintext JSON compatibility shape.
func isEncryptedBlob(blob []byte) bool {
	return !strings.HasPrefix(strings.TrimSpace(string(blob)), "{")
}

// decodeUserBlob decodes the CLI credential: plaintext JSON or AES-128-CBC
// base64 with key=iv=machineID[:16], strict PKCS7 (reference:
// qoder2api-hub qoder_sign.aes_cbc_decrypt).
func decodeUserBlob(blob []byte, machineID string) (userBlob, error) {
	raw := []byte(strings.TrimSpace(string(blob)))
	if len(raw) == 0 {
		return userBlob{}, errors.New("qoder credential blob is empty")
	}
	var plaintext []byte
	if !isEncryptedBlob(raw) {
		plaintext = raw
	} else {
		key := machineKey(machineID)
		if key == nil {
			return userBlob{}, errors.New("qoder machine_id missing or shorter than 16 bytes")
		}
		ciphertext := make([]byte, base64.StdEncoding.DecodedLen(len(raw)))
		n, err := base64.StdEncoding.Decode(ciphertext, raw)
		if err != nil {
			return userBlob{}, fmt.Errorf("decode qoder credential: %w", err)
		}
		ciphertext = ciphertext[:n]
		if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
			return userBlob{}, errors.New("qoder credential ciphertext not block aligned")
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return userBlob{}, err
		}
		plaintext = make([]byte, len(ciphertext))
		cipher.NewCBCDecrypter(block, key).CryptBlocks(plaintext, ciphertext)
		plaintext, err = unpadPKCS7(plaintext, aes.BlockSize)
		if err != nil {
			return userBlob{}, err
		}
	}
	var parsed userBlob
	if err := json.Unmarshal(plaintext, &parsed); err != nil {
		return userBlob{}, fmt.Errorf("decode qoder credential json: %w", err)
	}
	if strings.TrimSpace(parsed.bearerToken()) == "" {
		return userBlob{}, errors.New("qoder credential carries no access token")
	}
	parsed.Raw = raw
	return parsed, nil
}

// encodeUserBlob re-encodes the patched JSON in the original form.
func encodeUserBlob(cred userBlob, machineID string) ([]byte, error) {
	patch := map[string]any{
		"access_token":  cred.AccessToken,
		"refresh_token": cred.RefreshToken,
		"expire_time":   cred.ExpireTime,
	}
	var current map[string]any
	if err := json.Unmarshal(cred.Raw, &current); err != nil {
		// Plaintext blobs round-trip fine; a truncated original still yields
		// a valid JSON rewrite below.
		current = map[string]any{}
	}
	for key, value := range patch {
		current[key] = value
	}
	delete(current, "security_oauth_token")
	raw, err := json.Marshal(current)
	if err != nil {
		return nil, err
	}
	if !isEncryptedBlob(cred.Raw) {
		return raw, nil
	}
	key := machineKey(machineID)
	if key == nil {
		return nil, errors.New("qoder machine_id shorter than 16 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	padded := padPKCS7(raw, aes.BlockSize)
	encrypted := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, key).CryptBlocks(encrypted, padded)
	return []byte(base64.StdEncoding.EncodeToString(encrypted)), nil
}

func machineKey(machineID string) []byte {
	trimmed := strings.TrimSpace(machineID)
	if len(trimmed) < 16 {
		return nil
	}
	return []byte(trimmed[:16])
}

func unpadPKCS7(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 || len(data)%blockSize != 0 {
		return nil, errors.New("invalid pkcs7 payload length")
	}
	padding := int(data[len(data)-1])
	if padding == 0 || padding > blockSize || padding > len(data) {
		return nil, errors.New("invalid pkcs7 padding")
	}
	for _, b := range data[len(data)-padding:] {
		if int(b) != padding {
			return nil, errors.New("invalid pkcs7 padding")
		}
	}
	return data[:len(data)-padding], nil
}

func padPKCS7(data []byte, blockSize int) []byte {
	padding := blockSize - len(data)%blockSize
	pad := make([]byte, padding)
	for i := range pad {
		pad[i] = byte(padding)
	}
	return append(data, pad...)
}

// machineIDFromCredential derives a deterministic machine id for credential
// blobs that ship without one. The CLI keys AES on the stored machine id, so
// only imported plaintext blobs (no native material) hit this path — those
// never need decryption, and the derived value keeps Cosy machine headers
// stable across processes.
func machineIDFromCredential(accountID string) string {
	mac := hmac.New(sha256.New, []byte("cli2api/qoder/machine-id"))
	mac.Write([]byte(accountID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

const (
	nativeTimeout    = 15 * time.Second
	maxResponseBytes = 1 << 16
)

// nativeHTTP returns the HTTP client for direct Qoder control-plane calls,
// honoring the account proxy first and the global proxy_url secret second
// (same precedence as the worker's own outbound requests).
func (c *Client) nativeHTTP(ctx context.Context, account accounts.Account) (*http.Client, error) {
	c.mu.RLock()
	base := c.nativeBase
	c.mu.RUnlock()
	if base == nil {
		base = &http.Client{Timeout: nativeTimeout}
	}
	proxyURL := strings.TrimSpace(account.ProxyURL)
	if proxyURL == "" && c.nativeStore != nil {
		if value, found, err := c.nativeStore.GetSecret(ctx, "proxy_url"); err != nil {
			return nil, fmt.Errorf("load global proxy setting: %w", err)
		} else if found {
			proxyURL = strings.TrimSpace(value)
		}
	}
	transport, err := c.transports.Get(proxyURL)
	if err != nil {
		return nil, err
	}
	client := *base
	if transport != nil {
		client.Transport = transport
	}
	return &client, nil
}

// limitRead reads at most maxResponseBytes+1 bytes so oversize responses are
// detectable instead of silently truncated.
func limitRead(r io.Reader) ([]byte, error) {
	limited := io.LimitReader(r, maxResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("response exceeds %d bytes", maxResponseBytes)
	}
	return body, nil
}
