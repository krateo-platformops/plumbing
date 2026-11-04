package dialguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE "NO DIAL BYPASSES THE DIALER" ARM, which Diego named explicitly on snowplow#1131 and which
// is the only test here that could have caught the actual gap.
//
// The policy tests exercise DenyAddr. They say nothing about whether any real dial reaches it, and
// before this change TWO of plumbing's five dial paths had no dialer at all — jwtutil/jwks.go built
// a bare &http.Client{} and http/util/retry.go fell back to http.DefaultClient, both of which use
// http.DefaultTransport and its unguarded zero-value dialer. A third, helm/getter/getter.go, built
// an &http.Transport{} with no DialContext on the very path that fetches a chart from a
// user-written URL. A check installed only where a dialer already existed would have looked
// complete and left the #306 path open.
//
// The lesson is the one the ruling cites from oasgen: "a check in a builder that some call sites
// bypass looks complete and isn't". So this walks the whole module and fails on any construction
// that can dial without the hook.

// allowedUnguarded lists files permitted to construct a transport or client without the hook, with
// the reason. Deliberately tiny: an entry here is a hole in the policy, so adding one should feel
// expensive.
var allowedUnguarded = map[string]string{
	"dialguard/dialguard.go": "this package IS the guard; its own Transport/Client install the hook",
}

func TestNoUnguardedTransportOrClientInPlumbing(t *testing.T) {
	root := ".."

	var offenders []string
	var scanned, transports, clients int

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// `path != root` is load-bearing: the walk root is ".." and its own Name() is "..",
			// which the dot-prefix filter below matched — so the very first callback returned
			// SkipDir and the walk scanned ZERO files while reporting zero offenders. The
			// denominator assertion at the end is what caught it; without that, this test would
			// have passed forever against a module it never looked at.
			if n := info.Name(); path != root && (n == "vendor" || n == ".git" || strings.HasPrefix(n, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if _, ok := allowedUnguarded[rel]; ok {
			return nil
		}
		scanned++

		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}

		ast.Inspect(f, func(n ast.Node) bool {
			// http.DefaultClient / http.DefaultTransport: both carry the unguarded shared dialer.
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "http" {
					if sel.Sel.Name == "DefaultClient" || sel.Sel.Name == "DefaultTransport" {
						offenders = append(offenders, rel+": uses http."+sel.Sel.Name+
							" (unguarded shared dialer)")
					}
				}
			}

			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "http" {
				return true
			}

			switch sel.Sel.Name {
			case "Transport":
				transports++
				if !hasKey(lit, "DialContext") {
					offenders = append(offenders, rel+
						": &http.Transport{} with no DialContext (falls back to the zero-value dialer)")
				}
			case "Client":
				clients++
				if !hasKey(lit, "Transport") {
					offenders = append(offenders, rel+
						": &http.Client{} with no Transport (falls back to http.DefaultTransport)")
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}

	// THE DENOMINATOR. A walk that reached nothing reports no offenders, which is identical to a
	// clean result. The transport/client counts are the positive control: this change wired five
	// sites, so if the walk finds zero constructions of either it is not looking at the module.
	t.Logf("scanned %d non-test .go files; found %d &http.Transport{} and %d &http.Client{} literals",
		scanned, transports, clients)
	if scanned == 0 {
		t.Fatal("scanned 0 files — the walk is broken and this test asserts nothing")
	}
	if transports+clients == 0 {
		t.Fatal("found no &http.Transport{} or &http.Client{} literals anywhere. Either the module " +
			"moved or the AST matching is wrong; a zero here is a claim about this test, not about " +
			"the module")
	}

	if len(offenders) > 0 {
		t.Errorf("%d construction(s) can dial without the destination policy:\n  %s\n\n"+
			"Each one bypasses dialguard entirely — the policy tests would still pass while these "+
			"dials reach 169.254.169.254. Use dialguard.Transport() / dialguard.Client(), or add an "+
			"entry to allowedUnguarded with a reason if the dial genuinely cannot carry a "+
			"user-influenced destination.",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

func hasKey(lit *ast.CompositeLit, key string) bool {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if id, ok := kv.Key.(*ast.Ident); ok && id.Name == key {
			return true
		}
	}
	return false
}
