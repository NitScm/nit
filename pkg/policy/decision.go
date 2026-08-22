package policy

import "fmt"

// Reason is a machine-readable explanation of a Decision, so that clients and
// the audit log do not have to parse prose.
type Reason string

const (
	// ReasonAllowedByRule: an allow rule matched and no deny rule did.
	ReasonAllowedByRule Reason = "allowed_by_rule"

	// ReasonDeniedByRule: a deny rule matched. Deny always wins.
	ReasonDeniedByRule Reason = "denied_by_rule"

	// ReasonNoMatchingRule: nothing matched, and the default is deny.
	ReasonNoMatchingRule Reason = "no_matching_rule"

	// ReasonUnknownRepository: the repository is not under nit control.
	ReasonUnknownRepository Reason = "unknown_repository"

	// ReasonUserDisabled: the subject's account is disabled.
	ReasonUserDisabled Reason = "user_disabled"
)

// Decision is the outcome of evaluating one (path, action) pair, together with
// everything needed to explain it to a developer and to record it in the audit
// log.
type Decision struct {
	Allowed bool
	Effect  Effect
	Reason  Reason

	// RuleID is the rule that produced the decision; empty when nothing
	// matched.
	RuleID string

	// Pattern is the path pattern of that rule that matched.
	Pattern string

	// Description carries the rule's human-readable explanation, so a denial
	// tells the developer what to do next instead of just saying no.
	Description string

	// PolicyVersion identifies the exact bundle that produced this decision.
	// Without it, "why did this pass last month?" is unanswerable.
	PolicyVersion string
}

// String renders a one-line explanation suitable for CLI output.
func (d Decision) String() string {
	verb := "denied"
	if d.Allowed {
		verb = "allowed"
	}

	if d.RuleID == "" {
		return fmt.Sprintf("%s (%s)", verb, d.Reason)
	}

	return fmt.Sprintf("%s by rule %s (%s: %s)", verb, d.RuleID, d.Reason, d.Pattern)
}

func denied(reason Reason, version string) Decision {
	return Decision{
		Allowed:       false,
		Effect:        EffectDeny,
		Reason:        reason,
		PolicyVersion: version,
	}
}
