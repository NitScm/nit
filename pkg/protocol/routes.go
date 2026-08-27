package protocol

// Routes is every HTTP route this API serves, as "METHOD /path".
//
// # Why this is public, and why it is a list rather than a comment
//
// The API description used to live in this module, beside the server, with a
// test asserting the two agreed. The description now lives with the tooling
// that renders it, and the test that kept it honest had to be able to follow —
// otherwise a specification would drift from the server it describes with
// nothing to catch it, which is worse than having none, because a specification
// is believed.
//
// So the surface is declared here, in a package anybody can import, and the
// server's own test asserts that what it registers is exactly this. Whoever
// holds the description compares it against this list and gets a real answer
// without needing this module's source.
//
// It is also useful on its own: a client, a proxy or a load-balancer rule can
// enumerate the API without parsing anything.
//
// Ordered as the server registers them, which groups them by what they need:
// nothing, a token, an operator's token.
func Routes() []string {
	return []string{
		// Unauthenticated: a load balancer has no token.
		"GET " + RouteHealthz,

		// Authenticated.
		"GET " + RouteWhoAmI,
		"GET " + RouteRepos,

		"POST " + RouteWorkspaces,
		"GET " + RouteWorkspaces,

		"POST " + RoutePush,
		"POST " + RoutePull,

		"POST " + RouteBlobs,

		"GET " + RouteTasks + "/{id}",
		"GET " + RouteEvents,
		"GET " + RouteTaskPatch,

		// Operations: read-only, restricted to the admin groups. Everything
		// that changes authorization goes through the policy bundle, so the
		// reviewed path stays the only path.
		"GET " + RouteAdminTasks,
		"GET " + RouteAdminTasks + "/{id}",
		"GET " + RouteAdminAudit,
		"GET " + RouteAdminStats,
		"GET " + RouteAdminPolicy,
	}
}

// The operations routes, named here for the same reason as the rest: a path
// spelled twice is a path that will one day be spelled two ways.
const (
	RouteAdminTasks  = "/v1/admin/tasks"
	RouteAdminAudit  = "/v1/admin/audit"
	RouteAdminStats  = "/v1/admin/stats"
	RouteAdminPolicy = "/v1/admin/policy"
)
