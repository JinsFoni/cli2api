package agnes

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
	proxyutil "github.com/caigee-cmd/cli2api/internal/proxy"
)

// Store is the persistence surface the adapter needs. It mirrors the Command
// adapter: an in-process provider talks to the account store through this
// narrow interface and never imports internal/store.
type Store interface {
	Get(ctx context.Context, id string) (accounts.Account, error)
	LoadCredentialPayload(ctx context.Context, accountID string) (string, []byte, error)
	SaveCredentialPayload(ctx context.Context, accountID, format string, payload []byte) error
	Observe(ctx context.Context, id, remoteUID, status, lastError, lastKind string) error
}

// SecretReader is optional. Missing it means no global proxy, not an error.
type SecretReader interface {
	GetSecret(context.Context, string) (string, bool, error)
}

type Client struct {
	store Store
	http  *http.Client

	transports proxyutil.TransportCache

	baseURL string

	mu        sync.Mutex
	catalog   []providers.ModelInfo
	catalogAt time.Time
}

func NewClient(store Store) *Client {
	return &Client{
		store: store,
		http: &http.Client{
			Timeout: 120 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		baseURL: BaseURL,
	}
}

// SetBase overrides the API origin (tests point this at a local server).
func (c *Client) SetBase(base string) {
	if strings.TrimSpace(base) != "" {
		c.baseURL = strings.TrimRight(strings.TrimSpace(base), "/")
	}
}

func (c *Client) effectiveProxy(ctx context.Context, accountID string) (string, error) {
	if c.store == nil {
		return "", nil
	}
	account, err := c.store.Get(ctx, accountID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(account.ProxyURL) != "" {
		return strings.TrimSpace(account.ProxyURL), nil
	}
	if reader, ok := c.store.(SecretReader); ok {
		if global, ok, err := reader.GetSecret(ctx, "proxy_url"); err == nil && ok {
			return strings.TrimSpace(global), nil
		}
	}
	return "", nil
}

func (c *Client) httpClient(ctx context.Context, accountID string) (*http.Client, error) {
	rawProxy, err := c.effectiveProxy(ctx, accountID)
	if err != nil {
		return nil, err
	}
	client := *c.http
	transport, err := c.transports.Get(rawProxy)
	if err != nil {
		return nil, err
	}
	if transport != nil {
		client.Transport = transport
	}
	return &client, nil
}

func (c *Client) credential(ctx context.Context, accountID string) (Credential, error) {
	_, payload, err := c.store.LoadCredentialPayload(ctx, accountID)
	if err != nil {
		return Credential{}, err
	}
	cred, err := DecodeCredential(payload)
	if err != nil {
		return Credential{}, err
	}
	if !cred.Ready() {
		return cred, fmt.Errorf("agnes credential incomplete; paste an API key")
	}
	return cred, nil
}

// endpointBase is the base used for outbound calls: the credential's origin
// when set, otherwise the client default (BaseURL, or a SetBase override in
// tests).
func (c *Client) endpointBase(credential Credential) string {
	if strings.TrimSpace(credential.BaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(credential.BaseURL), "/")
	}
	if strings.TrimSpace(c.baseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(c.baseURL), "/")
	}
	return BaseURL
}

// Probe validates the key with a GET /models call. Agnes has no profile
// endpoint, so the catalog route doubles as the readiness check; the returned
// UID falls back to the credential itself because there is nothing to fetch.
func (c *Client) Probe(ctx context.Context, accountID string) (providers.AccountHealth, error) {
	credential, err := c.credential(ctx, accountID)
	if err != nil {
		return providers.AccountHealth{LastError: err.Error()}, nil
	}
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return providers.AccountHealth{LastError: err.Error()}, nil
	}
	if err := c.checkKey(ctx, client, credential); err != nil {
		if providerErr, ok := err.(*providers.Error); ok {
			return providers.AccountHealth{LastError: providerErr.Message}, nil
		}
		return providers.AccountHealth{LastError: err.Error()}, nil
	}
	_ = c.store.Observe(ctx, accountID, "agnes", "ready", "", "")
	return providers.AccountHealth{Ready: true, Hot: true, UID: "agnes"}, nil
}

func (c *Client) checkKey(ctx context.Context, client *http.Client, credential Credential) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpointBase(credential)+PathModels, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+credential.APIKey)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return newProviderError(resp.StatusCode, string(body))
	}
	return nil
}

// Adapter wires the capability surface. There is no Login (auth is a pasted
// API key) and no check-in; PAT login is surfaced through registry capabilities
// and the existing paste-key tab.
func (c *Client) Adapter() providers.Adapter {
	return providers.Adapter{
		ID:           "agnes",
		Credential:   credentialCodec{},
		Chat:         c,
		Models:       c,
		Classifier:   classifier{},
		ImportExport: importer{},
		Prober:       c,
	}
}

type credentialCodec struct{}

func (credentialCodec) Validate(payload []byte) error { return ValidateCredential(payload) }

type classifier struct{}

func (classifier) Classify(status int, body string) providers.ClassifiedError {
	return Classify(status, body)
}
