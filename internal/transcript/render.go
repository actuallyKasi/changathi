package transcript

import (
	"fmt"
	"strings"

	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type cached struct {
	revision           int
	expanded, selected bool
	text               string
}
type Renderer struct {
	width       int
	plain, dark bool
	markdown    *glamour.TermRenderer
	cache       map[string]cached
}

func NewRenderer(width int, plain, dark bool) *Renderer {
	width = max(10, width)
	style := "dark"
	if !dark {
		style = "light"
	}
	if plain {
		style = "ascii"
	}
	md, _ := glamour.NewTermRenderer(glamour.WithStandardStyle(style), glamour.WithWordWrap(max(8, width-2)))
	return &Renderer{width: width, plain: plain, dark: dark, markdown: md, cache: map[string]cached{}}
}

func (r *Renderer) accent(s string) string {
	if r.plain {
		return s
	}
	c := "#8B9EFF"
	if !r.dark {
		c = "#4338CA"
	}
	return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(c)).Render(s)
}

func (r *Renderer) muted(s string) string {
	if r.plain {
		return s
	}
	c := "#7C869C"
	if !r.dark {
		c = "#6B7280"
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(c)).Render(s)
}

func duration(ms int64) string {
	if ms >= 60_000 {
		return fmt.Sprintf("%dm%02ds", ms/60_000, ms/1000%60)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}

// divider renders lifecycle changes and turn boundaries as a single marker line.
func (r *Renderer) divider(i Item, selected bool) string {
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
	if selected {
		line = "> " + line
	}
	line = ansi.Truncate(line, r.width, "…")
	if failed {
		line = r.accent(line)
		if i.Text != "" {
			line += "\n" + ansi.Wrap(i.Text, r.width, "")
		}
	} else {
		line = r.muted(line)
	}
	return line + "\n\n"
}

func (r *Renderer) Render(i Item, expanded, selected bool) string {
	if c, ok := r.cache[i.Key]; ok && c.revision == i.Revision && c.expanded == expanded && c.selected == selected {
		return c.text
	}
	if i.Kind == "lifecycle" || i.Kind == "turn" {
		text := r.divider(i, selected)
		if r.plain {
			text = ansi.Strip(text)
		}
		if len(r.cache) >= MaxItems {
			clear(r.cache)
		}
		r.cache[i.Key] = cached{i.Revision, expanded, selected, text}
		return text
	}
	title := i.Title
	if title == "" {
		switch i.Kind {
		case "assistant":
			title = "Agent"
		case "tool":
			title = "Tool"
		default:
			title = i.Kind
		}
	}
	heading := title
	if i.Kind == "tool" || i.Kind == "diff" {
		marker := "+"
		if expanded {
			marker = "−"
		}
		heading = marker + " " + heading
	}
	if selected {
		heading = "> " + heading
	}
	if i.Status != "" {
		heading += "  [" + i.Status + "]"
	}
	if i.DurationMS > 0 {
		heading += fmt.Sprintf("  %.1fs", float64(i.DurationMS)/1000)
	}
	heading = r.accent(ansi.Truncate(heading, r.width, "…"))
	body := i.Text
	if (i.Kind == "tool" || i.Kind == "diff") && !expanded {
		body = ""
	}
	if i.Kind == "assistant" && body != "" && r.markdown != nil {
		if rendered, err := r.markdown.Render(body); err == nil {
			body = strings.Trim(rendered, "\n")
		}
	} else {
		body = ansi.Hardwrap(strings.ReplaceAll(body, "\t", "    "), r.width, true)
		if i.Kind == "diff" && !r.plain {
			lines := strings.Split(body, "\n")
			for n, line := range lines {
				color := ""
				if strings.HasPrefix(line, "+") {
					color = "#22A06B"
				}
				if strings.HasPrefix(line, "-") {
					color = "#D94C4C"
				}
				if color != "" {
					lines[n] = lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render(line)
				}
			}
			body = strings.Join(lines, "\n")
		}
	}
	if r.plain {
		body = ansi.Strip(body)
	}
	text := heading + "\n"
	if body != "" {
		text += body + "\n"
	}
	text += "\n"
	if len(r.cache) >= MaxItems {
		clear(r.cache)
	}
	r.cache[i.Key] = cached{i.Revision, expanded, selected, text}
	return text
}
