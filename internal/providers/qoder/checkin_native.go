package qoder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Native control-plane: direct Bearer HTTP to the Qoder openapi hosts, the
// same shape the worker's checkin module and the CLI's quota hook use. The
// worker stays chat-only; this file owns check-in, quota snapshots, and the
// credential verdict used when no worker is running.

type nativeEndpoints struct {
	base   string
	origin string
}

var nativeRegionEndpoints = map[string]nativeEndpoints{
	"cn":     {base: "https://openapi.qoder.com.cn", origin: "https://qoder.com" + ".cn"},
	"global": {base: "https://openapi.qoder.sh", origin: "https://qoder.com"},
}

func nativeEndpoint(region string) (nativeEndpoints, bool) {
	endpoint, ok := nativeRegionEndpoints[strings.ToLower(strings.TrimSpace(region))]
	return endpoint, ok
}

// endpoint resolves the openapi host for a region, honoring the test override
// first (SetHTTP's seam for the direct control-plane path).
func (c *Client) endpoint(region string) (nativeEndpoints, bool) {
	c.mu.RLock()
	override := c.endpointsOverride
	c.mu.RUnlock()
	if override != nil {
		if endpoint, ok := override[strings.ToLower(strings.TrimSpace(region))]; ok {
			return endpoint, true
		}
	}
	return nativeEndpoint(region)
}

// setEndpoints injects non-production openapi hosts; test seam only.
func (c *Client) setEndpoints(endpoints map[string]nativeEndpoints) {
	c.mu.Lock()
	c.endpointsOverride = endpoints
	c.mu.Unlock()
}

// campaign mirror of the sash list payload.
type nativeCampaign struct {
	CampaignID  string        `json:"campaignId"`
	ActionType  string        `json:"actionType"`
	ClaimStatus string        `json:"claimStatus"`
	Benefit     nativeBenefit `json:"benefit"`
}

type nativeBenefit struct {
	Kind   string   `json:"kind"`
	Amount *float64 `json:"amount"`
}

type nativeCampaignEnvelope struct {
	Campaigns []json.RawMessage `json:"campaigns"`
	Data      json.RawMessage   `json:"data"`
}

func decodeCampaigns(body []byte) ([]nativeCampaign, error) {
	var envelope nativeCampaignEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode campaigns: %w", err)
	}
	raw := envelope.Campaigns
	if len(raw) == 0 && len(envelope.Data) > 0 {
		// The upstream wraps the list either as {data:{campaigns:[...]}} or
		// as a bare {data:[...]} array; the worker accepted both.
		var wrapped struct {
			Campaigns []json.RawMessage `json:"campaigns"`
		}
		if err := json.Unmarshal(envelope.Data, &wrapped); err == nil && len(wrapped.Campaigns) > 0 {
			raw = wrapped.Campaigns
		} else {
			var list []json.RawMessage
			if err := json.Unmarshal(envelope.Data, &list); err == nil {
				raw = list
			}
		}
	}
	items := make([]nativeCampaign, 0, len(raw))
	for _, entry := range raw {
		var campaign nativeCampaign
		if err := json.Unmarshal(entry, &campaign); err != nil {
			return nil, fmt.Errorf("decode campaign: %w", err)
		}
		items = append(items, campaign)
	}
	return items, nil
}

// checkinMachineHeaders mirrors the headers the worker sent (f7d1a97): both
// Cosy machine fields carry the stored machine id.
func checkinMachineHeaders(machineID string) map[string]string {
	if strings.TrimSpace(machineID) == "" {
		return nil
	}
	return map[string]string{
		"Cosy-MachineId":    machineID,
		"Cosy-MachineToken": machineID,
	}
}

func checkinHeaders(token, machineID string, endpoint nativeEndpoints) map[string]string {
	headers := map[string]string{
		"Authorization":   "Bearer " + token,
		"Accept":          "application/json",
		"Content-Type":    "application/json",
		"User-Agent":      "Qoder",
		"Cosy-ClientType": "10",
		"Cosy-Version":    "0.3.4",
		"Origin":          endpoint.origin,
		"Referer":         endpoint.base + "/growth-page/activity-iframe",
	}
	for key, value := range checkinMachineHeaders(machineID) {
		headers[key] = value
	}
	return headers
}

