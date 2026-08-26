package mysql

import (
	"context"
	"database/sql"

	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/store"
)

type policyVersionStore struct{ db *sql.DB }

// Record notes that a bundle is in force.
//
// First sighting kept, last sighting updated. See the PostgreSQL file for why
// the source is written only on the first.
func (p *policyVersionStore) Record(ctx context.Context, v *store.PolicyVersion) error {
	_, err := p.db.ExecContext(ctx, `
		INSERT INTO policy_versions (tenant_id, version, source)
		VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE last_loaded_at = CURRENT_TIMESTAMP(6)`,
		v.TenantID, v.Version, v.Source)

	return mapError(err)
}

func (p *policyVersionStore) Attach(ctx context.Context, tenant policy.TenantID, version, ref, commit string) error {
	result, err := p.db.ExecContext(ctx, `
		UPDATE policy_versions SET ref = ?, commit_sha = ?
		WHERE tenant_id = ? AND version = ?`, ref, commit, tenant, version)
	if err != nil {
		return mapError(err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return mapError(err)
	}

	// MySQL reports zero rows affected when an UPDATE changes nothing, even if
	// the row exists — so a second Attach with the same values would look like
	// a missing row. Checked with a read rather than trusted from the count.
	if affected == 0 {
		if _, err := p.ByVersion(ctx, tenant, version); err != nil {
			return err
		}
	}

	return nil
}

func (p *policyVersionStore) List(ctx context.Context, tenant policy.TenantID, limit int) ([]*store.PolicyVersion, error) {
	if limit <= 0 {
		limit = 50
	}

	rows, err := p.db.QueryContext(ctx, `
		SELECT tenant_id, version, first_loaded_at, last_loaded_at, source, ref, commit_sha
		FROM policy_versions WHERE tenant_id = ?
		ORDER BY first_loaded_at DESC, version DESC
		LIMIT ?`, tenant, limit)
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

	err := p.db.QueryRowContext(ctx, `
		SELECT tenant_id, version, first_loaded_at, last_loaded_at, source, ref, commit_sha
		FROM policy_versions WHERE tenant_id = ? AND version = ?`, tenant, version).
		Scan(&v.TenantID, &v.Version, &v.FirstLoadedAt, &v.LastLoadedAt, &v.Source, &v.Ref, &v.Commit)

	if err != nil {
		return nil, mapError(err)
	}

	return v, nil
}
