// Package policy is the authorization core of nit: it decides whether a
// subject may perform an action on a path of a repository, at a given ref.
//
// Three properties drive the design.
//
// Evaluation is total. A Policy is compiled and validated once, up front; every
// pattern is checked then. Evaluate therefore returns a Decision and no error,
// which removes error paths from the hot loop and makes it impossible for a
// malformed rule to fail open at request time.
//
// Evaluation is order-independent. Every matching rule is considered; deny
// always wins over allow, and the default is deny. A policy file cannot change
// meaning because two rules were swapped.
//
// Every decision is attributable. A Decision names the rule that produced it,
// so the audit log can answer "why did this push pass on March 12?" — the
// question a permission system exists to answer.
//
// The package is pure: no IO, no clock, no logging. Loading and validating YAML
// bundles lives in the config subpackage.
package policy
