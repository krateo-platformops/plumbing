package crdgen

import (
	"reflect"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// oldSelfSpec carries transition rules in every place a chart author can put one: the root, a
// property, a property of a map's values, and a property of a keyed list's items. `full` sets every
// field a rule has.
const oldSelfSpec = `{"type":"object",
 "x-kubernetes-validations":[{"rule":"!has(oldSelf.region) || has(self.region)","message":"region can't be removed"}],
 "properties":{
  "region":{"type":"string","maxLength":16,
    "x-kubernetes-validations":[{"rule":"self == oldSelf","message":"region can't change"}]},
  "full":{"type":"integer","maximum":100,
    "x-kubernetes-validations":[{"rule":"oldSelf.hasValue() || self == 0","message":"full starts at 0",
      "messageExpression":"self > 50 ? 'full starts at 0, not that high' : 'full starts at 0, not above it'","reason":"FieldValueForbidden","optionalOldSelf":true}]},
  "off":{"type":"integer",
    "x-kubernetes-validations":[{"rule":"self >= oldSelf","optionalOldSelf":false}]},
  "parent":{"type":"object","properties":{"child":{"type":"string","maxLength":16}},
    "x-kubernetes-validations":[{"rule":"!has(oldSelf.child) || has(self.child)","fieldPath":".child"}]},
  "byName":{"type":"object","maxProperties":4,"additionalProperties":{"type":"object","properties":{
    "size":{"type":"integer","x-kubernetes-validations":[{"rule":"self >= oldSelf","message":"size can't shrink"}]}}}},
  "items":{"type":"array","maxItems":4,"x-kubernetes-list-type":"map","x-kubernetes-list-map-keys":["name"],
    "items":{"type":"object","required":["name"],"properties":{
      "name":{"type":"string","maxLength":16},
      "size":{"type":"integer","x-kubernetes-validations":[{"rule":"self >= oldSelf","message":"item size can't shrink"}]}}}}}}`

// A transition rule reaches the CRD as its author wrote it: every field, wherever it sits. Dropping
// optionalOldSelf would be the worst loss, as the apiserver then never runs the rule on create.
func TestOldSelfRulesReachTheCRDUnchanged(t *testing.T) {
	out, err := Generate(Options{Group: "composition.krateo.io", Version: "v1", Kind: "OldSelf", SpecSchema: []byte(oldSelfSpec)})
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(out, &crd); err != nil {
		t.Fatal(err)
	}
	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	forbidden, yes, no := apiextensionsv1.FieldValueForbidden, true, false
	for where, c := range map[string]struct {
		got  apiextensionsv1.ValidationRules
		want apiextensionsv1.ValidationRule
	}{
		"root": {spec.XValidations,
			apiextensionsv1.ValidationRule{Rule: "!has(oldSelf.region) || has(self.region)", Message: "region can't be removed"}},
		"region": {spec.Properties["region"].XValidations,
			apiextensionsv1.ValidationRule{Rule: "self == oldSelf", Message: "region can't change"}},
		"full": {spec.Properties["full"].XValidations,
			apiextensionsv1.ValidationRule{Rule: "oldSelf.hasValue() || self == 0", Message: "full starts at 0",
				MessageExpression: "self > 50 ? 'full starts at 0, not that high' : 'full starts at 0, not above it'", Reason: &forbidden, OptionalOldSelf: &yes}},
		"off": {spec.Properties["off"].XValidations,
			apiextensionsv1.ValidationRule{Rule: "self >= oldSelf", OptionalOldSelf: &no}},
		"parent": {spec.Properties["parent"].XValidations,
			apiextensionsv1.ValidationRule{Rule: "!has(oldSelf.child) || has(self.child)", FieldPath: ".child"}},
		"map value": {spec.Properties["byName"].AdditionalProperties.Schema.Properties["size"].XValidations,
			apiextensionsv1.ValidationRule{Rule: "self >= oldSelf", Message: "size can't shrink"}},
		"list item": {spec.Properties["items"].Items.Schema.Properties["size"].XValidations,
			apiextensionsv1.ValidationRule{Rule: "self >= oldSelf", Message: "item size can't shrink"}},
	} {
		if !reflect.DeepEqual(c.got, apiextensionsv1.ValidationRules{c.want}) {
			t.Errorf("%s: rules = %+v, want only %+v", where, c.got, c.want)
		}
	}
}

// Without optionalOldSelf, oldSelf is the plain value: a rule written for the optional one doesn't
// compile, and Generate says so instead of shipping a CRD the apiserver refuses.
func TestOptionalOldSelfRuleNeedsTheFlag(t *testing.T) {
	const tmpl = `{"type":"object","properties":{"full":{"type":"integer","x-kubernetes-validations":[{"rule":"oldSelf.hasValue() || self == 0"FLAG}]}}}`
	gen := func(flag string) error {
		_, err := Generate(Options{Group: "composition.krateo.io", Version: "v1", Kind: "OldSelf", SpecSchema: []byte(strings.Replace(tmpl, "FLAG", flag, 1))})
		return err
	}
	if err := gen(`,"optionalOldSelf":true`); err != nil {
		t.Errorf("control: with the flag: %v", err)
	}
	for _, flag := range []string{``, `,"optionalOldSelf":false`} {
		if err := gen(flag); err == nil || !strings.Contains(err.Error(), "hasValue") {
			t.Errorf("flag %q: err = %v, want the rule refused for hasValue", flag, err)
		}
	}
}

// The items of a list with no key have no old item to compare with. The apiserver refuses a CRD with
// oldSelf there, so Generate refuses it, with the rule's path.
func TestOldSelfInAnUnkeyedListIsRefused(t *testing.T) {
	gen := func(rule string) error {
		_, err := Generate(Options{Group: "composition.krateo.io", Version: "v1", Kind: "OldSelf", SpecSchema: []byte(
			`{"type":"object","properties":{"items":{"type":"array","maxItems":4,"items":{"type":"integer","x-kubernetes-validations":[{"rule":"` + rule + `"}]}}}}`)})
		return err
	}
	if err := gen("self >= 0"); err != nil {
		t.Errorf("control: a rule without oldSelf: %v", err)
	}
	err := gen("self >= oldSelf")
	if err == nil || !strings.Contains(err.Error(), "properties[items].items.x-kubernetes-validations[0].rule") || !strings.Contains(err.Error(), "uncorrelatable") {
		t.Errorf("err = %v, want the rule's path and that oldSelf is uncorrelatable there", err)
	}
}
