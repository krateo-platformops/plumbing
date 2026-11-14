// Package conditions is the single shared home for condition strings that more than one Krateo
// controller writes, so that a consumer gating on one of them reads the same bytes whichever
// controller produced it.
//
// These strings are a CROSS-REPOSITORY CONTRACT, in the same sense as package labels. core-provider
// writes them on a CompositionDefinition and oasgen-provider on a RestDefinition, and dashboards,
// installer's dependency gates and the portal match them. Change one only together with every
// consumer. A rename here is a silent behaviour change for anything that matched the old string.
package conditions

// The scale-to-zero condition shape, decided by Diego on 2026-11-10 for both repositories
// (core-provider#393, oasgen-provider#550).
//
// A definition whose controller was DELIBERATELY scaled to zero reports:
//
//	Ready             True   ReasonScaledToZero   the message says why and when it wakes:
//	                                              core-provider "idle: no instances since <t>; wakes on the first"
//	                                              oasgen-provider "suspended by spec"
//	ControllerActive  False  ReasonScaledToZero   nothing is reconciling this kind right now
//
// and, while a controller replica runs:
//
//	ControllerActive  True   ReasonRunning        a stable message, NO counts (Diego, 2026-11-11):
//	                                              core-provider "controller running; scales to zero after <holdDown> without instances"
//	                                              oasgen-provider "controller running"
//
// WHY NO COUNTS. A condition is rewritten whenever its message changes, so an instance or Kind count
// in the message would rewrite the definition's status on every create and delete: the per-reconcile
// churn core-provider removed in #298/#439. The True arm exists so a reader can tell "running" from
// "never set"; the count is not needed for that, and it would make the condition change without the
// state changing.
//
// WHY READY STAYS TRUE. The zero is a declared intent (core-provider: spec.controller.scaleToZero;
// oasgen-provider: a mutable RestDefinition field), so it is a stable state, not a failure. False or
// Unknown would wedge every consumer that gates on Ready. The definition only: a generated CR of an
// idled kind never claims Ready, because nothing progresses it.
//
// WHY A SEPARATE TYPE. A gate that needs "is anything reconciling this kind" must branch on a
// condition, not match a reason string on Ready. It is deliberately NOT named "Reconciling", which
// kstatus already uses to mean "in progress": a gate reading kstatus semantics would invert it.
const (
	// TypeControllerActive is True while the definition's controller has a running replica, and
	// False with ReasonScaledToZero while it is deliberately scaled to zero.
	TypeControllerActive = "ControllerActive"

	// ReasonScaledToZero is the reason on BOTH Ready=True and ControllerActive=False for a
	// definition whose controller is deliberately at zero replicas.
	ReasonScaledToZero = "ScaledToZero"

	// ReasonRunning is the reason on ControllerActive=True: the definition's controller has a
	// running replica. Not "Active", which would repeat the type without saying why.
	ReasonRunning = "Running"
)

// The terminal-by-design declaration, decided by Diego on 2026-11-14 (core-provider#498).
//
// A managed child that rests at Ready=False ON PURPOSE declares it with
//
//	Ready  False  ReasonTerminalByDesign   the message carries the instance detail
//
// and core-provider's composition rollup then counts that child as healthy, and never applies its
// stall deadline to it. Example: a create that a vendor answers 202 and that must never be driven
// again, such as an Ironic NodeProvision (driving it to "active" again wipes the disk).
//
// ONLY ON A CHILD, NEVER ON A COMPOSITION. A composition's own Ready never carries it: its
// controller writes a composition's reason only from its own verdicts, and installer's dependency
// gate would withhold a composition carrying an unrecognised reason.
//
// NOT A FAILURE REASON, AND NOT A SET. It is the opposite verdict to rest-dynamic-controller's
// NoCreateVerbDeclared and CreateUnreachable, which the rollup counts as failed: those say a
// definition must change, this says nothing is wrong. Those specific reasons stay as they are. A
// child that is merely waiting (Pending) must not use this: Pending means both "in flight" and
// "resting for good", which is exactly the ambiguity this reason removes. Emit it only from an
// explicit declaration, never inferred.
const (
	// ReasonTerminalByDesign is the Ready=False reason by which a child declares its not-Ready
	// state to be its healthy end state.
	ReasonTerminalByDesign = "TerminalByDesign"
)
