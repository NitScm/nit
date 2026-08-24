package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/store"
)

type tenantStore struct{ pool *pgxpool.Pool }

func (s *tenantStore) AdminGroups(ctx context.Context, tenant policy.TenantID) ([]policy.GroupID, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT group_id FROM tenant_admin_groups WHERE tenant_id = $1 ORDER BY group_id`,
		string(tenant))
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	var out []policy.GroupID

	for rows.Next() {
		var group string
		if err := rows.Scan(&group); err != nil {
			return nil, mapError(err)
		}

		out = append(out, policy.GroupID(group))
	}

	return out, mapError(rows.Err())
}

// SetAdminGroups replaces the list in one transaction.
//
// Replace rather than merge: an operator writing the list means *this is the
// list*, and a merge would make removing an administrator require a second
// command nobody would remember to run.
func (s *tenantStore) SetAdminGroups(ctx context.Context, tenant policy.TenantID, groups []policy.GroupID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return mapError(err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`DELETE FROM tenant_admin_groups WHERE tenant_id = $1`, string(tenant)); err != nil {
		return mapError(err)
	}

	for _, group := range groups {
		if group == "" {
			continue
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO tenant_admin_groups (tenant_id, group_id)
			VALUES ($1, $2)
			ON CONFLICT DO NOTHING`, string(tenant), string(group)); err != nil {
			return mapError(err)
		}
	}

	return mapError(tx.Commit(ctx))
}

var _ store.TenantStore = (*tenantStore)(nil)
