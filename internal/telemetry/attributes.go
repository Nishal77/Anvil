package telemetry

import "go.opentelemetry.io/otel/attribute"

// Span attribute keys for PRD §17.1's mandatory list. Centralized here so
// every package spells anvil.job_id (etc.) exactly once — a key typo'd
// differently at two call sites doesn't fail anything, it just silently
// creates a second, useless attribute in Tempo that never lines up with
// the first.
const (
	AttrJobID          = attribute.Key("anvil.job_id")
	AttrUserID         = attribute.Key("anvil.user_id")
	AttrStepIdx        = attribute.Key("anvil.step_idx")
	AttrAttempt        = attribute.Key("anvil.attempt")
	AttrModel          = attribute.Key("anvil.model")
	AttrTokensIn       = attribute.Key("anvil.tokens_in")
	AttrTokensOut      = attribute.Key("anvil.tokens_out")
	AttrCostUSDMicros  = attribute.Key("anvil.cost_usd_micros")
	AttrToolName       = attribute.Key("anvil.tool_name")
	AttrPolicyDecision = attribute.Key("anvil.policy_decision")
)
