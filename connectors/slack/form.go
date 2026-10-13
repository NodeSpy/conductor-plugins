package main

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Form is a modal a message_shortcut or app_mention trigger collects before
// its workflow fires (#161 parity). The submitted values publish as
// {{.slack.form.<name>}}.
type Form struct {
	Title  string
	Submit string
	Fields []FormField
}

// FormField is one modal input. A select's submitted value is re-checked
// server-side against Options -- a value Slack did not offer never fires.
type FormField struct {
	Name     string
	Label    string
	Type     string // select | text | textarea
	Options  []string
	Default  string
	Optional bool
}

// Slack Block Kit limits the form validation enforces at load, so a config
// that validates never produces a views.open Slack rejects.
const (
	maxFormTitle     = 24  // modal title / submit label
	maxFormFields    = 20  // a sane cap well under Slack's 100 blocks
	maxSelectOptions = 100 // static_select
	maxOptionText    = 75  // option text
	maxOptionValue   = 150 // option value
	maxLabel         = 2000
	maxTextValue     = 3000 // plain_text_input
)

var formNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// ParseForm decodes and validates a trigger's `options.form` value (the
// generic YAML/JSON shape, already decoded to map[string]any by the host).
// nil, nil when absent.
func ParseForm(v any) (*Form, error) {
	if v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("form: want a map (title, submit, fields), got %T", v)
	}
	f := &Form{}
	for k, val := range m {
		switch k {
		case "title":
			f.Title, ok = val.(string)
		case "submit":
			f.Submit, ok = val.(string)
		case "fields":
			var list []any
			list, ok = val.([]any)
			for i, e := range list {
				fm, isMap := e.(map[string]any)
				if !isMap {
					return nil, fmt.Errorf("form: fields[%d]: want a map, got %T", i, e)
				}
				fld, err := parseField(i, fm)
				if err != nil {
					return nil, err
				}
				f.Fields = append(f.Fields, fld)
			}
		default:
			return nil, fmt.Errorf("form: unknown key %q (valid: title, submit, fields)", k)
		}
		if !ok {
			return nil, fmt.Errorf("form: %s: wrong type %T", k, val)
		}
	}
	return f, f.Validate()
}

func parseField(i int, m map[string]any) (FormField, error) {
	var f FormField
	for k, val := range m {
		ok := true
		switch k {
		case "name":
			f.Name, ok = val.(string)
		case "label":
			f.Label, ok = val.(string)
		case "type":
			f.Type, ok = val.(string)
		case "default":
			f.Default, ok = val.(string)
		case "optional":
			f.Optional, ok = val.(bool)
		case "options":
			var list []any
			list, ok = val.([]any)
			for _, o := range list {
				s, isStr := o.(string)
				if !isStr {
					return f, fmt.Errorf("form: fields[%d].options: want strings, got %T", i, o)
				}
				f.Options = append(f.Options, s)
			}
		default:
			return f, fmt.Errorf("form: fields[%d]: unknown key %q (valid: name, label, type, options, default, optional)", i, k)
		}
		if !ok {
			return f, fmt.Errorf("form: fields[%d].%s: wrong type %T", i, k, val)
		}
	}
	return f, nil
}

