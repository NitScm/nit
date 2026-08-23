// Package auditlog writes a decision to the database and forwards it to a sink.
//
// It exists so the two properties in pkg/audit are implemented once rather
// than repeated at every call site: the database write always happens first
// and stays the record of truth, and neither write can fail the operation
// being recorded.
package auditlog

import (
	"context"
	"log/slog"

	"github.com/NitScm/nit/pkg/audit"
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/store"
)

// Recorder persists decisions and fans them out.
type Recorder struct {
	store store.AuditStore
	sink  audit.Sink
	log   *slog.Logger
}

// New wires a recorder. A nil sink is fine and means "persist only", which is
// what the community edition does when no export is configured.
func New(s store.AuditStore, sink audit.Sink, log *slog.Logger) *Recorder {
	if log == nil {
		log = slog.Default()
	}

	return &Recorder{store: s, sink: sink, log: log}
}

// Record persists the records and forwards them.
//
// It returns nothing, and that is the point. Audit is best-effort at request
// time: refusing an authorized push because a log write failed would be its
// own kind of outage. Returning an error would leave every call site free to
// propagate it, and one of them eventually would.
//
// The repository is passed rather than read from the records because a record
// carries the database row id, and a sink writes somewhere that has never
// heard of this database. Every batch concerns one operation on one
// repository, so there is nothing to lose by naming it.
func (r *Recorder) Record(ctx context.Context, repository policy.RepoID, records ...*store.AuditRecord) {
	if len(records) == 0 {
		return
	}

	// The database first, always. A sink is never the only copy: an export that
	// silently became the sole record would put the audit trail behind somebody
	// else's availability.
	if err := r.store.Append(ctx, records...); err != nil {
		r.log.ErrorContext(ctx, "audit append failed",
			"error", err, "repository", repository, "records", len(records))
	}

	if r.sink == nil {
		return
	}

	if err := r.sink.Emit(ctx, convert(repository, records)...); err != nil {
		r.log.ErrorContext(ctx, "audit sink failed",
			"error", err, "repository", repository, "records", len(records))
	}
}

// convert projects stored records onto the shape a sink receives, which
// deliberately carries no database identifiers.
func convert(repository policy.RepoID, records []*store.AuditRecord) []audit.Record {
	out := make([]audit.Record, 0, len(records))

	for _, rec := range records {
		if rec == nil {
			continue
		}

		out = append(out, audit.Record{
			OccurredAt:    rec.OccurredAt,
			Actor:         rec.ActorLabel,
			Action:        rec.Action,
			Repository:    string(repository),
			Branch:        rec.Branch,
			Path:          rec.Path,
			Effect:        string(rec.Effect),
			Reason:        string(rec.Reason),
			RuleID:        rec.RuleID,
			Guard:         rec.Guard,
			PolicyVersion: rec.PolicyVersion,
			RequestID:     rec.RequestID,
			TaskID:        string(rec.TaskID),
		})
	}

	return out
}
