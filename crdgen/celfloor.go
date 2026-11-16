package crdgen

import (
	"fmt"
	"sort"
	"sync"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	schemacel "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/version"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"k8s.io/apiserver/pkg/cel/environment"
	basecompat "k8s.io/component-base/version"
)

// MinKubernetesVersion is the oldest Kubernetes crdgen generates CRDs for: every CEL rule in a
// generated CRD must be one an apiserver of this version accepts (plumbing#70; Diego, 2026-11-16:
// "1.36, same as core-provider"). Options.MinKubernetesVersion can raise it, never lower it.
//
// # Why crdgen pins this rather than inheriting it
//
// validateCRD runs the apiserver's own CRD validation, which compiles CEL in the environment of the
// k8s.io libraries crdgen is LINKED with: an apiserver of version X accepts the CEL libraries
// introduced up to X-1 (environment.DefaultCompatibilityVersion). So the version crdgen validated
// against was a side effect of plumbing's go.mod, and the next k8s.io bump would have moved it
// silently: a rule using a function from that release would pass generation and be refused by every
// cluster still on the floor. The floor is now a decision, and the libraries only have to be new
// enough to know it.
//
// The CEL COST of a rule does not depend on the version (measured on #70 at compatibility 1.28 to
// 1.35, for a cheap and an expensive rule), so the floor decides which functions exist, nothing else.
const MinKubernetesVersion = "1.36"

// celTarget is the Kubernetes version a generation validates CEL for.
type celTarget struct {
	kube *version.Version // the cluster version, e.g. 1.36
}

// compat is the CEL compatibility version an apiserver of t.kube compiles new expressions with.
func (t celTarget) compat() *version.Version { return t.kube.SubtractMinor(1) }

// resolveCELTarget validates Options.MinKubernetesVersion and returns the target.
func resolveCELTarget(requested string) (celTarget, error) {
	floor := version.MustParseGeneric(MinKubernetesVersion)
	if requested == "" {
		requested = MinKubernetesVersion
	}
	v, err := version.ParseGeneric(requested)
	if err != nil {
		return celTarget{}, fmt.Errorf("Options.MinKubernetesVersion %q: %w", requested, err)
	}
	v = version.MajorMinor(v.Major(), v.Minor())
	if v.LessThan(floor) {
		return celTarget{}, fmt.Errorf("Options.MinKubernetesVersion %s is below crdgen's floor %s: it can be raised, not lowered", v, floor)
	}
	t := celTarget{kube: v}
	if err := t.librariesCover(); err != nil {
		return celTarget{}, err
	}
	return t, nil
}

// librariesCover reports an error when the linked k8s.io libraries cannot validate CEL for the
// target exactly.
//
// Two ways they can fall short. The libraries may not know a CEL library the target accepts: they
// know what was introduced up to their own release (DefaultKubeBinaryVersion). And validateCRD
// compiles at the LINKED compatibility version, so if that is older than the target's and differs
// from it, validateCRD would refuse a function the target accepts. Both would make crdgen refuse a
// CRD the target cluster installs, so both refuse the target instead, naming the bump that fixes it.
func (t celTarget) librariesCover() error {
	return t.librariesCoverWith(version.MustParseGeneric(basecompat.DefaultKubeBinaryVersion), environment.DefaultCompatibilityVersion())
}

// librariesCoverWith is librariesCover with the linked versions as parameters, so a test can put the
// libraries behind the target without relinking them.
func (t celTarget) librariesCoverWith(linked, linkedCompat *version.Version) error {
	if t.compat().GreaterThan(linked) {
		return fmt.Errorf("crdgen cannot validate CEL for Kubernetes %s: its k8s.io libraries are %s and do not know "+
			"the CEL functions a %s apiserver accepts; bump k8s.io/apiextensions-apiserver to %s or later", t.kube, linked, t.kube, t.compat())
	}
	if t.compat().GreaterThan(linkedCompat) && !sameCELSurface(t.compat(), linkedCompat) {
		return fmt.Errorf("crdgen cannot validate CEL for Kubernetes %s: the apiserver validation it runs compiles at "+
			"compatibility %s, which lacks CEL functions a %s apiserver accepts; bump k8s.io/apiextensions-apiserver to %s or later",
			t.kube, linkedCompat, t.kube, t.kube)
	}
	return nil
}

var celSurfaces sync.Map // "major.minor" -> map[string]bool

