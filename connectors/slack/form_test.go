package main

import "testing"

func validField() map[string]any {
	return map[string]any{"name": "env", "type": "select", "options": []any{"staging", "prod"}}
}

func TestParseFormNil(t *testing.T) {
	f, err := ParseForm(nil)
	if err != nil || f != nil {
		t.Fatalf("ParseForm(nil) = %v, %v", f, err)
	}
}

func TestParseFormBasic(t *testing.T) {
	f, err := ParseForm(map[string]any{
		"title": "Deploy", "submit": "Go",
		"fields": []any{validField(), map[string]any{"name": "notes", "type": "textarea", "optional": true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.title() != "Deploy" || f.submit() != "Go" {
		t.Fatalf("title/submit: %q/%q", f.title(), f.submit())
	}
	if len(f.Fields) != 2 {
		t.Fatalf("fields: %+v", f.Fields)
	}
}

func TestParseFormDefaults(t *testing.T) {
	f, err := ParseForm(map[string]any{"fields": []any{map[string]any{"name": "x", "type": "text"}}})
	if err != nil {
		t.Fatal(err)
	}
	if f.title() != "Conductor" || f.submit() != "Submit" {
		t.Fatalf("defaults: %q/%q", f.title(), f.submit())
	}
}

func TestFormValidateNeedsAtLeastOneField(t *testing.T) {
	if _, err := ParseForm(map[string]any{"fields": []any{}}); err == nil {
		t.Fatal("expected an error: no fields")
	}
}

func TestFormValidateFieldNameShape(t *testing.T) {
	for _, name := range []string{"Env", "1env", "env-prod", "", "e"} {
		valid := name == "e"
		_, err := ParseForm(map[string]any{"fields": []any{map[string]any{"name": name, "type": "text"}}})
		if valid && err != nil {
			t.Errorf("name %q should be valid: %v", name, err)
		}
		if !valid && err == nil {
			t.Errorf("name %q should be rejected", name)
		}
	}
}

func TestFormValidateDuplicateFieldNames(t *testing.T) {
	_, err := ParseForm(map[string]any{"fields": []any{
		map[string]any{"name": "env", "type": "text"},
		map[string]any{"name": "env", "type": "text"},
	}})
	if err == nil {
		t.Fatal("expected an error: duplicate field name")
	}
}

func TestFormValidateSelectNeedsOptions(t *testing.T) {
	_, err := ParseForm(map[string]any{"fields": []any{map[string]any{"name": "env", "type": "select"}}})
	if err == nil {
		t.Fatal("expected an error: select with no options")
	}
}

func TestFormValidateSelectDefaultMustBeAnOption(t *testing.T) {
	_, err := ParseForm(map[string]any{"fields": []any{
		map[string]any{"name": "env", "type": "select", "options": []any{"staging", "prod"}, "default": "bogus"},
	}})
	if err == nil {
		t.Fatal("expected an error: default not in options")
	}
}

func TestFormValidateSelectOptionNoTemplate(t *testing.T) {
	_, err := ParseForm(map[string]any{"fields": []any{
		map[string]any{"name": "env", "type": "select", "options": []any{"{{.evil}}"}},
	}})
	if err == nil {
		t.Fatal("expected an error: templated option value")
	}
}

func TestFormValidateUnknownType(t *testing.T) {
	_, err := ParseForm(map[string]any{"fields": []any{map[string]any{"name": "x", "type": "checkbox"}}})
	if err == nil {
		t.Fatal("expected an error: unknown field type")
	}
}

func TestFormValuesRequiredAndSelect(t *testing.T) {
	f, err := ParseForm(map[string]any{"fields": []any{
		validField(),
		map[string]any{"name": "notes", "type": "text"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]map[string]stateValue{
		"env": {formActionID: {SelectedOption: &struct {
			Value string "json:\"value\""
		}{Value: "prod"}}},
		"notes": {formActionID: {}},
	}
	_, errs := f.formValues(state)
	if _, ok := errs["notes"]; !ok {
		t.Fatalf("missing required text field should error, got %+v", errs)
	}

	state["notes"][formActionID] = stateValue{Value: "ship it"}
	vals, errs := f.formValues(state)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %+v", errs)
	}
	if vals["env"] != "prod" || vals["notes"] != "ship it" {
		t.Fatalf("vals: %+v", vals)
	}
}

func TestFormValuesRejectsOffListSelect(t *testing.T) {
	f, _ := ParseForm(map[string]any{"fields": []any{validField()}})
	state := map[string]map[string]stateValue{
		"env": {formActionID: {SelectedOption: &struct {
			Value string "json:\"value\""
		}{Value: "bogus"}}},
	}
	_, errs := f.formValues(state)
	if _, ok := errs["env"]; !ok {
		t.Fatal("an off-list select value must be rejected even though Slack only offers configured options")
	}
}
