package engine

import (
	"regexp"
	"strings"
	"sync"

	"github.com/pericles-luz/crm/internal/funnel/rules"
)

// matchTrigger returns true when the rule's trigger fires for the
// inbound message. The function is type-aware and dispatches on
// [rules.TriggerType]; unknown types are non-matching by default so the
// resolver can evolve the enum ahead of the engine learning the new
// case.
//
// The matcher is pure: no I/O, no clocks. It can be unit-tested in
// isolation, and the [Engine.Handle] keeps it that way by passing only
// the decoded [InboundMessage] + the resolved rule.
//
// Trigger contracts (per [rules.TriggerType] semantics):
//
//   - TriggerTypeMessageContains:
//     trigger_config: {"phrase": "<substring>"}
//     Matches when canonicalised(body) contains canonicalised(phrase).
//     Canonicalisation mirrors rules.TriggerSignature: lower-case + trim.
//
//   - TriggerTypeMessageKeywordRegex:
//     trigger_config: {"regex": "<RE2 expression>"}
//     Matches when the regex (case-sensitive, RE2 dialect) matches any
//     substring of body. An invalid regex is logged-as-poison upstream
//     and returns false here; the matcher itself does not log.
//
//   - TriggerTypeCampaignClick:
//     Not yet evaluable inside the engine — the click-to-message
//     attribution lives in inbox.ReceiveInbound (SIN-62959) and is not
//     re-published on the inbound subject yet. The matcher returns
//     false so cascade resolution stays correct; the trigger type
//     stays a first-class enum value so admin tooling and storage
//     accept it. Wiring the match will live in a future ticket once
//     the attribution payload joins the inbound event envelope.
//
// Caller responsibility: matchTrigger does NOT check rule.Enabled or
// rule.Scope — the resolver already filtered to the effective set.
func matchTrigger(rule rules.Rule, msg InboundMessage) bool {
	switch rule.TriggerType {
	case rules.TriggerTypeMessageContains:
		phrase, ok := stringField(rule.TriggerConfig, "phrase")
		if !ok {
			return false
		}
		needle := strings.ToLower(strings.TrimSpace(phrase))
		if needle == "" {
			return false
		}
		haystack := strings.ToLower(msg.Body)
		return strings.Contains(haystack, needle)

	case rules.TriggerTypeMessageKeywordRegex:
		expr, ok := stringField(rule.TriggerConfig, "regex")
		if !ok {
			return false
		}
		expr = strings.TrimSpace(expr)
		if expr == "" {
			return false
		}
		re, err := compileRegex(expr)
		if err != nil {
			return false
		}
		return re.MatchString(msg.Body)

	case rules.TriggerTypeCampaignClick:
		// Deferred — see package-level note. Falling through to false
		// keeps the engine honest about its current capabilities; the
		// rule stays installed and the action does not fire.
		return false
	}
	return false
}

// stageKey extracts the destination stage key from a rule's
// action_config. Returns "" + false when the field is missing or not a
// string; [Engine.Handle] treats that as a non-match and skips the
// action.
func stageKey(rule rules.Rule) (string, bool) {
	if rule.ActionType != rules.ActionTypeMoveToStage {
		return "", false
	}
	if v, ok := stringField(rule.ActionConfig, "stage_key"); ok {
		v = strings.TrimSpace(v)
		if v != "" {
			return v, true
		}
	}
	// Legacy alias used in the SIN-62952 migration example
	// ({"stage":"high-intent"}); accepted for forward-compat with
	// admin tooling that already writes the shorter key.
	if v, ok := stringField(rule.ActionConfig, "stage"); ok {
		v = strings.TrimSpace(v)
		if v != "" {
			return v, true
		}
	}
	return "", false
}

// stringField pulls a string from an untyped JSON map. Returns ok=false
// when the map is nil, the key is missing, or the value is non-string.
// Duplicate of the helper in rules.rule.go on purpose — the engine
// package does not import rules' unexported helpers, and the function
// is two lines.
func stringField(m map[string]any, key string) (string, bool) {
	if m == nil {
		return "", false
	}
	v, ok := m[key].(string)
	return v, ok
}

// regexCache memoises compiled regexes so the engine does not pay the
// MustCompile cost on every delivery. The cache is process-wide and
// safe under concurrent reads/writes: a sync.Map keyed by the verbatim
// expression. Sized informally — funnel rules per tenant cap in the
// dozens.
var regexCache sync.Map

func compileRegex(expr string) (*regexp.Regexp, error) {
	if cached, ok := regexCache.Load(expr); ok {
		return cached.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, err
	}
	regexCache.Store(expr, re)
	return re, nil
}
