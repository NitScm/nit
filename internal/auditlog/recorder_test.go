package auditlog_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/NitScm/nit/internal/auditlog"
	"github.com/NitScm/nit/pkg/audit"
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/store"
)

type fakeStore struct {
	appended []*store.AuditRecord
	fail     error
}

func (f *fakeStore) Append(_ context.Context, records ...*store.AuditRecord) error {
	f.appended = append(f.appended, records...)
	return f.fail
}

func (f *fakeStore) Query(context.Context, store.AuditQuery) ([]*store.AuditRecord, error) {
	return nil, nil
}

type fakeSink struct {
	emitted []audit.Record
	fail    error
}

func (f *fakeSink) Emit(_ context.Context, records ...audit.Record) error {
	f.emitted = append(f.emitted, records...)
	return f.fail
}

func record() *store.AuditRecord {
	return &store.AuditRecord{
		OccurredAt:    time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC),
		ActorLabel:    "maya",
		Action:        "push.denied_path",
		RepositoryID:  "9f2c1ab4-0000-0000-0000-000000000000",
		Branch:        "main",
		Path:          "secrets/prod.env",
		Effect:        policy.EffectDeny,
		RuleID:        "secrets-are-platform-only",
		PolicyVersion: "sha256:f604",
		RequestID:     "req-1",
		TaskID:        "task-1",
	}
}

// The database write is the record of truth and happens whether or not a sink
// is configured.
func TestRecordAlwaysPersists(t *testing.T) {
	st := &fakeStore{}
	auditlog.New(st, nil, nil).Record(context.Background(), "payments", record())

	if len(st.appended) != 1 {
		t.Fatalf("appended %d records, want 1", len(st.appended))
	}
}

func TestRecordForwardsToTheSink(t *testing.T) {
	st, sink := &fakeStore{}, &fakeSink{}

	auditlog.New(st, sink, nil).Record(context.Background(), "payments", record())

	if len(sink.emitted) != 1 {
		t.Fatalf("emitted %d records, want 1", len(sink.emitted))
	}

	got := sink.emitted[0]

	// The repository is the bundle identity the caller supplied, never the
	// database row id: a sink writes somewhere that has never heard of this
	// database.
	if got.Repository != "payments" {
		t.Errorf("Repository = %q, want the bundle id", got.Repository)
	}
	if got.Actor != "maya" || got.RuleID != "secrets-are-platform-only" {
		t.Errorf("record did not carry the decision: %+v", got)
	}
	if !got.OccurredAt.Equal(time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("OccurredAt = %v", got.OccurredAt)
	}
}

// Audit is best-effort at request time. Record returns nothing precisely so
// that no call site can turn a logging failure into a refused push — these
// cases prove neither failure escapes.
func TestNeitherFailureEscapes(t *testing.T) {
	boom := errors.New("unreachable")

	t.Run("store fails", func(t *testing.T) {
		sink := &fakeSink{}
		auditlog.New(&fakeStore{fail: boom}, sink, nil).
			Record(context.Background(), "payments", record())

		// And the sink is still tried: a database outage should not also cost
		// the export.
		if len(sink.emitted) != 1 {
			t.Errorf("emitted %d records after a store failure, want 1", len(sink.emitted))
		}
	})

	t.Run("sink fails", func(t *testing.T) {
		st := &fakeStore{}
		auditlog.New(st, &fakeSink{fail: boom}, nil).
			Record(context.Background(), "payments", record())

		if len(st.appended) != 1 {
			t.Errorf("appended %d records, want the database write to have happened", len(st.appended))
		}
	})
}

// Nothing to record is not an error, and must not reach a sink as an empty
// batch — a destination that logs one line per call would fill with blanks.
func TestRecordIgnoresAnEmptyBatch(t *testing.T) {
	st, sink := &fakeStore{}, &fakeSink{}

	auditlog.New(st, sink, nil).Record(context.Background(), "payments")

	if len(st.appended) != 0 || len(sink.emitted) != 0 {
		t.Errorf("appended %d, emitted %d; want nothing", len(st.appended), len(sink.emitted))
	}
}

func TestRecordSkipsNilRecords(t *testing.T) {
	sink := &fakeSink{}

	auditlog.New(&fakeStore{}, sink, nil).
		Record(context.Background(), "payments", record(), nil, record())

	if len(sink.emitted) != 2 {
		t.Errorf("emitted %d records, want 2", len(sink.emitted))
	}
}
