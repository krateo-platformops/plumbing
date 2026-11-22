package schemas

import (
	"encoding/json"
	"testing"
)

// plumbing#227: `"const": null` is told apart from no const at all.
func TestConstNullIsToldApartFromNoConst(t *testing.T) {
	for _, c := range []struct {
		in        string
		constNull bool
		constVal  any
	}{
		{`{"const": null}`, true, nil},
		{`{"const":null,"type":"string"}`, true, nil},
		{`{}`, false, nil},
		{`{"const": 1}`, false, float64(1)},
		{`{"const": "null"}`, false, "null"},
		{`true`, false, nil},
	} {
		var ty Type
		if err := json.Unmarshal([]byte(c.in), &ty); err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if ty.ConstNull != c.constNull || ty.Const != c.constVal {
			t.Errorf("%s: ConstNull=%v Const=%#v, want %v %#v", c.in, ty.ConstNull, ty.Const, c.constNull, c.constVal)
		}
	}
}
