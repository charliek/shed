package delivery

import (
	"fmt"
	"net/http"
)

// Rule names one rule of the contract an answer can break. The names are a
// closed list (Rules) and travel on the wire: a 400, 413 or 422 answer to a
// source names the rule its answer broke.
type Rule string

// The rules, in the order ParseAnswer checks them (the contract's order).
const (
	RuleBodyTooLarge Rule = "body_too_large"
	// RuleEchoesCredential is a hosted source's reply that holds the bearer
	// token or the assertion the server presented (contract section 3b).
	// Only the hosted transport judges it; ParseAnswer never does.
	RuleEchoesCredential  Rule = "echoes_credential"
	RuleNotJSON           Rule = "not_json"
	RuleDuplicateMember   Rule = "duplicate_member"
	RuleNullValue         Rule = "null_value"
	RuleUnknownMember     Rule = "unknown_member"
	RuleMissingMember     Rule = "missing_member"
	RuleWrongType         Rule = "wrong_type"
	RuleUnsupportedVer    Rule = "unsupported_version"
	RuleBadID             Rule = "bad_id"
	RuleIDMismatch        Rule = "id_mismatch"
	RuleTooManyItems      Rule = "too_many_items"
	RuleTooManyProblems   Rule = "too_many_problems"
	RuleBadKind           Rule = "bad_kind"
	RuleWrongKindMember   Rule = "wrong_kind_member"
	RuleBadName           Rule = "bad_name"
	RuleBadProvider       Rule = "bad_provider"
	RuleBadStage          Rule = "bad_stage"
	RuleBadBase           Rule = "bad_base"
	RuleProjectBaseStage  Rule = "project_base_stage"
	RulePathEmpty         Rule = "path_empty"
	RulePathTooLong       Rule = "path_too_long"
	RulePathNotPrintable  Rule = "path_not_printable"
	RulePathBackslash     Rule = "path_backslash"
	RulePathAbsolute      Rule = "path_absolute"
	RulePathTrailingSlash Rule = "path_trailing_slash"
	RulePathEmptySegment  Rule = "path_empty_segment"
	RulePathDotSegment    Rule = "path_dot_segment"
	RulePathDotDotSegment Rule = "path_dot_dot_segment"
	RuleRefusedTarget     Rule = "refused_target"
	RuleBadMode           Rule = "bad_mode"
	RuleBadBase64         Rule = "bad_base64"
	RuleFileTooLarge      Rule = "file_too_large"
	RuleBadVariable       Rule = "bad_variable"
	RuleRefusedVariable   Rule = "refused_variable"
	RuleValueEmpty        Rule = "value_empty"
	RuleValueTooLong      Rule = "value_too_long"
	RuleValueNotPrintable Rule = "value_not_printable"
	RuleValueEdgeSpace    Rule = "value_edge_space"
	RuleBadFingerprint    Rule = "bad_fingerprint"
	RuleBadTime           Rule = "bad_time"
	RuleDuplicateName     Rule = "duplicate_name"
	RuleDuplicateTarget   Rule = "duplicate_target"
	RuleNestedTarget      Rule = "nested_target"
	RuleTotalTooLarge     Rule = "total_too_large"
	RuleEnvTooLarge       Rule = "env_too_large"
	RuleBadProblemCode    Rule = "bad_problem_code"
	RuleBadProblemMessage Rule = "bad_problem_message"
)

// Rules is every rule, in the contract's order.
var Rules = []Rule{
	RuleBodyTooLarge, RuleEchoesCredential, RuleNotJSON, RuleDuplicateMember, RuleNullValue,
	RuleUnknownMember, RuleMissingMember, RuleWrongType, RuleUnsupportedVer,
	RuleBadID, RuleIDMismatch, RuleTooManyItems, RuleTooManyProblems,
	RuleBadKind, RuleWrongKindMember, RuleBadName, RuleBadProvider,
	RuleBadStage, RuleBadBase, RuleProjectBaseStage,
	RulePathEmpty, RulePathTooLong, RulePathNotPrintable, RulePathBackslash,
	RulePathAbsolute, RulePathTrailingSlash, RulePathEmptySegment,
	RulePathDotSegment, RulePathDotDotSegment, RuleRefusedTarget,
	RuleBadMode, RuleBadBase64, RuleFileTooLarge,
	RuleBadVariable, RuleRefusedVariable, RuleValueEmpty, RuleValueTooLong,
	RuleValueNotPrintable, RuleValueEdgeSpace,
	RuleBadFingerprint, RuleBadTime,
	RuleDuplicateName, RuleDuplicateTarget, RuleNestedTarget, RuleTotalTooLarge, RuleEnvTooLarge,
	RuleBadProblemCode, RuleBadProblemMessage,
}

// ValidRule reports whether r is one of Rules.
func ValidRule(r Rule) bool {
	for _, x := range Rules {
		if x == r {
			return true
		}
	}
	return false
}

// Status is the HTTP status the transport answers a refusal under r with:
// 413 for a body over MaxBodyBytes, 400 for a document that does not parse,
// 422 for every other rule. The code is answer_refused in all three.
func (r Rule) Status() int {
	switch r {
	case RuleBodyTooLarge:
		return http.StatusRequestEntityTooLarge
	case RuleNotJSON:
		return http.StatusBadRequest
	}
	return http.StatusUnprocessableEntity
}

// Refusal is an answer (or request) refused under a rule. Where locates it
// (items[3].path, problems[0].code, or empty for the whole document); it
// names members and indexes only, never a value, so a Refusal may be
// logged and returned to the source.
type Refusal struct {
	Rule  Rule
	Where string
}

func (r *Refusal) Error() string {
	if r.Where == "" {
		return fmt.Sprintf("refused: %s", r.Rule)
	}
	return fmt.Sprintf("refused: %s at %s", r.Rule, r.Where)
}

func refuse(rule Rule, where string) *Refusal { return &Refusal{Rule: rule, Where: where} }
