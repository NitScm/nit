package mysql

import (
	"context"
	"database/sql"

	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/store"
)

type tenantStore struct{ db *sql.DB }

func (s *tenantStore) AdminGroups(ctx context.Context, tenant policy.TenantID) ([]policy.GroupID, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT group_id FROM tenant_admin_groups WHERE tenant_id = ? ORDER BY group_id`,
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return mapError(err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM tenant_admin_groups WHERE tenant_id = ?`, string(tenant)); err != nil {
		return mapError(err)
	}

	for _, group := range groups {
		if group == "" {
			continue
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT IGNORE INTO tenant_admin_groups (tenant_id, group_id) VALUES (?, ?)`,
			string(tenant), string(group)); err != nil {
			return mapError(err)
		}
	}

	return mapError(tx.Commit())
}

var _ store.TenantStore = (*tenantStore)(nil)
