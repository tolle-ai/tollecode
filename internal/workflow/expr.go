package workflow

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/expr-lang/expr"
)

var (
	// tmplRe matches a single {{ ... }} template occurrence.
	tmplRe = regexp.MustCompile(`\{\{\s*(.*?)\s*\}\}`)
	// dollarRe strips an n8n-style `$` prefix from an identifier so expr-lang can
	// parse it: $json -> json, $node -> node, $vars -> vars, $item -> item.
	dollarRe = regexp.MustCompile(`\$([\p{L}_])`)
)

// evalEnv is the environment a node expression is evaluated against.
type evalEnv struct {
	input   any            // assembled input to the current node ($json / data)
	vars    map[string]any // workflow variables ($vars / vars)
	nodes   map[string]any // label|id -> {json: <node output>}  ($node)
	trigger map[string]any // {payload: <trigger payload>}
}

func (e evalEnv) toMap() map[string]any {
	return map[string]any{
		"json":    e.input,
		"data":    e.input, // back-compat with desktop {{data.x}}
		"vars":    e.vars,  // back-compat with desktop {{vars.x}}
		"node":    e.nodes,
		"trigger": e.trigger,
	}
}

// normalizeExpr rewrites n8n-style references so expr-lang can parse them.
func normalizeExpr(src string) string {
	return dollarRe.ReplaceAllString(src, "$1")
}

// prepCondition best-effort adapts a desktop-authored JS condition to expr syntax:
// strips {{ }} wrappers and rewrites strict equality operators.
func prepCondition(src string) string {
	src = strings.ReplaceAll(src, "{{", "")
	src = strings.ReplaceAll(src, "}}", "")
	src = strings.ReplaceAll(src, "===", "==")
	src = strings.ReplaceAll(src, "!==", "!=")
	return normalizeExpr(src)
}

// evalExpr compiles and evaluates a single expression against env.
func evalExpr(src string, env evalEnv) (any, error) {
	src = strings.TrimSpace(normalizeExpr(src))
	if src == "" {
		return nil, nil
	}
	program, err := expr.Compile(src, expr.AllowUndefinedVariables())
	if err != nil {
		return nil, fmt.Errorf("expression compile: %w", err)
	}
	out, err := expr.Run(program, env.toMap())
	if err != nil {
		return nil, fmt.Errorf("expression eval: %w", err)
	}
	return out, nil
}

// evalBool evaluates src as a boolean for condition/switch nodes.
func evalBool(src string, env evalEnv) bool {
	program, err := expr.Compile(strings.TrimSpace(prepCondition(src)), expr.AllowUndefinedVariables())
	if err != nil {
		return false
	}
	out, err := expr.Run(program, env.toMap())
	if err != nil {
		return false
	}
	return truthy(out)
}

// truthy mirrors JS-ish truthiness for expression results.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	default:
		return true
	}
}

// interpolate replaces {{ ... }} in s. If s is EXACTLY one {{expr}}, the raw typed
// value is returned (arrays/objects/numbers survive); otherwise each match is
// stringified and spliced into the surrounding text.
func interpolate(s string, env evalEnv) any {
	trimmed := strings.TrimSpace(s)
	if locs := tmplRe.FindAllStringIndex(trimmed, -1); len(locs) == 1 &&
		locs[0][0] == 0 && locs[0][1] == len(trimmed) {
		inner := tmplRe.FindStringSubmatch(trimmed)[1]
		out, err := evalExpr(inner, env)
		if err != nil {
			return ""
		}
		return out
	}
	return tmplRe.ReplaceAllStringFunc(s, func(match string) string {
		inner := tmplRe.FindStringSubmatch(match)[1]
		out, err := evalExpr(inner, env)
		if err != nil {
			return ""
		}
		return stringify(out)
	})
}

// interpolateStr is interpolate but always coerces to a string.
func interpolateStr(s string, env evalEnv) string {
	return stringify(interpolate(s, env))
}

// stringify coerces an expression result to a string for embedding.
func stringify(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		// Render integers without a trailing .0.
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	case int, int64:
		return fmt.Sprintf("%d", x)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprintf("%v", x)
		}
		return string(b)
	}
}

// toSlice coerces an expression result to a []any for loop iteration.
func toSlice(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case nil:
		return nil
	default:
		// Marshal round-trip handles typed slices ([]string, []map, etc.).
		b, err := json.Marshal(x)
		if err != nil {
			return nil
		}
		var out []any
		if json.Unmarshal(b, &out) == nil {
			return out
		}
		return nil
	}
}
