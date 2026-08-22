package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CredentialsFile is where tokens live, under the user's home directory.
const CredentialsFile = "credentials.json"

// ErrNoCredentials means no token is stored for a server.
var ErrNoCredentials = errors.New("no credentials; run: nit login <server-url>")

// Credentials maps a server URL to its token.
//
// Keyed by server so one machine can talk to several deployments — a staging
// instance and production — without the tokens overwriting each other.
type Credentials struct {
	Tokens map[string]string `json:"tokens"`

	path string
}

// CredentialsPath returns the location of the credentials file.
func CredentialsPath() (string, error) {
	if override := os.Getenv("NIT_CONFIG_DIR"); override != "" {
		return filepath.Join(override, CredentialsFile), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate the home directory: %w", err)
	}

	return filepath.Join(home, ".nit", CredentialsFile), nil
}

// LoadCredentials reads the credentials file, returning an empty set when it
// does not exist yet.
func LoadCredentials() (*Credentials, error) {
	path, err := CredentialsPath()
	if err != nil {
		return nil, err
	}

	creds := &Credentials{Tokens: map[string]string{}, path: path}

	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return creds, nil
	}
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(content, creds); err != nil {
		return nil, fmt.Errorf("%s is corrupt: %w", path, err)
	}
	if creds.Tokens == nil {
		creds.Tokens = map[string]string{}
	}

	creds.path = path

	return creds, nil
}

// Token returns the token stored for a server.
func (c *Credentials) Token(server string) (string, error) {
	token, ok := c.Tokens[normalizeServer(server)]
	if !ok || token == "" {
		return "", fmt.Errorf("%w for %s", ErrNoCredentials, server)
	}

	return token, nil
}

// Set records a token for a server.
func (c *Credentials) Set(server, token string) {
	c.Tokens[normalizeServer(server)] = token
}

// Remove forgets a server's token.
func (c *Credentials) Remove(server string) {
	delete(c.Tokens, normalizeServer(server))
}

// Save writes the credentials file with owner-only permissions.
//
// The file holds bearer tokens. It is written through a temporary file created
// with the final mode, so the secret is never briefly readable by others — a
// window that a plain write followed by a chmod would leave open.
func (c *Credentials) Save() error {
	if c.path == "" {
		path, err := CredentialsPath()
		if err != nil {
			return err
		}
		c.path = path
	}

	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}

	encoded, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}

	tmp := c.path + ".tmp"

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	if _, err := f.Write(append(encoded, '\n')); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}

	return os.Rename(tmp, c.path)
}

// normalizeServer makes "https://nit.example.com/" and "https://nit.example.com"
// the same key, so a trailing slash in one command does not lose the token
// stored by another.
func normalizeServer(server string) string {
	return strings.TrimRight(strings.TrimSpace(server), "/")
}
