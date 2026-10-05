package qoder

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Native login (plan Task 10): PAT exchange and the device flow talk to the
// openapi hosts directly with plain Bearer HTTP — no worker, no WASM. The
// stored blob mirrors the CLI's ~/.qoder*/.auth/user JSON (AES-128-CBC with
// the machine id, already handled by credential.go) so an account can move
// between the CLI and this gateway unchanged.
//
// The PAT exchange response carries the job token pair; the userinfo endpoint
// supplies uid/name; runtime fields (encrypt_user_info + key) are generated
// locally with the same WASM algorithm as the CLI (Task 4) and persisted on
// the blob — Task 8 recordings confirmed the CLI stores the pair the same way
// (testdata/native/credential_cn.json).

// patExchangeClientTimeout bounds one exchange/userinfo call.
const patExchangeClientTimeout = 20 * time.Second

// devicePollTimeout matches the CLI's five-minute login window.
const devicePollTimeout = 5 * time.Minute

type loginNativeClient struct {
	HTTP      *http.Client
	Region    string
	Now       func() time.Time           // injectable clock for tests
	Entropy   io.Reader                  // injectable entropy for runtime fields
	Endpoints map[string]nativeEndpoints // test override; nil uses real hosts
}

// endpointFor resolves the region host, honoring the test override.
func (l loginNativeClient) endpointFor() (nativeEndpoints, bool) {
	if l.Endpoints != nil {
		if e, ok := l.Endpoints[l.Region]; ok {
			return e, true
		}
		return nativeEndpoints{}, false
	}
	return nativeEndpoint(l.Region)
}

// exchangedToken is the parsed jobToken/exchange response.
type exchangedToken struct {
	Token              string
	RefreshToken       string
	ExpireTimeS        int64
	RefreshExpireTimeS int64
}

// ExchangePATForCredential runs the native PAT flow for one account: the
// credential blob lands in the CLI's storage shape and the machine id is
// persisted alongside it.
func (c *Client) ExchangePATForCredential(ctx context.Context, accountID, pat string) (providers.NativeCredentialPair, error) {
	client := c.loginNativeClient(ctx, accountID)
	blob, machineID, err := client.LoginPATNative(ctx, pat)
	if err != nil {
		return providers.NativeCredentialPair{}, err
	}
	// Re-encode as the CLI's AES form keyed by the machine id so the stored
	// blob matches what the CLI itself would write.
	encoded, err := encodeUserBlob(blob, machineID)
	if err != nil {
		return providers.NativeCredentialPair{}, err
	}
	return providers.NativeCredentialPair{UserBlob: encoded, MachineID: machineID}, nil
}

// loginNativeClient builds the native login client for one account, honoring
// the account proxy like the other native paths.
func (c *Client) loginNativeClient(ctx context.Context, accountID string) loginNativeClient {
	region := ""
	var account accounts.Account
	if c.nativeStore != nil {
		if a, err := c.nativeStore.Get(ctx, accountID); err == nil {
			account = a
			region = a.ProviderRegion
		}
	}
	if strings.TrimSpace(region) == "" {
		region = "global"
	}
	httpClient, err := c.nativeHTTP(ctx, account)
	if err != nil || httpClient == nil {
		httpClient = &http.Client{Timeout: patExchangeClientTimeout}
	}
	return loginNativeClient{HTTP: httpClient, Region: region}
}

func (l loginNativeClient) httpClient() *http.Client {
	if l.HTTP != nil {
		return l.HTTP
	}
	return &http.Client{Timeout: patExchangeClientTimeout}
}

