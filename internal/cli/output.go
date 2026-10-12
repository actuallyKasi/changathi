package cli

import (
	"changathi/internal/core"
	"changathi/internal/transcript"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"unicode"
)

func singleLine(s string) string { return strings.Join(strings.Fields(transcript.Clean(s)), " ") }

// display keeps machine output unchanged while giving arbitrary provider metadata
// and action results a readable, deterministic fallback.
func (a *app) display(v any) error {
	if a.json {
		return a.print(v)
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if object, ok := value.(map[string]any); ok && object["parent"] != nil && object["plan"] != nil {
		var supervisor core.Supervisor
		if err := json.Unmarshal(data, &supervisor); err != nil {
			return err
		}
		return a.showSupervisor(supervisor)
	}
	var b strings.Builder
	readableValue(&b, value, "")
	if b.Len() == 0 {
		b.WriteString("No data available.\n")
	}
	_, err = fmt.Fprint(a.out, b.String())
	return err
}

func fieldLabel(key string) string {
	var b strings.Builder
	for n, r := range key {
		if r == '_' {
			b.WriteByte(' ')
			continue
		}
		if unicode.IsUpper(r) && n > 0 {
			b.WriteByte(' ')
		}
		if n == 0 {
			r = unicode.ToUpper(r)
		}
		b.WriteRune(r)
	}
	return singleLine(b.String())
}

func readableValue(b *strings.Builder, value any, indent string) {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			val := v[key]
			if val == nil || val == "" {
				continue
			}
			fmt.Fprintf(b, "%s%s:", indent, fieldLabel(key))
			switch val.(type) {
			case map[string]any, []any:
				b.WriteByte('\n')
				readableValue(b, val, indent+"  ")
			default:
				b.WriteByte(' ')
				readableValue(b, val, indent)
			}
		}
	case []any:
		if len(v) == 0 {
			fmt.Fprintf(b, "%sNone\n", indent)
		}
		for n, item := range v {
			if n > 0 {
				b.WriteByte('\n')
			}
			readableValue(b, item, indent)
		}
	case nil:
		fmt.Fprintf(b, "%sNo data available.\n", indent)
	case bool:
		if v {
			b.WriteString("yes\n")
		} else {
			b.WriteString("no\n")
		}
	default:
		text := transcript.Clean(fmt.Sprint(v))
		b.WriteString(strings.ReplaceAll(text, "\n", "\n"+indent) + "\n")
	}
}

func (a *app) showSupervisor(v core.Supervisor) error {
	w := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "Supervisor #%d — %s\n\n", v.Parent.Number, singleLine(v.Parent.Title))
	fmt.Fprintf(w, "Status:\t%s\n", strings.ReplaceAll(singleLine(v.State), "_", " "))
	if v.Reason != "" {
		fmt.Fprintf(w, "Detail:\t%s\n", singleLine(v.Reason))
	}
	if v.Hold != "" {
		fmt.Fprintf(w, "Hold:\t%s\n", singleLine(v.Hold))
	}
	if v.Current != 0 {
		fmt.Fprintf(w, "Current issue:\t#%d\n", v.Current)
	}
	if v.Parent.HTMLURL != "" {
		fmt.Fprintf(w, "URL:\t%s\n", singleLine(v.Parent.HTMLURL))
	}
	if v.WorkpadError != "" {
		fmt.Fprintf(w, "Workpad error:\t%s\n", singleLine(v.WorkpadError))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if v.Parent.Body != "" {
		if _, err := fmt.Fprintf(a.out, "\n%s\n", transcript.Clean(v.Parent.Body)); err != nil {
			return err
		}
	}
	fmt.Fprintln(w, "\nISSUE\tSUBJECT\tSTATUS\tDEPENDS ON\tPR\tDETAIL")
	statuses := map[int]core.ChildStatus{}
	for _, child := range v.Children {
		statuses[child.Issue] = child
	}
	for _, child := range v.Plan.Children {
		status := statuses[child.Issue]
		state := status.State
		if state == "" {
			state = "not evaluated"
		}
		deps := []string{}
		for _, dep := range child.DependsOn {
			deps = append(deps, fmt.Sprintf("#%d", dep))
		}
		dependency := strings.Join(deps, ", ")
		if dependency == "" {
			dependency = "—"
		}
		pr := "—"
		if status.PR != 0 {
			pr = fmt.Sprintf("#%d", status.PR)
		}
		fmt.Fprintf(w, "#%d\t%s\t%s\t%s\t%s\t%s\n", child.Issue, singleLine(status.Title), strings.ReplaceAll(singleLine(state), "_", " "), dependency, pr, singleLine(status.Reason))
	}
	return w.Flush()
}
