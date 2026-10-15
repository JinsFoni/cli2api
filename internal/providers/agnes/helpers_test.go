package agnes

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/accounts"
)

// fakeStore is the minimal persistence surface the adapter needs.
type fakeStore struct {
	format   string
	payload  []byte
	observed map[string]string
}

func (f *fakeStore) Get(context.Context, string) (accounts.Account, error) {
	return accounts.Account{ID: "acc-1", Provider: "agnes", ProviderRegion: "global"}, nil
}

func (f *fakeStore) LoadCredentialPayload(context.Context, string) (string, []byte, error) {
	return f.format, f.payload, nil
}

func (f *fakeStore) SaveCredentialPayload(_ context.Context, _ string, format string, payload []byte) error {
	f.format = format
	f.payload = payload
	return nil
}

func (f *fakeStore) Observe(_ context.Context, id, _, status, _, _ string) error {
	if f.observed == nil {
		f.observed = map[string]string{}
	}
	f.observed[id] = status
	return nil
}

// newTestClient returns a Client pointed at a test server with a stored key.
func newTestClient(t *testing.T, srv *httptest.Server) (*Client, *fakeStore) {
	t.Helper()
	cred := Credential{APIKey: "sk-test_key_1234567890", BaseURL: srv.URL}
	payload, err := cred.Encode()
	if err != nil {
		t.Fatalf("encode credential: %v", err)
	}
	store := &fakeStore{format: CredentialFormat, payload: payload}
	client := NewClient(store)
	client.SetBase(srv.URL)
	return client, store
}

// recorder captures the last request headers/body and a per-path hit count.
type recorder struct {
	mu      sync.Mutex
	headers http.Header
	body    string
	hits    map[string]int
}

func newRecorder() *recorder { return &recorder{hits: map[string]int{}} }

func (r *recorder) record(req *http.Request) string {
	raw, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.headers = req.Header.Clone()
	r.body = string(raw)
	r.hits[req.URL.Path]++
	return string(raw)
}

func (r *recorder) count(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits[path]
}

func (r *recorder) header(key string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.headers.Get(key)
}

func contains(s, substr string) bool { return strings.Contains(s, substr) }
