// Package memory is an in-memory implementation of the store interfaces.
//
// It exists for two reasons. It lets the queue semantics — partition exclusion,
// lease expiry, fencing, idempotency — be tested exhaustively without a
// database, and it gives single-node development and demos a zero-dependency
// backend. It is not a production store: nothing survives a restart.
//
// Every method returns copies. Handing out pointers into the map would let a
// caller mutate stored state without going through the store, which is exactly
// the class of bug the interface exists to prevent.
package memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/store"
)

// Store is an in-memory store.Store.
type Store struct {
	mu sync.Mutex

	seq atomic.Uint64

	users      map[store.ID]*store.User
	sessions   map[store.ID]*store.Session
	workspaces map[store.ID]*store.Workspace
	repos      map[store.ID]*store.Repository
	syncPoints map[syncKey]*store.SyncPoint
	tasks      map[store.ID]*store.Task
	artifacts  map[store.ID]*store.Artifact
	audit      []*store.AuditRecord

	auditSeq int64
}

type syncKey struct {
	workspace  store.ID
	repository store.ID
	branch     string
}

// New returns an empty in-memory store.
func New() *Store {
	return &Store{
		users:      make(map[store.ID]*store.User),
		sessions:   make(map[store.ID]*store.Session),
		workspaces: make(map[store.ID]*store.Workspace),
		repos:      make(map[store.ID]*store.Repository),
		syncPoints: make(map[syncKey]*store.SyncPoint),
		tasks:      make(map[store.ID]*store.Task),
		artifacts:  make(map[store.ID]*store.Artifact),
	}
}

func (s *Store) Users() store.UserStore              { return (*userStore)(s) }
func (s *Store) Sessions() store.SessionStore        { return (*sessionStore)(s) }
func (s *Store) Workspaces() store.WorkspaceStore    { return (*workspaceStore)(s) }
func (s *Store) Repositories() store.RepositoryStore { return (*repositoryStore)(s) }
func (s *Store) SyncPoints() store.SyncPointStore    { return (*syncPointStore)(s) }
func (s *Store) Tasks() store.TaskStore              { return (*taskStore)(s) }
func (s *Store) Artifacts() store.ArtifactStore      { return (*artifactStore)(s) }
func (s *Store) Audit() store.AuditStore             { return (*auditStore)(s) }
func (s *Store) Close() error                        { return nil }

func (s *Store) nextID(prefix string) store.ID {
	return store.ID(fmt.Sprintf("%s-%d", prefix, s.seq.Add(1)))
}

func newToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("memory: entropy unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

var _ store.Store = (*Store)(nil)

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

type userStore Store

func (s *userStore) ByID(_ context.Context, id store.ID) (*store.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	u, ok := s.users[id]
	if !ok {
		return nil, store.ErrNotFound
	}

	clone := *u
	return &clone, nil
}

func (s *userStore) ByPolicyID(_ context.Context, tenant policy.TenantID, policyID policy.UserID) (*store.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, u := range s.users {
		if u.TenantID == tenant && u.PolicyUserID == policyID {
			clone := *u
			return &clone, nil
		}
	}

	return nil, store.ErrNotFound
}

func (s *userStore) Upsert(_ context.Context, u *store.User) (*store.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for id, existing := range s.users {
		if existing.TenantID == u.TenantID && existing.PolicyUserID == u.PolicyUserID {
			updated := *u
			updated.ID = id
			updated.CreatedAt = existing.CreatedAt
			s.users[id] = &updated

			clone := updated
			return &clone, nil
		}
	}

	created := *u
	if created.ID == "" {
		created.ID = (*Store)(s).nextID("user")
	}
	s.users[created.ID] = &created

	clone := created
	return &clone, nil
}

// ---------------------------------------------------------------------------
// Workspaces
// ---------------------------------------------------------------------------

type workspaceStore Store

func (s *workspaceStore) ByID(_ context.Context, id store.ID) (*store.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	w, ok := s.workspaces[id]
	if !ok {
		return nil, store.ErrNotFound
	}

	clone := *w
	return &clone, nil
}

func (s *workspaceStore) ListByUser(_ context.Context, userID store.ID) ([]*store.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*store.Workspace
	for _, w := range s.workspaces {
		if w.UserID == userID {
			clone := *w
			out = append(out, &clone)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *workspaceStore) Create(_ context.Context, w *store.Workspace) (*store.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	created := *w
	if created.ID == "" {
		created.ID = (*Store)(s).nextID("ws")
	}
	s.workspaces[created.ID] = &created

	clone := created
	return &clone, nil
}

func (s *workspaceStore) Touch(_ context.Context, id store.ID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	w, ok := s.workspaces[id]
	if !ok {
		return store.ErrNotFound
	}

	w.LastSeenAt = &at
	return nil
}

// ---------------------------------------------------------------------------
// Repositories
// ---------------------------------------------------------------------------

type repositoryStore Store

func (s *repositoryStore) ByID(_ context.Context, id store.ID) (*store.Repository, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.repos[id]
	if !ok {
		return nil, store.ErrNotFound
	}

	clone := *r
	return &clone, nil
}

func (s *repositoryStore) ByPolicyID(_ context.Context, tenant policy.TenantID, policyID policy.RepoID) (*store.Repository, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, r := range s.repos {
		if r.TenantID == tenant && r.PolicyRepoID == policyID {
			clone := *r
			return &clone, nil
		}
	}

	return nil, store.ErrNotFound
}

func (s *repositoryStore) List(_ context.Context, tenant policy.TenantID) ([]*store.Repository, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*store.Repository
	for _, r := range s.repos {
		if r.TenantID == tenant {
			clone := *r
			out = append(out, &clone)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].PolicyRepoID < out[j].PolicyRepoID })
	return out, nil
}

func (s *repositoryStore) Reconcile(_ context.Context, tenant policy.TenantID, repos []*store.Repository) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	byPolicyID := make(map[policy.RepoID]*store.Repository)
	for _, r := range s.repos {
		if r.TenantID == tenant {
			byPolicyID[r.PolicyRepoID] = r
		}
	}

	for _, incoming := range repos {
		if existing, ok := byPolicyID[incoming.PolicyRepoID]; ok {
			updated := *incoming
			updated.ID = existing.ID
			updated.CreatedAt = existing.CreatedAt
			s.repos[existing.ID] = &updated
			continue
		}

		created := *incoming
		if created.ID == "" {
			created.ID = (*Store)(s).nextID("repo")
		}
		created.TenantID = tenant
		s.repos[created.ID] = &created
	}

	// Repositories dropped from the bundle are deliberately kept: tasks and
	// audit records still point at them.
	return nil
}

// ---------------------------------------------------------------------------
// Sync points
// ---------------------------------------------------------------------------

type syncPointStore Store

func (s *syncPointStore) Get(_ context.Context, workspaceID, repositoryID store.ID, branch string) (*store.SyncPoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sp, ok := s.syncPoints[syncKey{workspaceID, repositoryID, branch}]
	if !ok {
		return nil, store.ErrNotFound
	}

	clone := *sp
	return &clone, nil
}

func (s *syncPointStore) Put(_ context.Context, sp *store.SyncPoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := syncKey{sp.WorkspaceID, sp.RepositoryID, sp.Branch}

	stored := *sp
	if existing, ok := s.syncPoints[key]; ok {
		stored.CreatedAt = existing.CreatedAt
	}

	s.syncPoints[key] = &stored
	return nil
}

func (s *syncPointStore) CompareAndSet(_ context.Context, sp *store.SyncPoint, expectedCommit string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := syncKey{sp.WorkspaceID, sp.RepositoryID, sp.Branch}

	existing, ok := s.syncPoints[key]

	switch {
	case !ok && expectedCommit != "":
		return store.ErrNotFound
	case ok && existing.UpstreamCommit != expectedCommit:
		return store.ErrConflict
	}

	stored := *sp
	if ok {
		stored.CreatedAt = existing.CreatedAt
	}

	s.syncPoints[key] = &stored
	return nil
}

func (s *syncPointStore) ListByWorkspace(_ context.Context, workspaceID store.ID) ([]*store.SyncPoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*store.SyncPoint
	for key, sp := range s.syncPoints {
		if key.workspace == workspaceID {
			clone := *sp
			out = append(out, &clone)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].RepositoryID != out[j].RepositoryID {
			return out[i].RepositoryID < out[j].RepositoryID
		}
		return out[i].Branch < out[j].Branch
	})

	return out, nil
}

