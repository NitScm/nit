// Package audittest is a conformance suite every audit.Sink must pass.
//
// A sink is where the audit trail goes to be read by somebody who was not
// there: an auditor asking why a push was refused, an incident responder asking
// who touched a subtree. What that reader needs is not "most of" a decision.
// A sink that drops RuleID makes a refusal unexplainable; one that reorders a
// batch turns a sequence of per-path decisions into a set; one that silently
// discards a record with no rule id loses precisely the default-deny cases,
// which are the ones nobody wrote a rule for and the ones worth reading.
//
// So this suite is mostly about fidelity, not about delivery. Delivery is the
// assembly's job — the database write happens first and stays the record of
// truth, and pkg/nitd keeps a slow destination off the request path — and none
// of it helps if what arrives is not what was decided.
package audittest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NitScm/nit/pkg/audit"
)

// Readback returns the records a sink has received, in the order it received
// them.
//
// It waits for at least want of them before giving up, because a sink is
// allowed to be asynchronous — that is the recommended shape for one that talks
// to a network — and a suite that read immediately would test only the
// synchronous ones. An implementation that delivers inline ignores want and
// returns what it has.
//
// Returning fewer than want is not an error to report here: the assertion
// belongs to the case, which knows what the shortfall means.
type Readback func(t *testing.T, want int) []audit.Record

// Factory builds a fresh sink for one case, along with the way to read back
// what reached it.
type Factory func(t *testing.T) (audit.Sink, Readback)

// Run executes the whole suite against an implementation.
func Run(t *testing.T, newSink Factory) {
	t.Helper()

	t.Run("EveryFieldOfADecisionSurvives", func(t *testing.T) { testFidelity(t, newSink) })
	t.Run("ABatchArrivesWholeAndInOrder", func(t *testing.T) { testBatchOrder(t, newSink) })
	t.Run("ADefaultDenyIsNotDropped", func(t *testing.T) { testDefaultDeny(t, newSink) })
	t.Run("EmittingNothingIsNotAnError", func(t *testing.T) { testEmptyEmit(t, newSink) })
	t.Run("TheCallersSliceIsNotRetained", func(t *testing.T) { testNoAliasing(t, newSink) })
	t.Run("AwkwardTextSurvives", func(t *testing.T) { testAwkwardText(t, newSink) })
	t.Run("ConcurrentEmitsAllArrive", func(t *testing.T) { testConcurrentEmit(t, newSink) })
}

// A record is the only account of a decision that leaves this deployment.
// Every field of it answers a question an auditor asks, so the test is field by
// field rather than "a record arrived".
func testFidelity(t *testing.T, newSink Factory) {
	sink, readback := newSink(t)

	want := audit.Record{
		OccurredAt:    time.Date(2026, 3, 14, 9, 26, 53, 0, time.UTC),
		Actor:         "maya",
		Action:        "push.denied_path",
		Repository:    "payments",
		Branch:        "main",
		Path:          "config/secrets.yaml",
		Effect:        "deny",
		Reason:        "rule",
		RuleID:        "no-secrets-outside-security",
		Guard:         "requires_review",
		PolicyVersion: "sha256:1e9b2c",
		RequestID:     "01J8Z9QN4T0000000000000000",
		TaskID:        "task-7",
	}

	emit(t, sink, want)

	got := one(t, readback)

	for _, difference := range differences(want, got) {
		t.Errorf("%s", difference)
	}
}

