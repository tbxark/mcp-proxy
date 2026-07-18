package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/mark3labs/mcp-go/client/transport"
)

// FileTokenStore persists a single OAuth token to a JSON file, so tokens
// survive proxy restarts and can be shared between a one-off `-authorize`
// run and the long-running daemon.
type FileTokenStore struct {
	path string
	mu   sync.Mutex
}

func NewFileTokenStore(path string) *FileTokenStore {
	return &FileTokenStore{path: path}
}

// oauthTokenPath returns the on-disk path for a server's token file,
// rooted under the user's config dir (~/.config/mcp-proxy/oauth on Linux).
func oauthTokenPath(serverName string) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("failed to determine user config dir: %w", err)
	}
	return filepath.Join(dir, "mcp-proxy", "oauth", serverName+".json"), nil
}

func (s *FileTokenStore) GetToken(ctx context.Context) (*transport.Token, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, transport.ErrNoToken
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read token file %s: %w", s.path, err)
	}
	var token transport.Token
	if err := json.Unmarshal(data, &token); err != nil {
		return nil, fmt.Errorf("failed to parse token file %s: %w", s.path, err)
	}
	return &token, nil
}

func (s *FileTokenStore) SaveToken(ctx context.Context, token *transport.Token) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return fmt.Errorf("failed to create token directory: %w", err)
	}
	data, err := json.MarshalIndent(token, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal token: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("failed to write token file %s: %w", tmp, err)
	}
	return os.Rename(tmp, s.path)
}

// registeredClient is what oauthClientPath persists: the client_id (and
// client_secret, if the provider issued one) obtained via RFC 7591 dynamic
// client registration during a `-authorize` run. RegisterClient only
// mutates the in-memory OAuthHandler of that one-off process, so without
// persisting it here, a freshly-started daemon would build a new
// OAuthHandler with an empty ClientID and every subsequent token refresh
// would silently send client_id="" and be rejected by the provider.
type registeredClient struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret,omitempty"`
}

// oauthClientPath returns the on-disk path for a server's registered OAuth
// client credentials, alongside its token file.
func oauthClientPath(serverName string) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("failed to determine user config dir: %w", err)
	}
	return filepath.Join(dir, "mcp-proxy", "oauth", serverName+".client.json"), nil
}

// loadRegisteredClient returns (clientID, clientSecret, true) if a
// previously dynamically-registered client was persisted for this server,
// or ("", "", false) if none exists yet.
func loadRegisteredClient(serverName string) (string, string, bool, error) {
	path, err := oauthClientPath(serverName)
	if err != nil {
		return "", "", false, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("failed to read client file %s: %w", path, err)
	}
	var rc registeredClient
	if err := json.Unmarshal(data, &rc); err != nil {
		return "", "", false, fmt.Errorf("failed to parse client file %s: %w", path, err)
	}
	return rc.ClientID, rc.ClientSecret, true, nil
}

// saveRegisteredClient persists a dynamically-registered client's
// credentials so future daemon starts (and re-authorizations) reuse the
// same registration instead of getting an empty ClientID.
func saveRegisteredClient(serverName, clientID, clientSecret string) error {
	path, err := oauthClientPath(serverName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("failed to create client directory: %w", err)
	}
	data, err := json.MarshalIndent(registeredClient{ClientID: clientID, ClientSecret: clientSecret}, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal client: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("failed to write client file %s: %w", tmp, err)
	}
	return os.Rename(tmp, path)
}
