// Package audit defines where a decision record goes after it is made.
//
// Every authorization decision is appended to the database. A Sink lets that
// fan out — to a SIEM, to object storage, to a file — without the code that
// makes decisions knowing anything about the destination.
//
// Two properties every Sink must preserve, and they are not style preferences:
//
//   - **A failed write must not fail the operation being recorded.** Audit is
//     best-effort at request time on purpose: refusing an authorized push
//     because a log write failed would be its own kind of outage. A Sink that
//     returns an error has it logged loudly; the operation continues.
//
//   - **A Sink is never the only copy.** The database write stays. An export
//     that silently became the sole record would put the audit trail behind
//     somebody else's availability.
//
// # What a Sink does not have to do
//
// Buffer. This package used to make "an implementation that talks to a network
// buffers and returns" the implementer's obligation, which meant the same queue
// written once per destination and losing records differently each time. The
// assembly does it now: pkg/nitd puts a bounded queue in front of whatever
// sink it is handed, so Emit is called from a background goroutine, in batches,
// with a context of its own — never the request's, because a developer
// cancelling their push must not also cancel the record of what was decided
// before they did.
//
// So a Sink can be what it should be: a client that speaks one protocol.
// Blocking in Emit is fine. What is not fine is keeping the slice it was handed,
// or not being safe to call from several goroutines. pkg/audit/audittest is the
// suite that says so, and pkg/audit.Memory is a working sink to test against.
//
// See docs/EXTENSIONS.md.
package audit

import (
	"context"
	"errors"
	"time"
)

// Record is one authorization decision, in the form a Sink receives it.
//
// It is deliberately a value type with no store identifiers: a Sink writes
// somewhere that has never heard of this database, and giving it primary keys
// would invite it to join on them.
type Record struct {
	// OccurredAt is a time, not a formatted string: how a destination wants it
	// rendered is the sink's business, and baking one format in here would make
	// every sink that wants another one parse it back.
	OccurredAt time.Time

	// Actor is the bundle identity that acted — the stable key, not a display
	// name. Names and addresses collide and change.
	Actor string

	// Action is the stable identifier a consumer branches on: push.accepted,
	// push.denied_path, pull.delivered, and so on.
	Action string

	// Repository is the bundle identity, supplied by the caller — never the
	// database row id. A sink writes somewhere that has never heard of this
	// database, and handing it a primary key invites it to join on one.
	Repository string
	Branch     string

	// Path is set on a per-path decision and empty otherwise.
	Path string

	// Effect, Reason, RuleID and Guard say what decided it. RuleID is empty
	// when nothing matched and the default deny applied, which is a policy gap
	// rather than a refusal anyone wrote.
	Effect string
	Reason string
	RuleID string
	Guard  string

	// PolicyVersion is the bundle in force when the decision was made, so a
	// past decision can be replayed against exactly the rules that produced it.
	PolicyVersion string

	// RequestID follows one operation end to end; TaskID names the task it
	// became, when it became one.
	RequestID string
	TaskID    string
}

// Sink receives decision records.
type Sink interface {
	// Emit is called after the records have been persisted, from a background
	// goroutine, with a context that is not the request's. Taking a moment is
	// expected; a destination that is down costs a push nothing.
	//
	// It must not retain records — the caller is free to reuse the slice — and
	// it must be safe to call concurrently. An empty batch is a no-op, not an
	// error.
	Emit(ctx context.Context, records ...Record) error
}

// Multi fans records out to several sinks.
//
// Every sink is attempted even after one fails, and the errors are joined:
// stopping at the first would make the order of configuration decide which
// destinations receive a record, which is not a thing an operator should have
// to reason about.
type Multi []Sink

// Emit implements Sink.
func (m Multi) Emit(ctx context.Context, records ...Record) error {
	var errs []error

	for _, sink := range m {
		if sink == nil {
			continue
		}

		if err := sink.Emit(ctx, records...); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// Discard is a Sink that drops everything, which is what the community edition
// registers when no export is configured.
type Discard struct{}

// Emit implements Sink.
func (Discard) Emit(context.Context, ...Record) error { return nil }
