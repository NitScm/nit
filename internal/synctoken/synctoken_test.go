package synctoken

import (
	"errors"
	"strings"
	"testing"

	"github.com/NitScm/nit/pkg/protocol"
	"github.com/NitScm/nit/pkg/store"
)

func storeID(s string) store.ID { return store.ID(s) }

func newTestSigner(t *testing.T) *Signer {
	t.Helper()

	s, err := NewSigner([]byte(strings.Repeat("k", MinKeyBytes)))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

func samplePayload() Payload {
	return Payload{
		Workspace:      "ws-1",
		Repository:     "repo-1",
		Branch:         "feature/checkout",
		UpstreamCommit: "9f2c1ab4e5d6",
		PolicyVersion:  "sha256:abcd",
		IssuedAt:       1_800_000_000,
	}
}

func TestRoundTrip(t *testing.T) {
	s := newTestSigner(t)
	want := samplePayload()

	token, err := s.Sign(want)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	got, err := s.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// The attack the signature exists to stop: a client editing the token to name a
// base of its choosing, so the server applies its patch there.
func TestTamperedPayloadIsRejected(t *testing.T) {
	s := newTestSigner(t)

	token, err := s.Sign(samplePayload())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	forged := samplePayload()
	forged.UpstreamCommit = "0000000000"
	forged.Branch = "main"

	other := newSignerWithKey(t, "attacker-key-that-is-long-enough")

	forgedToken, err := other.Sign(forged)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	if _, err := s.Verify(forgedToken); !errors.Is(err, ErrBadSignature) {
		t.Errorf("got %v, want ErrBadSignature for a token signed with another key", err)
	}

	// Splicing our payload onto the legitimate signature must fail too.
	parts := strings.Split(string(token), ".")
	spliced := protocol.SyncToken(parts[0] + "." + strings.Split(string(forgedToken), ".")[1] + "." + parts[2])

	if _, err := s.Verify(spliced); !errors.Is(err, ErrBadSignature) {
		t.Errorf("got %v, want ErrBadSignature for a spliced token", err)
	}
}

func newSignerWithKey(t *testing.T, key string) *Signer {
	t.Helper()

	for len(key) < MinKeyBytes {
		key += key
	}

	s, err := NewSigner([]byte(key))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

func TestVerifyRejectsMalformed(t *testing.T) {
	s := newTestSigner(t)

	malformed := []protocol.SyncToken{
		"",
		"nonsense",
		"st1.only-two-parts",
		"st2.abc.def",
		"st1.!!!not-base64!!!.sig",
	}

	for _, token := range malformed {
		if _, err := s.Verify(token); err == nil {
			t.Errorf("Verify(%q) accepted a malformed token", token)
		}
	}
}

// A short key makes the signature forgeable, which makes the token forgeable.
func TestNewSignerRejectsShortKey(t *testing.T) {
	if _, err := NewSigner([]byte("too short")); err == nil {
		t.Error("a short signing key must be refused")
	}
}

// A valid signature proves the server minted the token, not that the caller
// presenting it is the one it was minted for.
func TestMatches(t *testing.T) {
	p := samplePayload()

	if !p.Matches("ws-1", "repo-1", "feature/checkout") {
		t.Error("Matches rejected the token's own coordinates")
	}

	cases := [][3]string{
		{"ws-2", "repo-1", "feature/checkout"},
		{"ws-1", "repo-2", "feature/checkout"},
		{"ws-1", "repo-1", "main"},
	}

	for _, c := range cases {
		if p.Matches(storeID(c[0]), storeID(c[1]), c[2]) {
			t.Errorf("Matches accepted %v", c)
		}
	}
}