// resolvedCredential loads the stored credential, decoding the user blob.
// decode-failures with a credential present are honest errors: the native
// path has no worker fallback for check-in.
func (c *Client) resolvedCredential(ctx context.Context, accountID string) (accounts.Account, userBlob, error) {
	if c.nativeStore == nil {
		return accounts.Account{}, userBlob{}, errors.New("qoder native store not bound")
	}
	account, err := c.nativeStore.Get(ctx, accountID)
	if err != nil {
		return accounts.Account{}, userBlob{}, fmt.Errorf("load account: %w", err)
	}
	credential, err := c.nativeStore.LoadCredential(ctx, accountID)
	if err != nil {
		return accounts.Account{}, userBlob{}, fmt.Errorf("load credential: %w", err)
	}
	machineID := strings.TrimSpace(credential.MachineID)
	if machineID == "" {
		machineID = machineIDFromCredential(accountID)
	}
	cred, err := decodeUserBlob(credential.UserBlob, machineID)
	if err != nil {
		return accounts.Account{}, userBlob{}, err
	}
	cred.MachineID = machineID
	return account, cred, nil
}

// refreshRoutes picks the refresh endpoint from the refresh-token prefix
// (reference: qoder2api-hub refresh routing).
func refreshRoute(refreshToken string) (string, bool) {
	switch {
	case strings.HasPrefix(refreshToken, "drt-"):
		return "/api/v1/deviceToken/refresh", true
	case strings.HasPrefix(refreshToken, "jrt-"):
		return "/api/v1/jobToken/refresh", true
	default:
		return "", false
	}
}

// refreshAccessToken rotates the token pair over direct HTTP. Only known
// refresh-token families are refreshed; an unknown prefix means the stored
// credential cannot self-renew and the failure is surfaced honestly.
func (c *Client) refreshAccessToken(ctx context.Context, httpClient *http.Client, account accounts.Account, cred userBlob) (userBlob, error) {
	endpoint, ok := c.endpoint(account.ProviderRegion)
	if !ok {
		return userBlob{}, fmt.Errorf("qoder region %q has no openapi endpoint", account.ProviderRegion)
	}
	path, ok := refreshRoute(cred.RefreshToken)
	if !ok {
		return userBlob{}, errors.New("refresh token is not renewable (unknown prefix); re-login required")
	}
	body, _ := json.Marshal(map[string]string{"refresh_token": cred.RefreshToken})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.base+path, strings.NewReader(string(body)))
	if err != nil {
		return userBlob{}, err
	}
	for key, value := range map[string]string{
		"Content-Type": "application/json",
		"Accept":       "application/json",
		"User-Agent":   "Qoder",
	} {
		request.Header.Set(key, value)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return userBlob{}, fmt.Errorf("refresh token: %w", err)
	}
	defer response.Body.Close()
	payload, err := limitRead(response.Body)
	if err != nil {
		return userBlob{}, err
	}
	if response.StatusCode >= 300 {
		return userBlob{}, fmt.Errorf("refresh failed (HTTP %d): %s", response.StatusCode, truncateBody(payload))
	}
	var parsed struct {
		Token       string `json:"token"`
		DeviceToken string `json:"device_token"`
		Refresh     string `json:"refresh_token"`
		ExpiresIn   int64  `json:"expires_in"` // milliseconds
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return userBlob{}, fmt.Errorf("decode refresh response: %w", err)
	}
	token := strings.TrimSpace(parsed.Token)
	if token == "" {
		token = strings.TrimSpace(parsed.DeviceToken)
	}
	if token == "" {
		return userBlob{}, errors.New("refresh returned no token")
	}
	refreshed := cred
	refreshed.AccessToken = token
	refreshed.SecurityOauthToken = ""
	if next := strings.TrimSpace(parsed.Refresh); next != "" {
		refreshed.RefreshToken = next
	}
	if parsed.ExpiresIn > 0 {
		refreshed.ExpireTime = time.Now().UnixMilli() + parsed.ExpiresIn
	}
	refreshed.Raw = cred.Raw
	return refreshed, nil
}

// persistRefreshedCredential patches the stored blob in its original form.
func (c *Client) persistRefreshedCredential(ctx context.Context, account accounts.Account, refreshed userBlob) error {
	blob, err := encodeUserBlob(refreshed, refreshed.machineID())
	if err != nil {
		return fmt.Errorf("re-encode credential: %w", err)
	}
	return c.nativeStore.SaveCredential(ctx, account.ID, account.AuthType, accounts.NativeCredential{
		UserBlob:  blob,
		MachineID: refreshed.machineID(),
	})
}

func truncateBody(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 160 {
		return text[:160]
	}
	return text
}

