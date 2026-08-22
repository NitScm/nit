package policy

// TenantID scopes every entity. nit ships single-tenant, but the identifier is
// carried from day one: threading a tenant through a schema and an API after
// the fact is one of the most expensive migrations there is.
type TenantID string

// DefaultTenant is the tenant used while nit runs in single-tenant mode.
const DefaultTenant TenantID = "default"

type (
	// UserID is the nit-internal identity of a person, stable across forge
	// account renames.
	UserID string

	// GroupID identifies a group of users.
	GroupID string

	// RepoID identifies a repository as configured in nit, independently of its
	// URL on the forge.
	RepoID string
)

// User is a person known to nit, together with the forge identities that map
// onto them.
type User struct {
	ID      UserID
	Email   string
	Aliases []string

	// ForgeLogins maps a forge key ("github", "gitlab") to the login used
	// there. Identity always comes from the authenticated session; these are
	// used to attribute existing history and to verify commit authorship, never
	// to authenticate.
	ForgeLogins map[string]string

	Disabled bool
}

// Group is a named set of users, optionally including other groups.
type Group struct {
	ID          GroupID
	Description string
	Members     []UserID
	Includes    []GroupID
}

// Repository is a repository placed under nit control.
type Repository struct {
	ID RepoID

	// Remote is the upstream URL on the forge.
	Remote string

	// Forge keys the driver used to talk to the hosting provider.
	Forge string

	// DefaultBranch is the branch used when a client does not specify one.
	DefaultBranch string
}

// Subject is the resolved principal of an authorization request: a user plus
// the transitive closure of the groups they belong to.
type Subject struct {
	UserID UserID
	Groups []GroupID
}

// InGroup reports whether the subject belongs to the given group.
func (s Subject) InGroup(id GroupID) bool {
	for _, g := range s.Groups {
		if g == id {
			return true
		}
	}
	return false
}