func (s *syncPointStore) Delete(_ context.Context, workspaceID, repositoryID store.ID, branch string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.syncPoints, syncKey{workspaceID, repositoryID, branch})
	return nil
}

// ---------------------------------------------------------------------------
// Artifacts
// ---------------------------------------------------------------------------

type artifactStore Store

func (s *artifactStore) ByDigest(_ context.Context, tenant policy.TenantID, digest string) (*store.Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, a := range s.artifacts {
		if a.TenantID == tenant && a.Digest == digest {
			clone := *a
			return &clone, nil
		}
	}

	return nil, store.ErrNotFound
}

func (s *artifactStore) Create(_ context.Context, a *store.Artifact) (*store.Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Identical bytes are the same artifact: return the existing record rather
	// than storing a duplicate.
	for _, existing := range s.artifacts {
		if existing.TenantID == a.TenantID && existing.Digest == a.Digest {
			clone := *existing
			return &clone, nil
		}
	}

	created := *a
	if created.ID == "" {
		created.ID = (*Store)(s).nextID("artifact")
	}
	s.artifacts[created.ID] = &created

	clone := created
	return &clone, nil
}

func (s *artifactStore) Expired(_ context.Context, now time.Time, limit int) ([]*store.Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*store.Artifact
	for _, a := range s.artifacts {
		if a.ExpiresAt != nil && !now.Before(*a.ExpiresAt) {
			clone := *a
			out = append(out, &clone)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}

	return out, nil
}

func (s *artifactStore) Delete(_ context.Context, id store.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.artifacts, id)
	return nil
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

type auditStore Store

func (s *auditStore) Append(_ context.Context, records ...*store.AuditRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, r := range records {
		s.auditSeq++

		stored := *r
		stored.ID = s.auditSeq
		if stored.OccurredAt.IsZero() {
			stored.OccurredAt = time.Now().UTC()
		}

		s.audit = append(s.audit, &stored)
	}

	return nil
}

func (s *auditStore) Query(_ context.Context, q store.AuditQuery) ([]*store.AuditRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*store.AuditRecord

	for _, r := range s.audit {
		switch {
		case q.Tenant != "" && r.TenantID != q.Tenant:
			continue
		case q.ActorUserID != "" && r.ActorUserID != q.ActorUserID:
			continue
		case q.RepositoryID != "" && r.RepositoryID != q.RepositoryID:
			continue
		case q.RequestID != "" && r.RequestID != q.RequestID:
			continue
		case !q.Since.IsZero() && r.OccurredAt.Before(q.Since):
			continue
		case !q.Until.IsZero() && r.OccurredAt.After(q.Until):
			continue
		}

		clone := *r
		out = append(out, &clone)
	}

	// Newest first, matching what an operator expects from a log view.
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })

	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}

	return out, nil
}

var (
	_ store.UserStore       = (*userStore)(nil)
	_ store.WorkspaceStore  = (*workspaceStore)(nil)
	_ store.RepositoryStore = (*repositoryStore)(nil)
	_ store.SyncPointStore  = (*syncPointStore)(nil)
	_ store.ArtifactStore   = (*artifactStore)(nil)
	_ store.AuditStore      = (*auditStore)(nil)
)
