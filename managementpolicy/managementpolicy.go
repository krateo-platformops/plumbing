// Package managementpolicy is the one definition of which verbs a Krateo controller may perform on the
// external resource behind a managed object (unstructured-runtime#110, D9). Both runtimes that execute
// the verbs, unstructured-runtime (compositions, REST resources) and provider-runtime, read it from
// here, so the two cannot disagree again: before this package each kept its own copy, and they had
// diverged (unstructured-runtime gated only delete until v1.28.0).
//
// It owns MANAGEMENT verbs: what a controller may do to the external resource it manages. It does not
// own Kubernetes API verbs (get/list/watch/... for RBAC rules), which are compositionrbac's and
// kubeutil/rbacgen's. The words overlap; the vocabularies do not.
//
// The annotation (Diego, 2026-11-18):
//
//   - D1: krateo.io/management-policy is a SET of verbs, comma-separated, any order:
//     "observe,create" means observe and create only. The four historical presets are aliases:
//     default = observe,create,update,delete; observe-create-update; observe-delete; observe.
//     Unset or empty means default.
//   - D3: observe is always implied: "create" means observe,create.
//   - D2: a value that does not parse (an unknown token, a stray separator) makes the controller act
//     on NOTHING: the policy is observe-only, and Invalid names the bad token so the caller can report
//     Synced=False/ManagementPolicyInvalid. A typo meant to restrict never widens into full management.
//   - D5: krateo.io/deletion-policy: orphan ALWAYS wins. Delete runs only when the set allows it AND
//     the deletion policy is not orphan. (Before, observe-delete deleted despite orphan.) A policy
//     without delete orphans, as before.
//   - D8: a controller whose API declares only some verbs (a RestDefinition's verbsDescription) passes
//     them as Declared; the effective verbs are declared AND allowed. Declared absent (nil) means "no
//     verb information", and the policy alone decides; Declared empty means the API declares none.
package managementpolicy

import (
	"fmt"
	"strings"
)

// The reasons both runtimes report, so the literal is one and the same in each (D2, D4).
const (
	// ReasonInvalid (Synced=False): the annotation did not parse; Policy.Invalid says why.
	ReasonInvalid = "ManagementPolicyInvalid"
	// ReasonCreateWithheld (Ready=False, verdict failed): the external resource is absent and the
	// policy withholds create. Only a human changes it.
	ReasonCreateWithheld = "CreateWithheldByPolicy"
	// ReasonUpdateWithheld (Ready=True): the resource exists and works, but drift (or a new chart
	// version, D7) is not applied because the policy withholds update.
	ReasonUpdateWithheld = "UpdateWithheldByPolicy"
	// EventAbsentUnderPolicy (a Warning event): an absent resource under an observe-only policy. It is
	// NOT the generic observe-error event reason (unstructured-runtime v1.28.1).
	EventAbsentUnderPolicy = "ExternalResourceAbsentUnderPolicy"
)

// The annotation keys this package reads.
const (
	AnnotationKey         = "krateo.io/management-policy"
	DeletionAnnotationKey = "krateo.io/deletion-policy"
	// DeletionOrphan is the deletion-policy value that keeps the external resource.
	DeletionOrphan = "orphan"
)

// Verb is one thing a controller may do to the external resource.
type Verb string

const (
	Observe Verb = "observe"
	Create  Verb = "create"
	Update  Verb = "update"
	Delete  Verb = "delete"
)

// verbs is every Verb, in canonical order.
var verbs = []Verb{Observe, Create, Update, Delete}

// presets are the historical single-token values, kept as aliases (D1).
var presets = map[string][]Verb{
	"default":               {Observe, Create, Update, Delete},
	"observe-create-update": {Observe, Create, Update},
	"observe-delete":        {Observe, Delete},
	"observe":               {Observe},
}

// Policy is a parsed management policy: the verbs it allows. Observe is always among them.
type Policy struct {
	allowed map[Verb]bool
	// Invalid is non-empty when the annotation did not parse: it names what was wrong, and the policy
	// is observe-only (D2).
	Invalid string
}