// A push produces one record per path, and their order is the order the policy
// reached them. A reader who sees them shuffled cannot tell which denial came
// first, which is the whole shape of "where did this stop".
func testBatchOrder(t *testing.T, newSink Factory) {
	sink, readback := newSink(t)

	const count = 5

	batch := make([]audit.Record, 0, count)
	for i := range count {
		batch = append(batch, audit.Record{
			OccurredAt: time.Date(2026, 3, 14, 9, 26, 53, 0, time.UTC),
			Actor:      "maya",
			Action:     "push.denied_path",
			Repository: "payments",
			Path:       fmt.Sprintf("src/file-%d.go", i),
			Effect:     "deny",
		})
	}

	emit(t, sink, batch...)

	got := readback(t, count)
	if len(got) != count {
		t.Fatalf("received %d records, want the whole batch of %d", len(got), count)
	}

	for i := range got {
		if want := batch[i].Path; got[i].Path != want {
			t.Errorf("record %d is %q, want %q: the batch arrived out of order",
				i, got[i].Path, want)
		}
	}
}

// Nothing matched, so the default deny applied. RuleID is empty because no
// refusal anybody wrote produced this — it is a gap in the bundle, and it is
// the record most worth exporting. A sink that treats an empty rule id as an
// incomplete record loses exactly these.
func testDefaultDeny(t *testing.T, newSink Factory) {
	sink, readback := newSink(t)

	want := audit.Record{
		OccurredAt: time.Date(2026, 3, 14, 9, 26, 53, 0, time.UTC),
		Actor:      "maya",
		Action:     "push.denied_path",
		Repository: "payments",
		Branch:     "main",
		Path:       "vendor/new-thing/go.mod",
		Effect:     "deny",
		Reason:     "default",
	}

	emit(t, sink, want)

	got := one(t, readback)

	if got.RuleID != "" {
		t.Errorf("RuleID = %q, want it left empty rather than filled in", got.RuleID)
	}

	for _, difference := range differences(want, got) {
		t.Errorf("%s", difference)
	}
}

// The recorder calls Emit with whatever it built, and a batch can legitimately
// be empty when every record in it was nil. That is not a condition a
// destination should hear about, let alone one that should produce an error a
// caller logs.
func testEmptyEmit(t *testing.T, newSink Factory) {
	sink, readback := newSink(t)

	if err := sink.Emit(context.Background()); err != nil {
		t.Fatalf("Emit with no records: %v", err)
	}

	if got := readback(t, 0); len(got) != 0 {
		t.Errorf("received %d records from an empty emit", len(got))
	}
}

// A sink that keeps the slice it was handed shares memory with a caller that is
// free to reuse it. The corruption that follows is invisible in the sink's own
// tests and shows up as records attributed to the wrong person.
func testNoAliasing(t *testing.T, newSink Factory) {
	sink, readback := newSink(t)

	batch := []audit.Record{{
		OccurredAt: time.Date(2026, 3, 14, 9, 26, 53, 0, time.UTC),
		Actor:      "maya",
		Action:     "push.accepted",
		Repository: "payments",
	}}

	emit(t, sink, batch...)

	batch[0].Actor = "someone-else"
	batch[0].Action = "push.denied_path"

	got := one(t, readback)

	if got.Actor != "maya" {
		t.Errorf("Actor = %q, want maya: the sink kept the caller's slice", got.Actor)
	}
	if got.Action != "push.accepted" {
		t.Errorf("Action = %q, want push.accepted: the sink kept the caller's slice", got.Action)
	}
}

// Paths and identities are whatever the repository and the directory contain.
// A sink that mangles them is one an auditor cannot search.
func testAwkwardText(t *testing.T, newSink Factory) {
	sink, readback := newSink(t)

	want := audit.Record{
		OccurredAt: time.Date(2026, 3, 14, 9, 26, 53, 0, time.UTC),
		Actor:      "Maÿa O'Brien",
		Action:     "push.denied_path",
		Repository: "paiements-clés",
		Branch:     "feature/naïve-fix",
		Path:       `docs/"quoted"/日本語/a,b;c.md`,
		Effect:     "deny",
		Reason:     "rule",
		RuleID:     "règle-1",
	}

	emit(t, sink, want)

	for _, difference := range differences(want, one(t, readback)) {
		t.Errorf("%s", difference)
	}
}

