package qoder

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
)

// identityTTL bounds a cached chat credential. Runtime fields carry no server
// expiry, but an operator edit (region flip, refreshed blob) must not keep
// serving a stale pair forever; a modest TTL bounds that window.
const identityTTL = 10 * time.Minute

// identityEntry is one cached chat credential set: the account row and the
// decoded credential it was last resolved from. The signed identity is
// rebuilt per attempt (fresh runtime fields), so the cache only spares the
// store round-trip within the TTL. A refresh replaces the entry wholesale.
type identityEntry struct {
	account   accounts.Account
	cred      userBlob
	fetchedAt time.Time
}

// identityCache memoizes resolved chat credentials per account. Because the
// signed identity is rebuilt per attempt, a refreshed token automatically
// produces a fresh Cosy-Key on the next chat — the cache never pins runtime
// fields, only the credential pair they derive from.
type identityCache struct {
	mu   sync.Mutex
	byID map[string]identityEntry
}

func newIdentityCache() *identityCache {
	return &identityCache{byID: map[string]identityEntry{}}
}

func (c *identityCache) get(accountID string) (identityEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.byID[accountID]
	if !ok || time.Since(entry.fetchedAt) > identityTTL {
		return identityEntry{}, false
	}
	return entry, true
}

func (c *identityCache) put(accountID string, entry identityEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry.fetchedAt = time.Now()
	c.byID[accountID] = entry
}

// chatCredentialFor resolves the account, credential, and a freshly built
// chat identity for one chat. Within the TTL the account+credential pair is
// served from cache so repeated chats skip the store round-trip; runtime
// fields regenerate every time, so each attempt signs with a new Cosy-Key.
func (c *Client) chatCredentialFor(ctx context.Context, accountID string) (accounts.Account, userBlob, inferIdentity, error) {
	entry, ok := c.identityCache().get(accountID)
	if !ok {
		account, cred, err := c.resolvedCredential(ctx, accountID)
		if err != nil {
			return accounts.Account{}, userBlob{}, inferIdentity{}, err
		}
		entry = identityEntry{account: account, cred: cred}
		c.identityCache().put(accountID, entry)
	}
	identity, err := buildChatIdentity(entry.account, entry.cred)
	if err != nil {
		return accounts.Account{}, userBlob{}, inferIdentity{}, err
	}
	return entry.account, entry.cred, identity, nil
}

// buildChatIdentity assembles the infer identity from one decoded credential.
// Organization fields are not yet modeled on the account row; they arrive
// with the Task 8/9 credential schema and stay empty until then.
// DataPolicyAgreed mirrors the CLI's agreed-on-login default.
func buildChatIdentity(account accounts.Account, cred userBlob) (inferIdentity, error) {
	uid := strings.TrimSpace(cred.UID)
	if uid == "" {
		uid = strings.TrimSpace(account.RemoteUID)
	}
	if uid == "" {
		return inferIdentity{}, errors.New("qoder credential carries no uid")
	}
	infer := inferIdentity{
		MachineID:        cred.machineID(),
		UID:              uid,
		Version:          qoderCLIVersion,
		DataPolicyAgreed: true,
		Region:           account.ProviderRegion,
		// PlainBody stays false: Task 8 recordings (testdata/native/
		// prepare_cn.json) show both regions send Encode=1 with an encoded
		// body, matching the WASM path byte for byte.
	}
	fields, err := generateRuntimeFields(runtimeFieldsInput{
		UID:              infer.UID,
		OrganizationTags: []string{},
		DataPolicyAgreed: infer.DataPolicyAgreed,
	}, nil)
	if err != nil {
		return inferIdentity{}, fmt.Errorf("qoder chat identity: %w", err)
	}
	infer.Info = fields.EncryptUserInfo
	infer.Key = fields.Key
	return infer, nil
}
