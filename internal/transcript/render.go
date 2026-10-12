package transcript

import (
	"fmt"
	"strings"
)

// Renderer prints transcript items as plain text for agents and pipes. Tool
// and diff bodies are shown only when they failed unless Full is set, which
// keeps routine command output out of the reader's context.
type Renderer struct {
	Full bool
}

func duration(ms int64) string {
	if ms >= 60_000 {
		return fmt.Sprintf("%dm%02ds", ms/60_000, ms/1000%60)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}

// divider renders lifecycle changes and turn boundaries as a single marker line.
func divider(i Item) string {
	parts := []string{i.Title}
	if i.Kind == "turn" {
		parts[0] = "Turn " + strings.ReplaceAll(i.Status, "_", " ")
		if i.DurationMS > 0 {
			parts = append(parts, duration(i.DurationMS))
		}
	}
	detail := strings.Join(strings.Fields(i.Text), " ")
	failed := i.Kind == "turn" && i.Status != "completed"
	if detail != "" && !failed {
		parts = append(parts, detail)
	}
	line := "── " + strings.Join(parts, " · ") + " ──"
	if failed && i.Text != "" {
		line += "\n" + i.Text
	}
	return line + "\n\n"
}

func (r Renderer) Render(i Item) string {
	if i.Kind == "lifecycle" || i.Kind == "turn" {
		return divider(i)
	}
	heading := i.Title
	if heading == "" {
		switch i.Kind {
		case "assistant":
			heading = "Agent"
		case "tool":
			heading = "Tool"
		default:
			heading = i.Kind
		}
	}
	if i.Status != "" {
		heading += "  [" + i.Status + "]"
	}
	if i.DurationMS > 0 {
		heading += "  " + duration(i.DurationMS)
	}
	body := i.Text
	if (i.Kind == "tool" || i.Kind == "diff") && !r.Full && i.Status != "failed" {
		body = ""
	}
	text := heading + "\n"
	if body = strings.TrimRight(body, "\n"); body != "" {
		text += body + "\n"
	}
	return text + "\n"
}