// One sink is shared by every request a server handles. An implementation that
// is not safe to call from several goroutines does not fail visibly; it loses
// records, or the race detector finds it long after the deployment did.
func testConcurrentEmit(t *testing.T, newSink Factory) {
	sink, readback := newSink(t)

	const writers = 8

	var wg sync.WaitGroup

	for i := range writers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			_ = sink.Emit(context.Background(), audit.Record{
				OccurredAt: time.Date(2026, 3, 14, 9, 26, 53, 0, time.UTC),
				Actor:      fmt.Sprintf("user-%d", i),
				Action:     "push.accepted",
				Repository: "payments",
			})
		}()
	}

	wg.Wait()

	got := readback(t, writers)
	if len(got) != writers {
		t.Fatalf("received %d records, want %d: concurrent emits were lost", len(got), writers)
	}

	// Order across goroutines is nobody's promise. Presence is.
	seen := map[string]bool{}
	for _, record := range got {
		seen[record.Actor] = true
	}

	for i := range writers {
		if actor := fmt.Sprintf("user-%d", i); !seen[actor] {
			t.Errorf("%s never arrived", actor)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func emit(t *testing.T, sink audit.Sink, records ...audit.Record) {
	t.Helper()

	if err := sink.Emit(context.Background(), records...); err != nil {
		t.Fatalf("Emit: %v", err)
	}
}

func one(t *testing.T, readback Readback) audit.Record {
	t.Helper()

	got := readback(t, 1)
	if len(got) != 1 {
		t.Fatalf("received %d records, want exactly 1", len(got))
	}

	return got[0]
}

// differences names the fields that did not survive, rather than printing two
// structs and leaving the reader to find the one that moved.
//
// Time is compared to the second. A destination that renders RFC3339 without a
// fractional part is a legitimate sink and not one this suite should fail; a
// destination that loses the second is not.
func differences(want, got audit.Record) []string {
	var out []string

	if !want.OccurredAt.Truncate(time.Second).Equal(got.OccurredAt.Truncate(time.Second)) {
		out = append(out, fmt.Sprintf("OccurredAt = %s, want %s",
			got.OccurredAt.Format(time.RFC3339Nano), want.OccurredAt.Format(time.RFC3339Nano)))
	}

	fields := []struct {
		name      string
		want, got string
	}{
		{"Actor", want.Actor, got.Actor},
		{"Action", want.Action, got.Action},
		{"Repository", want.Repository, got.Repository},
		{"Branch", want.Branch, got.Branch},
		{"Path", want.Path, got.Path},
		{"Effect", want.Effect, got.Effect},
		{"Reason", want.Reason, got.Reason},
		{"RuleID", want.RuleID, got.RuleID},
		{"Guard", want.Guard, got.Guard},
		{"PolicyVersion", want.PolicyVersion, got.PolicyVersion},
		{"RequestID", want.RequestID, got.RequestID},
		{"TaskID", want.TaskID, got.TaskID},
	}

	for _, field := range fields {
		if field.want != field.got {
			out = append(out, fmt.Sprintf("%s = %q, want %q", field.name, field.got, field.want))
		}
	}

	return out
}

// Poll is the readback helper an asynchronous sink can build on, so every such
// implementation does not invent its own deadline.
//
// It calls read until it reports at least want records or the deadline passes,
// and returns whatever it has either way — a shortfall is the case's to judge.
func Poll(t *testing.T, want int, read func() []audit.Record) []audit.Record {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)

	for {
		got := read()
		if len(got) >= want || time.Now().After(deadline) {
			return got
		}

		time.Sleep(time.Millisecond)
	}
}

// Describe is what an implementation's own test can print when the suite fails
// in a way that depends on how the sink was configured.
func Describe(records []audit.Record) string {
	var out strings.Builder

	for i, record := range records {
		fmt.Fprintf(&out, "%d: %s %s %s %s\n", i, record.Action, record.Repository, record.Path, record.Effect)
	}

	return out.String()
}
