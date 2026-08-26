package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/store"
)

// What every backend has to do with the record of which bundles have been in
// force.
//
// The point of the table is that a policy version in a six-month-old audit
// record can be resolved back to the rules. A backend that lost the first
// sighting, or let provenance be attached to a bundle it never loaded, would
// answer that question wrongly rather than not at all — which is worse.

const policyTenant = policy.TenantID("acme")

func testPolicyVersionsRecordAndResolve(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	versions := s.PolicyVersions()

	if err := versions.Record(ctx, &store.PolicyVersion{
		TenantID: policyTenant, Version: "sha256:aaaa", Source: "directory /etc/nit/policy",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, err := versions.ByVersion(ctx, policyTenant, "sha256:aaaa")
	if err != nil {
		t.Fatalf("ByVersion: %v", err)
	}

	if got.Source != "directory /etc/nit/policy" {
		t.Errorf("source = %q", got.Source)
	}

	if got.FirstLoadedAt.IsZero() || got.LastLoadedAt.IsZero() {
		t.Errorf("times are %v / %v", got.FirstLoadedAt, got.LastLoadedAt)
	}

	// A version nothing has loaded is not found, rather than an empty row.
	if _, err := versions.ByVersion(ctx, policyTenant, "sha256:never"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ByVersion of an unknown version: %v", err)
	}
}

// Reloading keeps the first sighting and moves the last.
//
// Either alone answers half the question an auditor asks. First says when a
// rule change took effect; last says whether it is still the rule change in
// effect. A backend that overwrote the first would report a change that
// happened in March as having happened this morning.
func testPolicyVersionsKeepTheFirstSighting(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	versions := s.PolicyVersions()

	record := &store.PolicyVersion{
		TenantID: policyTenant, Version: "sha256:bbbb", Source: "first",
	}

	if err := versions.Record(ctx, record); err != nil {
		t.Fatalf("Record: %v", err)
	}

	first, err := versions.ByVersion(ctx, policyTenant, "sha256:bbbb")
	if err != nil {
		t.Fatalf("ByVersion: %v", err)
	}

	// Far enough apart that a second-resolution clock still separates them.
	time.Sleep(1100 * time.Millisecond)

	if err := versions.Record(ctx, &store.PolicyVersion{
		TenantID: policyTenant, Version: "sha256:bbbb", Source: "somewhere else",
	}); err != nil {
		t.Fatalf("Record again: %v", err)
	}

	after, err := versions.ByVersion(ctx, policyTenant, "sha256:bbbb")
	if err != nil {
		t.Fatalf("ByVersion: %v", err)
	}

	if !after.FirstLoadedAt.Equal(first.FirstLoadedAt) {
		t.Errorf("the first sighting moved: %v then %v", first.FirstLoadedAt, after.FirstLoadedAt)
	}

	if !after.LastLoadedAt.After(first.LastLoadedAt) {
		t.Errorf("the last sighting did not move: %v then %v", first.LastLoadedAt, after.LastLoadedAt)
	}
}

// Provenance is attached afterwards, and only to something that was loaded.
func testPolicyVersionsAttachProvenance(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	versions := s.PolicyVersions()

	if err := versions.Record(ctx, &store.PolicyVersion{
		TenantID: policyTenant, Version: "sha256:cccc", Source: "seam",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if err := versions.Attach(ctx, policyTenant, "sha256:cccc", "refs/heads/main", "9c2b1f"); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	got, err := versions.ByVersion(ctx, policyTenant, "sha256:cccc")
	if err != nil {
		t.Fatalf("ByVersion: %v", err)
	}

	if got.Ref != "refs/heads/main" || got.Commit != "9c2b1f" {
		t.Errorf("provenance = %q / %q", got.Ref, got.Commit)
	}

	// Attaching the same thing twice is not an error. MySQL reports zero rows
	// affected for an UPDATE that changes nothing, which would otherwise look
	// like a missing row.
	if err := versions.Attach(ctx, policyTenant, "sha256:cccc", "refs/heads/main", "9c2b1f"); err != nil {
		t.Errorf("Attach twice: %v", err)
	}

	// A pairing for a bundle this deployment never loaded is a claim about
	// something that never happened here.
	err = versions.Attach(ctx, policyTenant, "sha256:never", "refs/heads/main", "9c2b1f")

	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Attach to an unloaded version: %v", err)
	}
}

// One tenant's history is not another's.
func testPolicyVersionsAreScopedToATenant(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	versions := s.PolicyVersions()

	if err := versions.Record(ctx, &store.PolicyVersion{
		TenantID: policyTenant, Version: "sha256:dddd", Source: "acme",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if err := versions.Record(ctx, &store.PolicyVersion{
		TenantID: "other", Version: "sha256:eeee", Source: "other",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if _, err := versions.ByVersion(ctx, policyTenant, "sha256:eeee"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("one tenant resolved another's version: %v", err)
	}

	listed, err := versions.List(ctx, policyTenant, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	for _, v := range listed {
		if v.TenantID != policyTenant {
			t.Errorf("listing for %s returned %s", policyTenant, v.TenantID)
		}
	}
}

// Newest first, because the question is almost always "what is in force now".
func testPolicyVersionsListNewestFirst(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	versions := s.PolicyVersions()

	for _, version := range []string{"sha256:one", "sha256:two", "sha256:three"} {
		if err := versions.Record(ctx, &store.PolicyVersion{
			TenantID: policyTenant, Version: version, Source: "directory",
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}

		// Enough for a second-resolution clock to separate them.
		time.Sleep(1100 * time.Millisecond)
	}

	listed, err := versions.List(ctx, policyTenant, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(listed) != 3 {
		t.Fatalf("%d versions listed", len(listed))
	}

	if listed[0].Version != "sha256:three" {
		t.Errorf("newest is %q", listed[0].Version)
	}

	for i := 1; i < len(listed); i++ {
		if listed[i].FirstLoadedAt.After(listed[i-1].FirstLoadedAt) {
			t.Errorf("out of order at %d", i)
		}
	}

	if short, err := versions.List(ctx, policyTenant, 2); err != nil || len(short) != 2 {
		t.Errorf("limit ignored: %d, %v", len(short), err)
	}
}
