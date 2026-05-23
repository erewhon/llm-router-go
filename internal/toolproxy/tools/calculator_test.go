package tools

import (
	"context"
	"strings"
	"testing"
)

func runCalc(t *testing.T, expr string) string {
	t.Helper()
	c := Calculator()
	return c.Run(context.Background(), `{"expression":"`+expr+`"}`)
}

func TestCalculator_Arithmetic(t *testing.T) {
	cases := map[string]string{
		"2+2":           "4",
		"100/4":         "25",
		"2**10":         "1024",
		"3*(4+5)":       "27",
		"7%3":           "1",
		"-3+8":          "5",
		"100.0 / 4.0":   "25",
		"0.1 + 0.2":     "0.30000000000000004", // canonical float arithmetic
	}
	for expr, want := range cases {
		got := runCalc(t, expr)
		if got != want {
			t.Errorf("%q = %q, want %q", expr, got, want)
		}
	}
}

func TestCalculator_Functions(t *testing.T) {
	cases := []struct{ expr, want string }{
		{"sqrt(144)", "12"},
		{"sqrt(2)", "1.4142135623730951"},
		{"abs(-7)", "7"},
		{"ceil(3.2)", "4"},
		{"floor(3.9)", "3"},
		{"round(3.5)", "4"},
		{"log10(1000)", "3"},
		{"log2(1024)", "10"},
		{"exp(0)", "1"},
		{"factorial(5)", "120"},
		{"factorial(10)", "3628800"},
	}
	for _, c := range cases {
		got := runCalc(t, c.expr)
		if got != c.want {
			t.Errorf("%q = %q, want %q", c.expr, got, c.want)
		}
	}
}

func TestCalculator_Constants(t *testing.T) {
	// pi rounded to 2 dp should match 3.14.
	got := runCalc(t, "round(pi*100)/100")
	if got != "3.14" {
		t.Errorf("round(pi*100)/100 = %q, want 3.14", got)
	}
	// e ≈ 2.71828
	got = runCalc(t, "round(e*1000)/1000")
	if got != "2.718" {
		t.Errorf("round(e*1000)/1000 = %q, want 2.718", got)
	}
}

func TestCalculator_CompoundExpression(t *testing.T) {
	// Sample from the tool description.
	got := runCalc(t, "sqrt(144) + 2**10")
	if got != "1036" {
		t.Errorf("sqrt(144)+2**10 = %q, want 1036", got)
	}
}

func TestCalculator_InvalidExpression(t *testing.T) {
	got := runCalc(t, "this is not math")
	if !strings.HasPrefix(got, "Invalid expression") {
		t.Errorf("garbage input returned %q, want Invalid expression…", got)
	}
}

func TestCalculator_DivideByZero(t *testing.T) {
	// gval evaluates 1/0 to +Inf in float context; our formatter
	// renders that as "+Inf". Confirm we don't panic.
	got := runCalc(t, "1/0")
	if got == "" {
		t.Errorf("1/0 returned empty string")
	}
	// Don't assert the exact text — gval's behaviour here is its own.
}

func TestCalculator_Factorial_RejectsNegative(t *testing.T) {
	got := runCalc(t, "factorial(-3)")
	if !strings.Contains(got, "non-negative") {
		t.Errorf("factorial(-3) returned %q", got)
	}
}

func TestCalculator_Factorial_RejectsFloat(t *testing.T) {
	got := runCalc(t, "factorial(3.5)")
	if !strings.Contains(got, "non-negative") {
		t.Errorf("factorial(3.5) returned %q", got)
	}
}

func TestCalculator_Factorial_OverflowGuard(t *testing.T) {
	got := runCalc(t, "factorial(200)")
	if !strings.Contains(got, "too large") {
		t.Errorf("factorial(200) returned %q, want too-large error", got)
	}
}

func TestCalculator_MissingExpression(t *testing.T) {
	c := Calculator()
	got := c.Run(context.Background(), `{"expression":""}`)
	if !strings.Contains(got, "empty") {
		t.Errorf("empty expression returned %q", got)
	}
}

func TestCalculator_BadArguments(t *testing.T) {
	c := Calculator()
	got := c.Run(context.Background(), "{not json}")
	if !strings.Contains(got, "Invalid arguments") {
		t.Errorf("bad JSON returned %q", got)
	}
}

func TestCalculator_RegisteredShape(t *testing.T) {
	r := NewRegistry()
	r.Register(Calculator())

	if !r.Has(CalculatorName) {
		t.Fatalf("calculator not registered")
	}
	defs := r.Definitions()
	if len(defs) != 1 {
		t.Fatalf("Definitions len = %d, want 1", len(defs))
	}
	fn, _ := defs[0]["function"].(map[string]any)
	if fn["name"] != CalculatorName {
		t.Errorf("function name = %q, want %q", fn["name"], CalculatorName)
	}
	params, _ := fn["parameters"].(map[string]any)
	if params["type"] != "object" {
		t.Errorf("parameters.type = %v, want object", params["type"])
	}
	required, _ := params["required"].([]string)
	if len(required) != 1 || required[0] != "expression" {
		t.Errorf("required = %v, want [expression]", required)
	}
}
