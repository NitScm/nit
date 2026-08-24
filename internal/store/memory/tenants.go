package memory

import (
	"context"

	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/store"
)

type tenantStore Store

func (s *tenantStore) AdminGroups(_ context.Context, tenant policy.TenantID) ([]policy.GroupID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	groups := s.adminGroups[tenant]
	if groups == nil {
		return nil, nil
	}

	return append([]policy.GroupID(nil), groups...), nil
}

func (s *tenantStore) SetAdminGroups(_ context.Context, tenant policy.TenantID, groups []policy.GroupID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.adminGroups == nil {
		s.adminGroups = map[policy.TenantID][]policy.GroupID{}
	}

	kept := make([]policy.GroupID, 0, len(groups))

	for _, group := range groups {
		if group != "" {
			kept = append(kept, group)
		}
	}

	s.adminGroups[tenant] = kept

	return nil
}

var _ store.TenantStore = (*tenantStore)(nil)
