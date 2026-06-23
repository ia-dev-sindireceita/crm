// Package airaw provides a static analyzer that forbids passing AI-derived
// data to templ.Raw / templ.Unsafe* helpers.
//
// Background: ADR 0077 §3.5 (SIN-62225) bans `templ.Raw(...)` on any value
// produced by the AI subtree (`internal/ai/...`) or by the
// `port.Summarizer.Summarize` method. This analyzer enforces that ban at
// build/CI time using SSA def-use chains so that an attempt to render LLM
// output as raw HTML fails the build, not just code review.
package airaw

import (
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/ssa"
)

// AIPackagePrefix matches every package whose import path lives under
// internal/ai/ in this module. Anything sourced there is presumed to be
// model-derived or model-adjacent and must NEVER be rendered as raw HTML.
//
// port.Summarizer.Summarize returns a domain.AISummary (declared in
// internal/ai/domain), so any subsequent field access on that result also
// resolves through this prefix and trips the analyzer.
const AIPackagePrefix = "github.com/pericles-luz/crm/internal/ai/"

// templRawNames lists the exact package-level functions in github.com/a-h/templ
// whose argument is written verbatim into the response. Each one is a
// candidate XSS vector when fed AI-derived data.
var templRawNames = map[string]struct{}{
	"Raw":          {},
	"Unsafe":       {},
	"UnsafeHTML":   {},
	"UnsafeAttr":   {},
	"UnsafeScript": {},
	"UnsafeURL":    {},
	"UnsafeCSS":    {},
}

// Analyzer is the airaw lint pass.
var Analyzer = &analysis.Analyzer{
	Name:     "airaw",
	Doc:      "report templ.Raw / templ.Unsafe* calls whose first argument originates in internal/ai/...",
	URL:      "https://pitho.local/SIN/issues/SIN-62237",
	Requires: []*analysis.Analyzer{buildssa.Analyzer},
	Run:      run,
}

func run(pass *analysis.Pass) (interface{}, error) {
	ssain, _ := pass.ResultOf[buildssa.Analyzer].(*buildssa.SSA)
	if ssain == nil {
		return nil, nil
	}
	for _, fn := range ssain.SrcFuncs {
		inspectFunction(pass, fn)
	}
	return nil, nil
}

func inspectFunction(pass *analysis.Pass, fn *ssa.Function) {
	for _, b := range fn.Blocks {
		for _, ins := range b.Instrs {
			call, ok := ins.(ssa.CallInstruction)
			if !ok {
				continue
			}
			callee := call.Common().StaticCallee()
			if callee == nil || !isTemplRawCallee(callee) {
				continue
			}
			args := call.Common().Args
			if len(args) == 0 {
				continue
			}
			label, ok := findAIOrigin(args[0], make(map[ssa.Value]bool))
			if !ok {
				continue
			}
			pass.Reportf(call.Pos(),
				"airaw: %s called with AI-derived value (origin: %s); render via internal/ai/render.SafeText instead",
				calleeQualifiedName(callee), label,
			)
		}
	}
}

// isTemplRawCallee reports whether fn is one of the banned templ helpers.
func isTemplRawCallee(fn *ssa.Function) bool {
	obj := fn.Object()
	if obj == nil || obj.Pkg() == nil {
		return false
	}
	if obj.Pkg().Path() != "github.com/a-h/templ" {
		return false
	}
	_, ok := templRawNames[obj.Name()]
	return ok
}

// calleeQualifiedName renders an "templ.Raw" / "templ.Unsafe" style label for
// the diagnostic message.
func calleeQualifiedName(fn *ssa.Function) string {
	pkg := "templ"
	name := fn.Name()
	if i := strings.IndexByte(name, '['); i >= 0 {
		name = name[:i]
	}
	if obj := fn.Object(); obj != nil && obj.Pkg() != nil {
		pkg = obj.Pkg().Name()
	}
	return pkg + "." + name
}

// findAIOrigin walks the SSA def-use chain backwards from v looking for any
// value that originates in the AI subtree. It returns a human-readable label
// for the diagnostic when a match is found.
//
// Positive matches:
//
//   - *ssa.Call whose static callee lives in the AI subtree.
//   - *ssa.FieldAddr whose receiver type is declared in the AI subtree
//     (e.g. AISummary.Summary, Suggestion.Body) — this also catches
//     port.Summarizer.Summarize results because every field access on the
//     returned AISummary trips this branch.
//
// Pass-through instructions (Phi, Extract, Convert, ChangeType, UnOp, BinOp)
// are walked transitively. `seen` short-circuits SSA cycles introduced by Phi
// nodes.
func findAIOrigin(v ssa.Value, seen map[ssa.Value]bool) (string, bool) {
	if v == nil || seen[v] {
		return "", false
	}
	seen[v] = true

	switch x := v.(type) {

	case *ssa.Call:
		if callee := x.Call.StaticCallee(); callee != nil {
			if obj := callee.Object(); obj != nil && obj.Pkg() != nil && isAIPath(obj.Pkg().Path()) {
				return obj.Pkg().Name() + "." + obj.Name() + "()", true
			}
		}

	case *ssa.Phi:
		for _, e := range x.Edges {
			if label, ok := findAIOrigin(e, seen); ok {
				return label, true
			}
		}

	case *ssa.Extract:
		return findAIOrigin(x.Tuple, seen)

	case *ssa.Convert:
		return findAIOrigin(x.X, seen)

	case *ssa.ChangeType:
		return findAIOrigin(x.X, seen)

	case *ssa.UnOp:
		return findAIOrigin(x.X, seen)

	case *ssa.FieldAddr:
		if label, ok := aiStructFieldLabel(x.X.Type(), int(x.Field)); ok {
			return label, true
		}
		return findAIOrigin(x.X, seen)

	case *ssa.BinOp:
		if label, ok := findAIOrigin(x.X, seen); ok {
			return label, true
		}
		return findAIOrigin(x.Y, seen)
	}

	return "", false
}

func isAIPath(path string) bool {
	return strings.HasPrefix(path, AIPackagePrefix)
}

// aiStructFieldLabel returns "<typeName>.<fieldName>" if the Nth field of t
// (or t's pointee, if t is a pointer to struct) belongs to a struct declared
// inside the AI subtree.
func aiStructFieldLabel(t types.Type, field int) (string, bool) {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok {
		return "", false
	}
	st, ok := named.Underlying().(*types.Struct)
	if !ok || field < 0 || field >= st.NumFields() {
		return "", false
	}
	tn := named.Obj()
	if tn == nil || tn.Pkg() == nil || !isAIPath(tn.Pkg().Path()) {
		return "", false
	}
	return tn.Name() + "." + st.Field(field).Name(), true
}
