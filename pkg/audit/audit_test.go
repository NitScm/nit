package audit_test

import (
	"context"
	"errors"
	"testing"

	"github.com/NitScm/nit/pkg/audit"
)

type recorder struct {
	got  [][]audit.Record
	fail error
}

func (r *recorder) Emit(_ context.Context, records ...audit.Record) error {
	r.got = append(r.got, records)
	return r.fail
}

// One sink failing must not stop the others. Otherwise the order in which an
// operator happened to configure destinations decides which of them receive a
// record, which is not something anyone should have to reason about during an
// incident.
func TestMultiEmitsToEverySinkEvenAfterAFailure(t *testing.T) {
	boom := errors.New("siem unreachable")

	first := &recorder{fail: boom}
	second := &recorder{}
	third := &recorder{}

	err := audit.Multi{first, nil, second, third}.Emit(context.Background(),
		audit.Record{Action: "push.denied_path", Actor: "maya"})

	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want the failure reported", err)
	}
	for name, sink := range map[string]*recorder{"second": second, "third": third} {
		if len(sink.got) != 1 {
			t.Errorf("%s sink received %d batches, want 1", name, len(sink.got))
		}
	}
}

// A nil entry is skipped rather than panicking: configuration assembles this
// slice, and a disabled export is a nil sink.
func TestMultiToleratesNilSinks(t *testing.T) {
	if err := (audit.Multi{nil, nil}).Emit(context.Background(), audit.Record{}); err != nil {
		t.Errorf("Emit: %v", err)
	}
}

func TestDiscardAcceptsAnything(t *testing.T) {
	if err := (audit.Discard{}).Emit(context.Background(), audit.Record{}, audit.Record{}); err != nil {
		t.Errorf("Emit: %v", err)
	}
}

// The zero Multi is what the community edition registers when nothing is
// configured, and it must be usable without a nil check at every call site.
func TestZeroMultiIsUsable(t *testing.T) {
	var m audit.Multi

	if err := m.Emit(context.Background(), audit.Record{Action: "push.applied"}); err != nil {
		t.Errorf("Emit: %v", err)
	}
}