// Validate checks a form against the shape and Slack's own limits.
func (f *Form) Validate() error {
	if f == nil {
		return nil
	}
	if utf8.RuneCountInString(f.Title) > maxFormTitle {
		return fmt.Errorf("form: title must be at most %d characters", maxFormTitle)
	}
	if utf8.RuneCountInString(f.Submit) > maxFormTitle {
		return fmt.Errorf("form: submit must be at most %d characters", maxFormTitle)
	}
	if len(f.Fields) == 0 {
		return fmt.Errorf("form: needs at least one field")
	}
	if len(f.Fields) > maxFormFields {
		return fmt.Errorf("form: at most %d fields", maxFormFields)
	}
	seen := map[string]bool{}
	for i, fld := range f.Fields {
		where := fmt.Sprintf("form: fields[%d]", i)
		if !formNameRe.MatchString(fld.Name) {
			return fmt.Errorf("%s: name %q must be lowercase letters, digits, and _ (starting with a letter, at most 32)", where, fld.Name)
		}
		if seen[fld.Name] {
			return fmt.Errorf("%s: duplicate name %q", where, fld.Name)
		}
		seen[fld.Name] = true
		if utf8.RuneCountInString(fld.Label) > maxLabel {
			return fmt.Errorf("%s: label too long", where)
		}
		switch fld.Type {
		case "select":
			if len(fld.Options) == 0 || len(fld.Options) > maxSelectOptions {
				return fmt.Errorf("%s (%s): a select needs 1-%d options", where, fld.Name, maxSelectOptions)
			}
			opts := map[string]bool{}
			for _, o := range fld.Options {
				if o == "" || utf8.RuneCountInString(o) > maxOptionText || len(o) > maxOptionValue {
					return fmt.Errorf("%s (%s): option %q must be 1-%d characters", where, fld.Name, o, maxOptionText)
				}
				if strings.Contains(o, "{{") {
					return fmt.Errorf("%s (%s): option %q must be literal (no {{…}})", where, fld.Name, o)
				}
				if opts[o] {
					return fmt.Errorf("%s (%s): duplicate option %q", where, fld.Name, o)
				}
				opts[o] = true
			}
			if fld.Default != "" && !opts[fld.Default] {
				return fmt.Errorf("%s (%s): default %q is not one of the options", where, fld.Name, fld.Default)
			}
		case "text", "textarea":
			if len(fld.Options) > 0 {
				return fmt.Errorf("%s (%s): options apply to type: select only", where, fld.Name)
			}
			if utf8.RuneCountInString(fld.Default) > maxTextValue {
				return fmt.Errorf("%s (%s): default too long", where, fld.Name)
			}
		default:
			return fmt.Errorf("%s (%s): type must be select|text|textarea, got %q", where, fld.Name, fld.Type)
		}
	}
	return nil
}

// formActionID is the action_id every input element carries; the block_id is
// the field name, so view.state.values[<name>][formActionID] is its value.
const formActionID = "value"

func plain(s string) map[string]any {
	return map[string]any{"type": "plain_text", "text": s}
}

func (f *Form) title() string {
	if f.Title != "" {
		return f.Title
	}
	return "Conductor"
}

func (f *Form) submit() string {
	if f.Submit != "" {
		return f.Submit
	}
	return "Submit"
}

// blocks renders the form as Block Kit input blocks.
func (f *Form) blocks() []any {
	var out []any
	for _, fld := range f.Fields {
		label := fld.Label
		if label == "" {
			label = fld.Name
		}
		var el map[string]any
		switch fld.Type {
		case "select":
			var opts []any
			var initial any
			for _, o := range fld.Options {
				opt := map[string]any{"text": plain(o), "value": o}
				opts = append(opts, opt)
				if o == fld.Default {
					initial = opt
				}
			}
			el = map[string]any{"type": "static_select", "action_id": formActionID, "options": opts}
			if initial != nil {
				el["initial_option"] = initial
			}
		default:
			el = map[string]any{"type": "plain_text_input", "action_id": formActionID, "multiline": fld.Type == "textarea"}
			if fld.Default != "" {
				el["initial_value"] = fld.Default
			}
		}
		out = append(out, map[string]any{
			"type": "input", "block_id": fld.Name, "label": plain(label),
			"optional": fld.Optional, "element": el,
		})
	}
	return out
}

// formValues reads the submitted values out of a view_submission's
// view.state.values and checks each one against its field: a required field
// must be set, a select's value must be one of its configured options (Slack
// only offers those, but the submission is re-checked rather than trusted),
// and text is length-capped. errs maps block_id -> message, Slack's
// response_action: errors shape.
func (f *Form) formValues(state map[string]map[string]stateValue) (vals map[string]any, errs map[string]string) {
	vals = map[string]any{}
	errs = map[string]string{}
	for _, fld := range f.Fields {
		sv := state[fld.Name][formActionID]
		v := sv.Value
		if fld.Type == "select" {
			v = ""
			if sv.SelectedOption != nil {
				v = sv.SelectedOption.Value
			}
		}
		v = strings.TrimSpace(v)
		switch {
		case v == "" && !fld.Optional:
			errs[fld.Name] = "Required."
			continue
		case fld.Type == "select" && v != "" && !contains(fld.Options, v):
			errs[fld.Name] = "Pick one of the listed options."
			continue
		case utf8.RuneCountInString(v) > maxTextValue:
			errs[fld.Name] = fmt.Sprintf("At most %d characters.", maxTextValue)
			continue
		}
		if v == "" && fld.Type == "select" {
			v = fld.Default
		}
		vals[fld.Name] = v
	}
	return vals, errs
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}
