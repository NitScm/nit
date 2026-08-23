// Package synctoken mints and verifies the opaque sync tokens clients carry.
//
// A sync token names the upstream commit whose filtered projection produced a
// workspace's current state (see docs/PROTOCOL.md §1). Clients store it and
// send it back; only the server interprets it.
//
// It is signed, and that is not decoration. The token is the client's claim
// about which base its patch was computed against, and the server applies the
// patch on top of whatever it names. An unsigned token would let a client claim
// any base it liked — including one whose projection it was never entitled to
// see — and have the server apply a patch there.
//
// Signing also lets the token stay stateless and self-describing while staying
// opaque by contract: clients must treat it as a cookie, which leaves the
// server free to change what it contains.
package synctoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/NitScm/nit/pkg/protocol"
	"github.com/NitScm/nit/pkg/store"
)

// version prefixes every token so the format can change without ambiguity.
const version = "st1"

// MinKeyBytes is the shortest signing key accepted. A short key makes the
// signature forgeable, and a forgeable sync token is a way to have the server
// apply a patch on a base the client chose.
const MinKeyBytes = 32

var (
	// ErrMalformed: the token is not a sync token at all.
	ErrMalformed = errors.New("synctoken: malformed token")

	// ErrBadSignature: the token was tampered with, or signed with another key.
	ErrBadSignature = errors.New("synctoken: bad signature")
)

// Payload is what a token carries.
type Payload struct {
	Workspace      store.ID `json:"w"`
	Repository     store.ID `json:"r"`
	Branch         string   `json:"b"`
	UpstreamCommit string   `json:"c"`

	// PolicyVersion is the bundle that produced the projection. A rule change
	// can widen or narrow what a workspace should hold, so a token minted under
	// an older bundle may call for a resynchronization rather than an
	// incremental diff.
	PolicyVersion string `json:"p"`

	IssuedAt int64 `json:"t"`
}

// Signer mints and verifies tokens.
type Signer struct {
	key []byte
}

// NewSigner returns a signer over a secret key.
func NewSigner(key []byte) (*Signer, error) {
	if len(key) < MinKeyBytes {
		return nil, fmt.Errorf("synctoken: key is %d bytes, need at least %d", len(key), MinKeyBytes)
	}

	return &Signer{key: append([]byte(nil), key...)}, nil
}

// Sign returns the token for a payload.
func (s *Signer) Sign(p Payload) (protocol.SyncToken, error) {
	if p.IssuedAt == 0 {
		p.IssuedAt = time.Now().UTC().Unix()
	}

	body, err := json.Marshal(p)
	if err != nil {
		return "", err
	}

	encoded := base64.RawURLEncoding.EncodeToString(body)
	signed := version + "." + encoded

	return protocol.SyncToken(signed + "." + s.mac(signed)), nil
}

// Verify checks a token's signature and returns its payload.
func (s *Signer) Verify(token protocol.SyncToken) (Payload, error) {
	parts := strings.Split(string(token), ".")
	if len(parts) != 3 || parts[0] != version {
		return Payload{}, ErrMalformed
	}

	// hmac.Equal, not ==: a byte-by-byte comparison that stops at the first
	// difference leaks how much of a forged signature was correct.
	if !hmac.Equal([]byte(parts[2]), []byte(s.mac(parts[0]+"."+parts[1]))) {
		return Payload{}, ErrBadSignature
	}

	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Payload{}, ErrMalformed
	}

	var p Payload
	if err := json.Unmarshal(body, &p); err != nil {
		return Payload{}, ErrMalformed
	}

	return p, nil
}

func (s *Signer) mac(message string) string {
	h := hmac.New(sha256.New, s.key)
	h.Write([]byte(message))

	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// Matches reports whether a payload describes the workspace, repository and
// branch of the request presenting it.
//
// A signature only proves the server minted the token; it says nothing about
// who is presenting it or where. Without this check, a token issued for one
// branch would be accepted as the base for a push to another.
func (p Payload) Matches(workspace, repository store.ID, branch string) bool {
	return p.Workspace == workspace && p.Repository == repository && p.Branch == branch
}