// Checkin claims the daily credit campaign over direct HTTP. 401 triggers one
// refresh + retry; a failed refresh falls back to re-reading the credential
// once (another process may have rotated the stored blob mid-flight).
func (c *Client) Checkin(ctx context.Context, accountID string) (providers.CheckinResult, error) {
	account, cred, err := c.resolvedCredential(ctx, accountID)
	if err != nil {
		return providers.CheckinResult{}, err
	}
	endpoint, ok := c.endpoint(account.ProviderRegion)
	if !ok {
		return providers.CheckinResult{}, fmt.Errorf("qoder region %q does not support check-in", account.ProviderRegion)
	}
	httpClient, err := c.nativeHTTP(ctx, account)
	if err != nil {
		return providers.CheckinResult{}, err
	}

	result, status, err := c.checkinOnce(ctx, httpClient, account, cred, endpoint)
	if err != nil && status == http.StatusUnauthorized {
		refreshed, refreshErr := c.refreshAccessToken(ctx, httpClient, account, cred)
		if refreshErr != nil {
			// Rotation mitigation: re-read the credential from the store once
			// and retry with the possibly-rotated token.
			if rereadAccount, rereadCred, credErr := c.resolvedCredential(ctx, accountID); credErr == nil {
				if retryResult, _, retryErr := c.checkinOnce(ctx, httpClient, rereadAccount, rereadCred, endpoint); retryErr == nil {
					return retryResult, nil
				}
			}
			return providers.CheckinResult{}, refreshErr
		}
		if persistErr := c.persistRefreshedCredential(ctx, account, refreshed); persistErr != nil {
			return providers.CheckinResult{}, fmt.Errorf("refresh succeeded but save failed: %w", persistErr)
		}
		retryResult, _, retryErr := c.checkinOnce(ctx, httpClient, account, refreshed, endpoint)
		return retryResult, retryErr
	}
	return result, err
}

