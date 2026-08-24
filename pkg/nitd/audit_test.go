package nitd

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/NitScm/nit/internal/store/memory"
	"github.com/NitScm/nit/pkg/audit"
	"github.com/NitScm/nit/pkg/blob"
	"github.com/NitScm/nit/pkg/policy"
)

// The promise this assembly makes to anyone writing a sink: you write a client
// that speaks one protocol, and it is never on the request path.
//
// pkg/audit used to leave that to the implementer — "an implementation that
// talks to a network buffers and returns" — and an obligation met once per
// destination is met differently once per destination. If this wiring is ever
// dropped, a push starts paying for whatever a SIEM is doing.
func TestASuppliedSinkIsBufferedByTheAssembly(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	supplied := blocking{release: release}

	parts, err := open(context.Background(), Config{BlobDir: t.TempDir()}, Deps{
		Store:     memory.New(),
		Blobs:     blob.NewMemory(),
		Policy:    policy.Static{Policy: emptyPolicy(t)},
		AuditSink: supplied,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer parts.close()

	if parts.auditSink == nil {
		t.Fatal("a supplied sink was dropped rather than wired")
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		_ = parts.auditSink.Emit(context.Background(), audit.Record{Action: "push.accepted"})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Emit blocked on the destination: the assembly handed the sink " +
			"to the request path unbuffered")
	}
}

// No export configured must stay the absence of a sink, rather than a queue and
// a goroutine forwarding to nothing.
func TestNoSinkStaysNoSink(t *testing.T) {
	parts, err := open(context.Background(), Config{BlobDir: t.TempDir()}, Deps{
		Store:  memory.New(),
		Blobs:  blob.NewMemory(),
		Policy: policy.Static{Policy: emptyPolicy(t)},
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer parts.close()

	if parts.auditSink != nil {
		t.Error("a deployment with no export configured got a sink anyway")
	}
}

// emptyPolicy is a compiled bundle with no repositories: enough for the
// assembly to start, and nothing this test evaluates against.
func emptyPolicy(t *testing.T) *policy.Policy {
	t.Helper()

	compiled, err := policy.Compile(policy.Spec{})
	if err != nil {
		t.Fatalf("policy.Compile: %v", err)
	}

	return compiled
}

// blocking never returns until released, which is what a destination that is
// down looks like from here.
type blocking struct{ release chan struct{} }

func (b blocking) Emit(ctx context.Context, _ ...audit.Record) error {
	select {
	case <-b.release:
	case <-ctx.Done():
	}

	return nil
}