// Default is the policy of an object with no annotation: every verb.
func Default() Policy { return of(Observe, Create, Update, Delete) }

func of(vs ...Verb) Policy {
	p := Policy{allowed: map[Verb]bool{Observe: true}}
	for _, v := range vs {
		p.allowed[v] = true
	}
	return p
}

// Parse reads a krateo.io/management-policy value.
func Parse(value string) Policy {
	value = strings.TrimSpace(value)
	if value == "" {
		return Default()
	}
	if vs, ok := presets[value]; ok {
		return of(vs...)
	}
	p := of()
	for _, tok := range strings.Split(value, ",") {
		tok = strings.TrimSpace(tok)
		switch Verb(tok) {
		case Observe, Create, Update, Delete:
			p.allowed[Verb(tok)] = true
		default:
			if tok == "" {
				return invalid(fmt.Sprintf("%q has an empty entry", value))
			}
			return invalid(fmt.Sprintf("%q: unknown verb %q (want a comma-separated set of observe, create, update, delete, or one of default, observe-create-update, observe-delete, observe)", value, tok))
		}
	}
	return p
}

func invalid(why string) Policy {
	p := of() // observe only: act on nothing (D2)
	p.Invalid = why
	return p
}

// Allows reports whether the policy allows v.
func (p Policy) Allows(v Verb) bool { return p.allowed[v] }

// String is the canonical set, e.g. "observe,create".
func (p Policy) String() string {
	var out []string
	for _, v := range verbs {
		if p.allowed[v] {
			out = append(out, string(v))
		}
	}
	return strings.Join(out, ",")
}

// Decision is what a controller may do for one object: its policy, its deletion policy, and the verbs
// its API declares.
type Decision struct {
	Policy Policy
	// Orphan is deletion-policy: orphan.
	Orphan bool
	// Declared is the set of verbs the API declares (D8); nil when the controller gives no verb
	// information.
	Declared map[Verb]bool
}

// For reads the decision from an object's annotations. declared is the API's verbs (nil for none
// given; an empty, non-nil set when the API declares no verb).
func For(annotations map[string]string, declared []Verb) Decision {
	d := Decision{
		Policy: Parse(annotations[AnnotationKey]),
		Orphan: strings.TrimSpace(annotations[DeletionAnnotationKey]) == DeletionOrphan,
	}
	if declared != nil {
		d.Declared = map[Verb]bool{}
		for _, v := range declared {
			d.Declared[v] = true
		}
	}
	return d
}

// Withheld says why a verb will not run, or "" when it may.
type Withheld string

const (
	// NotWithheld: the verb may run.
	NotWithheld Withheld = ""
	// ByPolicy: krateo.io/management-policy does not allow it.
	ByPolicy Withheld = "ByPolicy"
	// ByDeletionPolicy: deletion-policy: orphan keeps the external resource (D5).
	ByDeletionPolicy Withheld = "ByDeletionPolicy"
	// NotDeclared: the API does not declare the verb (D8). Its owner is the API, not the object's
	// author, so it is reported apart from ByPolicy.
	NotDeclared Withheld = "NotDeclared"
)

// Why reports whether v may run, and if not, why. Observe always may: a controller that cannot observe
// cannot decide anything (D3), and an API with no observe verb is the API's own problem to report.
func (d Decision) Why(v Verb) Withheld {
	if v == Observe {
		return NotWithheld
	}
	if d.Declared != nil && !d.Declared[v] {
		return NotDeclared
	}
	if !d.Policy.Allows(v) {
		return ByPolicy
	}
	if v == Delete && d.Orphan {
		return ByDeletionPolicy
	}
	return NotWithheld
}

// May reports whether v may run.
func (d Decision) May(v Verb) bool { return d.Why(v) == NotWithheld }

// Withholds lists the verbs that will not run, in canonical order, for a message.
func (d Decision) Withholds() []Verb {
	var out []Verb
	for _, v := range verbs {
		if !d.May(v) {
			out = append(out, v)
		}
	}
	return out
}