func (l loginNativeClient) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// ExchangePAT swaps a pt-* personal access token for a jt-*/jrt-* job token
// pair via POST /api/v1/jobToken/exchange (plain JSON, no COSY signing —
// same shape as the CLI's openApiJsonRequest).
func (l loginNativeClient) ExchangePAT(ctx context.Context, pat string) (exchangedToken, error) {
	endpoint, ok := l.endpointFor()
	if !ok {
		return exchangedToken{}, fmt.Errorf("qoder region %q has no openapi endpoint", l.Region)
	}
	body, err := json.Marshal(map[string]string{"personal_token": pat})
	if err != nil {
		return exchangedToken{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.Base+"/api/v1/jobToken/exchange", strings.NewReader(string(body)))
	if err != nil {
		return exchangedToken{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "qoder-local-proxy")
	resp, err := l.httpClient().Do(req)
	if err != nil {
		return exchangedToken{}, TransportError{Err: err}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return exchangedToken{}, errors.New("qoder PAT rejected (wrong token or wrong region)")
	}
	if resp.StatusCode >= 300 {
		return exchangedToken{}, HTTPStatusError{Op: "pat_exchange", Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	return parseExchangedToken(raw)
}

// parseExchangedToken reads the access/refresh pair the way the CLI does:
// token from ["token","device_token","access_token"], expiry from absolute
// timestamps with an expires_in fallback.
func parseExchangedToken(raw []byte) (exchangedToken, error) {
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return exchangedToken{}, fmt.Errorf("qoder PAT exchange response: %w", err)
	}
	token := firstString(parsed, "token", "device_token", "access_token")
	if token == "" {
		return exchangedToken{}, errors.New("qoder PAT exchange returned no access token")
	}
	out := exchangedToken{
		Token:              token,
		RefreshToken:       firstString(parsed, "refresh_token", "refreshToken"),
		ExpireTimeS:        absoluteUnixSeconds(parsed, "expires_at", "expire_time", "expires_in"),
		RefreshExpireTimeS: absoluteUnixSeconds(parsed, "refresh_token_expires_at", "refresh_token_expire_time", "refresh_token_expires_in"),
	}
	return out, nil
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// absoluteUnixSeconds returns the first key present as unix seconds. Keys
// ending in _in are seconds-from-now (units differ across upstream shapes),
// others are absolute (already seconds or milliseconds).
func absoluteUnixSeconds(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			seconds := int64(n)
			if strings.HasSuffix(k, "_in") {
				seconds += time.Now().Unix()
			} else if seconds > 1<<45 { // milliseconds
				seconds /= 1000
			}
			return seconds
		case string:
			_ = n
		}
	}
	return 0
}

// fetchUserInfo reads uid/name via GET /api/v1/userinfo with the fresh token.
func (l loginNativeClient) fetchUserInfo(ctx context.Context, token string) (uid, name string, err error) {
	endpoint, ok := l.endpointFor()
	if !ok {
		return "", "", fmt.Errorf("qoder region %q has no openapi endpoint", l.Region)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.Base+"/api/v1/userinfo", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "qoder-local-proxy")
	resp, err := l.httpClient().Do(req)
	if err != nil {
		return "", "", TransportError{Err: err}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		return "", "", HTTPStatusError{Op: "userinfo", Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", "", fmt.Errorf("qoder userinfo response: %w", err)
	}
	// The CLI accepts id/user_id/uid; the id field is a device-session id in
	// some shapes, but /api/v1/userinfo returns the real account uid first.
	uid = firstString(parsed, "uid", "user_id", "id")
	if uid == "" {
		return "", "", errors.New("qoder userinfo carries no uid")
	}
	return uid, firstString(parsed, "name"), nil
}

// LoginPATNative exchanges a PAT and returns the complete credential blob in
// the CLI's storage shape (access/refresh pair, identity, runtime fields).
func (l loginNativeClient) LoginPATNative(ctx context.Context, pat string) (userBlob, string, error) {
	exchanged, err := l.ExchangePAT(ctx, pat)
	if err != nil {
		return userBlob{}, "", err
	}
	uid, name, err := l.fetchUserInfo(ctx, exchanged.Token)
	if err != nil {
		return userBlob{}, "", err
	}
	machineID, err := newMachineID()
	if err != nil {
		return userBlob{}, "", err
	}
	fields, err := generateRuntimeFields(runtimeFieldsInput{
		UID:              uid,
		OrganizationTags: []string{},
		DataPolicyAgreed: true,
	}, l.Entropy)
	if err != nil {
		return userBlob{}, "", err
	}
	now := l.now()
	blob := userBlob{
		AccessToken:        exchanged.Token,
		RefreshToken:       exchanged.RefreshToken,
		UID:                uid,
		Name:               name,
		ExpireTime:         exchanged.ExpireTimeS,
		SecurityOauthToken: exchanged.Token,
	}
	if blob.ExpireTime == 0 {
		blob.ExpireTime = now.Add(30 * 24 * time.Hour).Unix()
	}
	out, err := encodeUserBlobFull(blob, fields, pat, exchanged.RefreshExpireTimeS, now)
	if err != nil {
		return userBlob{}, "", err
	}
	return out, machineID, nil
}

// newMachineID mints a random v4 machine id for fresh logins (36 chars).
func newMachineID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("qoder machine id entropy: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return formatCosyUUID(raw), nil
}

// encodeUserBlobFull serializes the CLI's persisted userInfo shape (14 fields
// after a PAT login, per the Task 8 dump) and encrypts it the CLI way.
func encodeUserBlobFull(blob userBlob, fields runtimeFields, pat string, refreshExpireTimeS int64, now time.Time) (userBlob, error) {
	payload := map[string]any{
		"uid":                       blob.UID,
		"name":                      blob.Name,
		"email":                     "",
		"avatar_url":                "",
		"organization_id":           "",
		"organization_name":         "",
		"is_data_policy_modifiable": "",
		"data_policy_agreed":        true,
		"security_oauth_token":      blob.SecurityOauthToken,
		"access_token":              blob.AccessToken,
		"refresh_token":             blob.RefreshToken,
		"expire_time":               blob.ExpireTime,
		"refresh_token_expire_time": refreshExpireTimeS,
		"personal_access_token":     pat,
		"login_method":              "token",
		"login_timestamp":           now.Unix(),
		"encrypt_user_info":         fields.EncryptUserInfo,
		"key":                       fields.Key,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return userBlob{}, err
	}
	decoded, err := decodeUserBlob(raw, "")
	if err != nil {
		return userBlob{}, err
	}
	decoded.Raw = raw
	return decoded, nil
}

// DebugEndpointFor exposes the resolved region host for diagnostics.
func (l loginNativeClient) DebugEndpointFor() (string, bool) {
	e, ok := l.endpointFor()
	return e.Base, ok
}

// DebugLoginNativeClient exposes the native login client for diagnostics.
func (c *Client) DebugLoginNativeClient(ctx context.Context, accountID string) loginNativeClient {
	return c.loginNativeClient(ctx, accountID)
}

// EndpointForPublic reports the resolved openapi host.
func (l loginNativeClient) EndpointForPublic() (string, bool) {
	e, ok := l.endpointFor()
	return e.Base, ok
}

// DebugHTTPClient exposes the native HTTP client for diagnostics.
func (c *Client) DebugHTTPClient(ctx context.Context, accountID string) (*http.Client, error) {
	account, err := c.nativeStore.Get(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return c.nativeHTTP(ctx, account)
}

// SignedCatalogRequestForDebug builds the signed catalog request for one account.
func (c *Client) SignedCatalogRequestForDebug(ctx context.Context, accountID string) (*http.Request, error) {
	account, cred, err := c.resolvedCredential(ctx, accountID)
	if err != nil {
		return nil, err
	}
	identity, err := buildChatIdentity(account, cred)
	if err != nil {
		return nil, err
	}
	cc := catalogClient{Region: account.ProviderRegion, Identity: identity}
	return cc.signedCatalogRequest(ctx, nil, nil)
}
