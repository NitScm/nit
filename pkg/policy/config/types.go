package config

// UserFile is one entry of users.yaml.
type UserFile struct {
	ID    string `yaml:"id"`
	Email string `yaml:"email"`

	// Aliases are additional email addresses attributed to the same person,
	// used to match existing commit history.
	Aliases []string `yaml:"aliases"`

	// ForgeLogins maps a forge key to the account name used there. These are
	// used to attribute history and to verify commit authorship. They are never
	// used to authenticate: identity comes from the session token, because a
	// commit author field is free text that anyone can forge.
	ForgeLogins map[string]string `yaml:"forge_logins"`

	Disabled bool `yaml:"disabled"`
}

// GroupFile is one entry of groups.yaml.
type GroupFile struct {
	ID          string `yaml:"id"`
	Description string `yaml:"description"`

	Members []string `yaml:"members"`

	// Includes lists groups absorbed by this one: every member of an included
	// group is also a member of this group.
	Includes []string `yaml:"includes"`
}

// RepositoryFile is one entry of repositories.yaml.
type RepositoryFile struct {
	ID     string `yaml:"id"`
	Remote string `yaml:"remote"`
	Forge  string `yaml:"forge"`

	DefaultBranch string `yaml:"default_branch"`
}

// SubjectFile selects the principal of a rule.
type SubjectFile struct {
	Type string `yaml:"type"`
	ID   string `yaml:"id"`
}

// RuleFile is one entry of a repositories/<id>/rules.yaml file.
type RuleFile struct {
	// ID is optional; the loader derives a stable one from the file position
	// when it is absent. Naming rules explicitly is strongly recommended: the
	// id is what appears in denials and audit records.
	ID string `yaml:"id"`

	Subject SubjectFile `yaml:"subject"`

	// Except carves subjects out of Subject. It is how an exemption to a
	// universal deny is expressed, since deny always wins over allow.
	Except []SubjectFile `yaml:"except"`

	// Paths use the pattern syntax documented on policy.Pattern: a trailing
	// slash means "this subtree", anything else is a glob.
	Paths []string `yaml:"paths"`

	// Refs restricts the rule to matching refs, for example
	// "refs/heads/feature/**". Empty means every ref.
	Refs []string `yaml:"refs"`

	Actions []string `yaml:"actions"`

	Effect string `yaml:"effect"`

	// Description is shown to the developer when this rule denies them.
	Description string `yaml:"description"`
}
