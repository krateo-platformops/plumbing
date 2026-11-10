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
// and, while a controller replica runs, ControllerActive=True.
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
)