// checkinOnce runs campaigns-list + claim flow with the given credential.
// The status return distinguishes 401 (refreshable) from final errors.
func (c *Client) checkinOnce(ctx context.Context, httpClient *http.Client, account accounts.Account, cred userBlob, endpoint nativeEndpoints) (providers.CheckinResult, int, error) {
	machineID := cred.machineID()
	items, status, err := c.listCampaigns(ctx, httpClient, endpoint, cred.bearerToken(), machineID)
	if err != nil {
		if status == http.StatusNotFound || status == http.StatusMethodNotAllowed || status == http.StatusGone {
			// Activity platform not deployed (observed on the global region):
			// nothing to claim, not a failure.
			return providers.CheckinResult{Status: "skipped", Message: "签到活动未开放"}, status, nil
		}
		return providers.CheckinResult{}, status, err
	}

	benefits := make([]nativeCampaign, 0, len(items))
	for _, item := range items {
		if item.ActionType == "CLAIM_BENEFIT" && strings.TrimSpace(item.CampaignID) != "" && item.Benefit.Kind == "CREDITS" {
			benefits = append(benefits, item)
		}
	}
	if len(benefits) == 0 {
		return providers.CheckinResult{Status: "skipped", Message: "签到活动未开放"}, status, nil
	}
	claimable := make([]nativeCampaign, 0, len(benefits))
	for _, item := range benefits {
		if item.ClaimStatus == "CLAIMABLE" {
			claimable = append(claimable, item)
		}
	}
	if len(claimable) == 0 {
		for _, item := range benefits {
			if item.ClaimStatus == "CLAIMED" {
				return providers.CheckinResult{Status: "already", Message: "今日已签到"}, status, nil
			}
		}
		return providers.CheckinResult{Status: "skipped", Message: "签到活动未开放"}, status, nil
	}

	confirmed, recovered := 0, 0
	reward := 0.0
	hasReward := false
	for _, campaign := range claimable {
		path := fmt.Sprintf("/sash/api/v1/me/campaigns/%s/claim", strings.TrimSpace(campaign.CampaignID))
		body, claimStatus, err := c.nativeRequest(ctx, httpClient, endpoint, http.MethodPost, path, cred.bearerToken(), machineID)
		if err != nil {
			// A 401 on claim is refreshable one level up; everything else is final.
			return providers.CheckinResult{}, claimStatus, err
		}
		var claim struct {
			Data *struct {
				Status string `json:"status"`
			} `json:"data"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(body, &claim); err != nil {
			return providers.CheckinResult{}, claimStatus, fmt.Errorf("decode claim response: %w", err)
		}
		claimedStatus := claim.Status
		if claim.Data != nil {
			claimedStatus = claim.Data.Status
		}
		if claimedStatus != "CLAIMED" {
			// Re-list once: an unconfirmed claim may still have landed.
			relisted, _, err := c.listCampaigns(ctx, httpClient, endpoint, cred.bearerToken(), machineID)
			landed := false
			if err == nil {
				for _, item := range relisted {
					if item.CampaignID == campaign.CampaignID && item.ClaimStatus == "CLAIMED" {
						landed = true
						break
					}
				}
			}
			if landed {
				recovered++
			} else {
				return providers.CheckinResult{}, claimStatus, fmt.Errorf("claim %s unconfirmed (status %q)", campaign.CampaignID, claimedStatus)
			}
			continue
		}
		confirmed++
		if campaign.Benefit.Amount != nil && *campaign.Benefit.Amount >= 0 {
			hasReward = true
			reward += *campaign.Benefit.Amount
		}
	}
	if confirmed == 0 && recovered > 0 {
		return providers.CheckinResult{Status: "already", Message: "已签到（复查确认）"}, status, nil
	}
	result := providers.CheckinResult{Status: "success", Message: "签到成功"}
	if hasReward {
		result.Message = fmt.Sprintf("签到成功 +%s 积分", trimFloat(reward))
		result.RewardCredits = reward
	}
	return result, status, nil
}

func trimFloat(value float64) string {
	if value == float64(int64(value)) {
		return fmt.Sprintf("%d", int64(value))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", value), "0"), ".")
}

func (c *Client) nativeRequest(ctx context.Context, httpClient *http.Client, endpoint nativeEndpoints, method, path, token, machineID string) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint.base+path, nil)
	if err != nil {
		return nil, 0, err
	}
	for key, value := range checkinHeaders(token, machineID, endpoint) {
		request.Header.Set(key, value)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("check-in request failed: %w", err)
	}
	defer response.Body.Close()
	body, err := limitRead(response.Body)
	if err != nil {
		return nil, response.StatusCode, err
	}
	if response.StatusCode >= 300 {
		return nil, response.StatusCode, fmt.Errorf("check-in request failed (HTTP %d): %s", response.StatusCode, truncateBody(body))
	}
	return body, response.StatusCode, nil
}

func (c *Client) listCampaigns(ctx context.Context, httpClient *http.Client, endpoint nativeEndpoints, token, machineID string) ([]nativeCampaign, int, error) {
	body, status, err := c.nativeRequest(ctx, httpClient, endpoint, http.MethodGet, "/sash/api/v1/me/campaigns", token, machineID)
	if err != nil {
		return nil, status, err
	}
	items, err := decodeCampaigns(body)
	if err != nil {
		return nil, status, err
	}
	return items, status, nil
}

// ---------------------------------------------------------------------------
// Native quota
// ---------------------------------------------------------------------------

// nativeQuota mirrors the CLI's getQuotaUsage payload; workerQuota.snapshot()
// projects it onto the console/routing model unchanged.
type nativeQuota struct {
	UserQuota          *workerQuotaBlock `json:"userQuota"`
	AddOnQuota         *workerQuotaBlock `json:"addOnQuota"`
	OrgResourcePackage *workerQuotaBlock `json:"orgResourcePackage"`
	IsQuotaExceeded    bool              `json:"isQuotaExceeded"`
	FetchedAt          string            `json:"fetchedAt"`
}

// fetchNativeQuota pulls the three-bucket snapshot over direct HTTP.
func (c *Client) fetchNativeQuota(ctx context.Context, httpClient *http.Client, account accounts.Account, cred userBlob) (*workerQuota, error) {
	endpoint, ok := c.endpoint(account.ProviderRegion)
	if !ok {
		return nil, fmt.Errorf("qoder region %q has no openapi endpoint", account.ProviderRegion)
	}
	body, _, err := c.nativeQuotaRequest(ctx, httpClient, endpoint, cred.bearerToken(), cred.machineID())
	if err != nil {
		return nil, err
	}
	var quota nativeQuota
	if err := json.Unmarshal(body, &quota); err != nil {
		return nil, fmt.Errorf("decode quota usage: %w", err)
	}
	if quota.UserQuota == nil && quota.AddOnQuota == nil && quota.OrgResourcePackage == nil {
		return nil, errors.New("quota usage carries no buckets")
	}
	if quota.FetchedAt == "" {
		quota.FetchedAt = time.Now().UTC().Format(time.RFC3339)
	}
	return &workerQuota{
		UserQuota:          quota.UserQuota,
		AddOnQuota:         quota.AddOnQuota,
		OrgResourcePackage: quota.OrgResourcePackage,
		IsQuotaExceeded:    quota.IsQuotaExceeded,
		FetchedAt:          quota.FetchedAt,
	}, nil
}

func (c *Client) nativeQuotaRequest(ctx context.Context, httpClient *http.Client, endpoint nativeEndpoints, token, machineID string) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.base+"/api/v2/quota/usage", nil)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "Qoder")
	if machineID != "" {
		request.Header.Set("Cosy-MachineId", machineID)
		request.Header.Set("Cosy-MachineToken", machineID)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("quota usage request failed: %w", err)
	}
	defer response.Body.Close()
	body, err := limitRead(response.Body)
	if err != nil {
		return nil, response.StatusCode, err
	}
	if response.StatusCode >= 300 {
		return nil, response.StatusCode, fmt.Errorf("quota usage failed (HTTP %d): %s", response.StatusCode, truncateBody(body))
	}
	return body, response.StatusCode, nil
}
