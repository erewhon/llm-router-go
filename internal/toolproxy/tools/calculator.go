package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	"github.com/PaesslerAG/gval"
)

// CalculatorName is the registered tool name. Other packages reference
// this constant rather than hard-coding the string.
const CalculatorName = "calculator"

// calculatorDescription matches the Python tool wording so models
// trained on either stack get consistent prompting.
const calculatorDescription = "Evaluate a mathematical expression. " +
	"Supports arithmetic (+, -, *, /, **, %), " +
	"functions (sqrt, sin, cos, tan, asin, acos, atan, log, log10, log2, exp, abs, " +
	"round, ceil, floor, factorial), and constants (pi, e, tau, inf). " +
	"Use this for any calculation."

var calculatorParameters = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"expression": map[string]any{
			"type":        "string",
			"description": "The mathematical expression to evaluate (e.g. 'sqrt(144) + 2**10')",
		},
	},
	"required": []string{"expression"},
}

// Calculator returns the Tool definition for the safe-math calculator.
func Calculator() Tool {
	lang := calculatorLanguage()
	return Tool{
		Name:        CalculatorName,
		Description: calculatorDescription,
		Parameters:  calculatorParameters,
		Run: func(ctx context.Context, arguments string) string {
			var args struct {
				Expression string `json:"expression"`
			}
			if err := json.Unmarshal([]byte(arguments), &args); err != nil {
				return fmt.Sprintf("Invalid arguments: %s", arguments)
			}
			return evalExpression(lang, args.Expression)
		},
	}
}

// calculatorLanguage assembles the gval language allowed to the tool —
// arithmetic + a curated set of math functions/constants. Anything
// outside this set fails to parse, so the calculator can't be coerced
// into reading globals, calling unsafe methods, etc.
func calculatorLanguage() gval.Language {
	return gval.NewLanguage(
		gval.Arithmetic(),
		gval.PropositionalLogic(),
		gval.Parentheses(),
		gval.Constant("pi", math.Pi),
		gval.Constant("e", math.E),
		gval.Constant("tau", 2*math.Pi),
		gval.Constant("inf", math.Inf(1)),
		gval.Function("sqrt", oneArg(math.Sqrt)),
		gval.Function("sin", oneArg(math.Sin)),
		gval.Function("cos", oneArg(math.Cos)),
		gval.Function("tan", oneArg(math.Tan)),
		gval.Function("asin", oneArg(math.Asin)),
		gval.Function("acos", oneArg(math.Acos)),
		gval.Function("atan", oneArg(math.Atan)),
		gval.Function("log", oneArg(math.Log)),
		gval.Function("log10", oneArg(math.Log10)),
		gval.Function("log2", oneArg(math.Log2)),
		gval.Function("exp", oneArg(math.Exp)),
		gval.Function("abs", oneArg(math.Abs)),
		gval.Function("ceil", oneArg(math.Ceil)),
		gval.Function("floor", oneArg(math.Floor)),
		gval.Function("round", oneArg(func(x float64) float64 { return math.Round(x) })),
		gval.Function("factorial", factorial),
	)
}

func evalExpression(lang gval.Language, expr string) string {
	if expr == "" {
		return "Invalid expression: empty"
	}
	v, err := lang.Evaluate(expr, nil)
	if err != nil {
		return "Invalid expression: " + err.Error()
	}
	return formatResult(v)
}

// formatResult turns a numeric result into the same shape Python emits:
// integers without a trailing ".0", floats with their natural string
// representation. Booleans pass through too (PropositionalLogic).
func formatResult(v any) string {
	switch x := v.(type) {
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) {
			return strconv.FormatFloat(x, 'g', -1, 64)
		}
		if x == math.Trunc(x) && math.Abs(x) < 1e18 {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case int, int64:
		return fmt.Sprintf("%d", x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case string:
		return x
	default:
		return fmt.Sprintf("%v", x)
	}
}

// oneArg adapts a math.Float64-style function to gval.Function's signature.
// gval delivers numeric args as float64.
func oneArg(f func(float64) float64) func(args ...any) (any, error) {
	return func(args ...any) (any, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("expected 1 argument, got %d", len(args))
		}
		x, ok := toFloat(args[0])
		if !ok {
			return nil, fmt.Errorf("expected a number, got %T", args[0])
		}
		return f(x), nil
	}
}

// factorial accepts any non-negative integer-valued number and returns
// the factorial as a float64 (matches Python's int-promotion for the
// numbers we'd realistically see in a chat).
func factorial(args ...any) (any, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("factorial: expected 1 argument, got %d", len(args))
	}
	x, ok := toFloat(args[0])
	if !ok {
		return nil, fmt.Errorf("factorial: expected a number, got %T", args[0])
	}
	if x < 0 || x != math.Trunc(x) {
		return nil, fmt.Errorf("factorial: requires a non-negative integer")
	}
	if x > 170 {
		// 171! overflows float64.
		return nil, fmt.Errorf("factorial: argument too large (max 170)")
	}
	r := 1.0
	for i := 2.0; i <= x; i++ {
		r *= i
	}
	return r, nil
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint:
		return float64(x), true
	case uint64:
		return float64(x), true
	}
	return 0, false
}