// celSurface is every function overload and macro the new-expressions environment at compatibility
// version v declares. Two versions with the same surface accept the same rules.
func celSurface(v *version.Version) map[string]bool {
	key := fmt.Sprintf("%d.%d", v.Major(), v.Minor())
	if s, ok := celSurfaces.Load(key); ok {
		return s.(map[string]bool)
	}
	env := environment.MustBaseEnvSet(v).NewExpressionsEnv()
	s := map[string]bool{}
	// Overload IDs, not function names: every new function brings an overload, and a library can
	// also add an overload to an existing name.
	for _, fn := range env.Functions() {
		for _, o := range fn.OverloadDecls() {
			s["overload:"+o.ID()] = true
		}
	}
	for _, m := range env.Macros() {
		s["macro:"+m.MacroKey()] = true
	}
	celSurfaces.Store(key, s)
	return s
}

func sameCELSurface(a, b *version.Version) bool {
	sa, sb := celSurface(a), celSurface(b)
	if len(sa) != len(sb) {
		return false
	}
	for k := range sa {
		if !sb[k] {
			return false
		}
	}
	return true
}

// celFloorErrors compiles every x-kubernetes-validations rule (and messageExpression) in the schema
// in the environment an apiserver of the target version uses for new expressions, and reports each
// one it refuses, with the version that would accept it.
//
// It runs AFTER the apiserver's own validation has passed, so the rules are well-formed and priced;
// all this adds is the question validateCRD cannot ask, because its environment is the linked
// libraries' rather than the target's. The walk mirrors the apiserver's: x-kubernetes-validations
// are only legal on properties, items and additionalProperties, and the type information comes from
// the apiserver's own CEL schema contexts.
func celFloorErrors(root *apiextensions.JSONSchemaProps, rootPath *field.Path, t celTarget) field.ErrorList {
	if root == nil {
		return nil
	}
	envSet := environment.MustBaseEnvSet(t.compat())
	var errs field.ErrorList
	var walk func(s *apiextensions.JSONSchemaProps, ctx *validation.CELSchemaContext, p *field.Path)
	walk = func(s *apiextensions.JSONSchemaProps, ctx *validation.CELSchemaContext, p *field.Path) {
		if len(s.XValidations) > 0 {
			if ti, err := ctx.TypeInfo(); err == nil && ti != nil {
				results, err := schemacel.Compile(ti.Schema, ti.DeclType, celconfig.PerCallLimit, envSet, schemacel.NewExpressionsEnvLoader())
				if err == nil {
					for i, r := range results {
						rp := p.Child("x-kubernetes-validations").Index(i)
						if r.Error != nil {
							errs = append(errs, field.Invalid(rp.Child("rule"), s.XValidations[i].Rule, t.refusal(ti, s.XValidations[i].Rule, r.Error.Detail)))
						}
						if r.MessageExpressionError != nil {
							errs = append(errs, field.Invalid(rp.Child("messageExpression"), s.XValidations[i].MessageExpression, t.refusal(ti, "", r.MessageExpressionError.Detail)))
						}
					}
				}
			}
		}
		names := make([]string, 0, len(s.Properties))
		for name := range s.Properties {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			prop := s.Properties[name]
			walk(&prop, ctx.ChildPropertyContext(&prop, name), p.Child("properties").Key(name))
		}
		if s.Items != nil && s.Items.Schema != nil {
			walk(s.Items.Schema, ctx.ChildItemsContext(s.Items.Schema), p.Child("items"))
		}
		if s.AdditionalProperties != nil && s.AdditionalProperties.Schema != nil {
			walk(s.AdditionalProperties.Schema, ctx.ChildAdditionalPropertiesContext(s.AdditionalProperties.Schema), p.Child("additionalProperties"))
		}
	}
	walk(root, validation.RootCELContext(root), rootPath)
	return errs
}

// refusal explains a rule the target refuses: the compiler's own reason, and the first Kubernetes
// version whose apiserver accepts it, found by compiling at each newer compatibility version the
// linked libraries know. rule is "" for a messageExpression, which is only reported, not searched.
func (t celTarget) refusal(ti *validation.CELTypeInfo, rule, detail string) string {
	msg := fmt.Sprintf("a Kubernetes %s apiserver refuses this (%s); crdgen targets Kubernetes %s (MinKubernetesVersion)", t.kube, detail, t.kube)
	if rule == "" {
		return msg
	}
	linked := version.MustParseGeneric(basecompat.DefaultKubeBinaryVersion)
	for c := t.compat().AddMinor(1); !c.GreaterThan(linked); c = c.AddMinor(1) {
		probe := *ti.Schema
		probe.XValidations = apiextensionsv1.ValidationRules{{Rule: rule}}
		results, err := schemacel.Compile(&probe, ti.DeclType, celconfig.PerCallLimit, environment.MustBaseEnvSet(c), schemacel.NewExpressionsEnvLoader())
		if err == nil && len(results) == 1 && results[0].Error == nil {
			return fmt.Sprintf("%s; it is accepted from Kubernetes %s", msg, c.AddMinor(1))
		}
	}
	return msg
}
