package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/store"
)

type policyVersionStore struct{ pool *pgxpool.Pool }

// Record notes that a bundle is in force.
//
// First sighting kept, last sighting updated. The source is only written on the
// first: a reload from somewhere else is worth knowing about, but overwriting
// would lose where a version was first seen, which is the half an auditor asks
// about.
func (p *policyVersionStore) Record(ctx context.Context, v *store.PolicyVersion) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO policy_versions (tenant_id, version, source)
		VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, version) DO UPDATE SET last_loaded_at = now()`,
		v.TenantID, v.Version, v.Source)

	return mapError(err)
}

// Attach adds provenance to a version already recorded.
func (p *policyVersionStore) Attach(ctx context.Context, tenant policy.TenantID, version, ref, commit string) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE policy_versions SET ref = $3, commit_sha = $4
		WHERE tenant_id = $1 AND version = $2`, tenant, version, ref, commit)
	if err != nil {
		return mapError(err)
	}

	// Refused rather than inserted. A provenance row for a bundle this
	// deployment has never loaded is a claim about something that never
	// happened here.
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}

	return nil
}

func (p *policyVersionStore) List(ctx context.Context, tenant policy.TenantID, limit int) ([]*store.PolicyVersion, error) {
	if limit <= 0 {
		limit = 50
	}

	rows, err := p.pool.Query(ctx, `
		SELECT tenant_id, version, first_loaded_at, last_loaded_at, source, ref, commit_sha
		FROM policy_versions WHERE tenant_id = $1
		ORDER BY first_loaded_at DESC, version DESC
		LIMIT $2`, tenant, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	var out []*store.PolicyVersion

	for rows.Next() {
		v := &store.PolicyVersion{}

		if err := rows.Scan(&v.TenantID, &v.Version, &v.FirstLoadedAt,
			&v.LastLoadedAt, &v.Source, &v.Ref, &v.Commit); err != nil {
			return nil, mapError(err)
		}

		out = append(out, v)
	}

	return out, mapError(rows.Err())
}

func (p *policyVersionStore) ByVersion(ctx context.Context, tenant policy.TenantID, version string) (*store.PolicyVersion, error) {
	v := &store.PolicyVersion{}

	err := p.pool.QueryRow(ctx, `
		SELECT tenant_id, version, first_loaded_at, last_loaded_at, source, ref, commit_sha
		FROM policy_versions WHERE tenant_id = $1 AND version = $2`, tenant, version).
		Scan(&v.TenantID, &v.Version, &v.FirstLoadedAt, &v.LastLoadedAt, &v.Source, &v.Ref, &v.Commit)

	if err != nil {
		return nil, mapError(err)
	}

	return v, nil
}
