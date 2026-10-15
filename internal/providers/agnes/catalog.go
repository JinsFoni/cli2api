package agnes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// CatalogEntry is one model from GET /models. The platform exposes the plain
// OpenAI shape; only the text models are advertised through this gateway —
// image/video models live behind separate generation endpoints.
type CatalogEntry struct {
	ID string `json:"id"`
}

type catalogEnvelope struct {
	Data []CatalogEntry `json:"data"`
}

const catalogTTL = 3 * time.Hour

// Models lists the account's models with a TTL cache, mirroring the Command
// adapter. Native and public ids are the platform's canonical ids; reasoning
// support is deliberately not claimed because the catalog does not declare it.
func (c *Client) Models(ctx context.Context, accountID string) ([]providers.ModelInfo, error) {
	c.mu.Lock()
	if len(c.catalog) > 0 && time.Since(c.catalogAt) < catalogTTL {
		models := c.catalog
		c.mu.Unlock()
		return models, nil
	}
	c.mu.Unlock()

	credential, err := c.credential(ctx, accountID)
	if err != nil {
		return nil, err
	}
	client, err := c.httpClient(ctx, accountID)
	if err != nil {
		return nil, err
	}
	models, err := c.fetchCatalog(ctx, client, credential)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.catalog = models
	c.catalogAt = time.Now()
	c.mu.Unlock()
	return models, nil
}

// ParseCatalogJSON maps the /models payload onto the provider ModelInfo
// surface. Non-text ids (agnes-image-…, agnes-video-…) are skipped: this
// gateway serves chat only.
func ParseCatalogJSON(raw []byte) ([]providers.ModelInfo, error) {
	var env catalogEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if len(env.Data) == 0 {
		return nil, fmt.Errorf("agnes catalog empty")
	}
	models := make([]providers.ModelInfo, 0, len(env.Data))
	seen := map[string]struct{}{}
	for _, entry := range env.Data {
		id := strings.TrimSpace(entry.ID)
		if id == "" || !isTextModel(id) {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		models = append(models, providers.ModelInfo{
			NativeModel: id,
			PublicModel: id,
			DisplayName: id,
			Capabilities: providers.ModelCapabilities{
				Tools: true,
			},
		})
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("agnes catalog produced no text models")
	}
	return models, nil
}

func (c *Client) fetchCatalog(ctx context.Context, client *http.Client, credential Credential) ([]providers.ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpointBase(credential)+PathModels, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+credential.APIKey)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return nil, newProviderError(resp.StatusCode, string(body))
	}
	return ParseCatalogJSON(body)
}

// isTextModel reports whether a catalog id is a chat-servable text model.
// Image and video models have their own endpoints and are out of scope.
func isTextModel(id string) bool {
	lower := strings.ToLower(id)
	return !strings.HasPrefix(lower, "agnes-image-") && !strings.HasPrefix(lower, "agnes-video-")
}
