package flow

import (
	"reflect"
	"testing"
)

func TestTemplateInterpolation(t *testing.T) {
	s := newScope(map[string]any{
		"config": map[string]any{"q": "widgets", "page": float64(2)},
	})
	got, err := renderTemplateString("search?q={{urlencode(config.q)}}&page={{config.page}}", s)
	if err != nil {
		t.Fatal(err)
	}
	if got != "search?q=widgets&page=2" {
		t.Fatalf("got %q", got)
	}
}

// TestTemplateSingleExprKeepsType proves a whole-template single expression yields
// the underlying typed value, not a stringified one, so a body field can be a list
// or number.
func TestTemplateSingleExprKeepsType(t *testing.T) {
	s := newScope(map[string]any{
		"steps": map[string]any{"x": map[string]any{"items": []any{float64(1), float64(2)}}},
	})
	tpl, err := parseTemplate("{{steps.x.items}}")
	if err != nil {
		t.Fatal(err)
	}
	v, err := tpl.renderValue(s)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{float64(1), float64(2)}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("got %#v", v)
	}
}

func TestDeepTemplate(t *testing.T) {
	s := newScope(map[string]any{
		"config": map[string]any{"name": "ion", "count": float64(3)},
	})
	in := map[string]any{
		"label": "hi {{config.name}}",
		"total": "{{config.count}}",
		"nested": map[string]any{
			"list": []any{"{{config.name}}", "static"},
		},
	}
	got, err := deepTemplate(in, s)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"label": "hi ion",
		"total": float64(3), // single-expr keeps the number type
		"nested": map[string]any{
			"list": []any{"ion", "static"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestTemplateUnclosedBraceErrors(t *testing.T) {
	if _, err := parseTemplate("a {{ b "); err == nil {
		t.Fatal("expected an unclosed brace error")
	}
}
