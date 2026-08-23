package synctoken

import (
	"strings"
	"testing"

	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/protocol"
)

func rootOverKey(t *testing.T) *Root {
	t.Helper()

	root, err := NewRoot([]byte(strings.Repeat("k", MinKeyBytes)))
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}

	return root
}

// The property the derivation exists for: a token minted for one tenant must
// not verify for another.
//
// Without it, a bug in whatever resolves the tenant is not a rejected request —
// it is a patch applied on a base the client was never entitled to, on somebody
// else's repository.
func TestATokenDoesNotVerifyForAnotherTenant(t *testing.T) {
	root := rootOverKey(t)

	acme, err := root.For("acme")
	if err != nil {
		t.Fatalf("For(acme): %v", err)
	}

	globex, err := root.For("globex")
	if err != nil {
		t.Fatalf("For(globex): %v", err)
	}

	token, err := acme.Sign(samplePayload())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	if _, err := acme.Verify(token); err != nil {
		t.Fatalf("the minting tenant could not verify its own token: %v", err)
	}

	if _, err := globex.Verify(token); err == nil {
		t.Fatal("another tenant verified a token it did not mint")
	}
}

// Two tenants must not share a subkey, whatever the root.
func TestEachTenantGetsADistinctKey(t *testing.T) {
	root := rootOverKey(t)

	seen := map[string]policy.TenantID{}

	for _, tenant := range []policy.TenantID{"default", "acme", "globex", "a", "b"} {
		signer, err := root.For(tenant)
		if err != nil {
			t.Fatalf("For(%s): %v", tenant, err)
		}

		key := string(signer.key)

		if other, ok := seen[key]; ok {
			t.Errorf("%s and %s derive the same key", tenant, other)
		}

		seen[key] = tenant
	}
}

// Derivation has to be deterministic, or a restart invalidates every token a
// deployment has issued.
func TestDerivationIsStableAcrossRoots(t *testing.T) {
	first, err := rootOverKey(t).For("acme")
	if err != nil {
		t.Fatalf("For: %v", err)
	}

	second, err := rootOverKey(t).For("acme")
	if err != nil {
		t.Fatalf("For: %v", err)
	}

	token, err := first.Sign(samplePayload())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	if _, err := second.Verify(token); err != nil {
		t.Errorf("a signer rebuilt from the same root rejected its own token: %v", err)
	}
}

// A signer needs a tenant, and there is no value meaning "all of them".
func TestASignerNeedsATenant(t *testing.T) {
	if _, err := rootOverKey(t).For(""); err == nil {
		t.Fatal("a signer was derived for no tenant")
	}
}

// Tokens issued before per-tenant keys still verify — but only for the default
// tenant, so the transition cannot outlive the single-tenant world it came
// from. The moment a deployment has a second tenant, a legacy token is refused
// rather than accepted everywhere, which is the hole this closes.
func TestLegacyTokensAreAcceptedOnlyByTheDefaultTenant(t *testing.T) {
	key := []byte(strings.Repeat("k", MinKeyBytes))

	// A token as the previous version minted them: signed with the root key.
	legacy := &Signer{key: key, root: key, tenant: policy.DefaultTenant}

	body, err := legacy.Sign(samplePayload())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// Rewritten to the old version prefix, which is what identifies it.
	old := strings.Replace(string(body), version+".", legacyVersion+".", 1)
	parts := strings.Split(old, ".")
	old = parts[0] + "." + parts[1] + "." + macWith(key, parts[0]+"."+parts[1])

	root, err := NewRoot(key)
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}

	def, err := root.For(policy.DefaultTenant)
	if err != nil {
		t.Fatalf("For: %v", err)
	}

	if _, err := def.Verify(protocol.SyncToken(old)); err != nil {
		t.Errorf("the default tenant refused a token from before the change: %v", err)
	}

	other, err := root.For("acme")
	if err != nil {
		t.Fatalf("For: %v", err)
	}

	if _, err := other.Verify(protocol.SyncToken(old)); err == nil {
		t.Error("a second tenant accepted a legacy token, which is the hole this closes")
	}
}

// A token from another deployment's root must not verify here, tenant or no
// tenant. The derivation adds a layer; it must not remove one.
func TestAnotherRootStillDoesNotVerify(t *testing.T) {
	mine, err := rootOverKey(t).For("acme")
	if err != nil {
		t.Fatalf("For: %v", err)
	}

	stranger, err := NewRoot([]byte(strings.Repeat("x", MinKeyBytes)))
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}

	theirs, err := stranger.For("acme")
	if err != nil {
		t.Fatalf("For: %v", err)
	}

	token, err := theirs.Sign(samplePayload())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	if _, err := mine.Verify(token); err == nil {
		t.Fatal("a token signed under another deployment's root verified")
	}
}
