package provider

import (
	"changathi/internal/core"
	"encoding/json"
	"fmt"
	"strings"
)

// NormalizeOutput keeps wire method names/raw text for diagnostics, alongside a
// portable payload for replay. Unknown provider items are left in the raw view.
func NormalizeOutput(method string, raw json.RawMessage, turn string) *core.Output {
	var p struct {
		ItemID string `json:"itemId"`
		Delta  string `json:"delta"`
		Turn   struct {
			ID         string `json:"id"`
			Status     string `json:"status"`
			DurationMS int64  `json:"durationMs"`
			Error      *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"turn"`
		Item struct {
			ID               string          `json:"id"`
			Type             string          `json:"type"`
			Text             string          `json:"text"`
			Command          string          `json:"command"`
			Status           string          `json:"status"`
			AggregatedOutput string          `json:"aggregatedOutput"`
			ExitCode         *int            `json:"exitCode"`
			DurationMS       int64           `json:"durationMs"`
			Tool             string          `json:"tool"`
			Namespace        string          `json:"namespace"`
			Success          *bool           `json:"success"`
			ContentItems     json.RawMessage `json:"contentItems"`
			Server           string          `json:"server"`
			Result           json.RawMessage `json:"result"`
			Error            json.RawMessage `json:"error"`
			Changes          []struct {
				Path string `json:"path"`
				Diff string `json:"diff"`
			} `json:"changes"`
		} `json:"item"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return nil
	}
	o := &core.Output{ItemID: p.ItemID, Text: p.Delta, Mode: "delta", Status: "running"}
	if o.ItemID == "" {
		o.ItemID = "legacy-" + turn + "-" + method
	}
	switch method {
	case "turn/completed":
		o.Kind, o.ItemID, o.Mode, o.Text = "turn", "turn-"+p.Turn.ID, "snapshot", ""
		o.Status, o.DurationMS = p.Turn.Status, p.Turn.DurationMS
		if o.Status == "" {
			o.Status = "completed"
		}
		if p.Turn.Error != nil {
			o.Text = p.Turn.Error.Message
		}
	case "item/agentMessage/delta":
		o.Kind = "assistant"
	case "item/commandExecution/outputDelta":
		o.Kind = "tool"
	case "item/fileChange/outputDelta":
		o.Kind = "diff"
	case "item/started", "item/completed":
		o.ItemID = p.Item.ID
		if o.ItemID == "" {
			return nil
		}
		o.Mode, o.Status, o.DurationMS = "snapshot", p.Item.Status, p.Item.DurationMS
		if o.Status == "" {
			if method == "item/completed" {
				o.Status = "completed"
			} else {
				o.Status = "running"
			}
		}
		if o.Status == "inProgress" {
			o.Status = "running"
		}
		switch p.Item.Type {
		case "agentMessage":
			o.Kind, o.Text = "assistant", p.Item.Text
		case "commandExecution":
			o.Kind, o.Title, o.Text = "tool", p.Item.Command, p.Item.AggregatedOutput
			if p.Item.ExitCode != nil && *p.Item.ExitCode != 0 {
				o.Status = "failed"
			}
		case "fileChange":
			o.Kind, o.Title = "diff", "File changes"
			var b strings.Builder
			for _, c := range p.Item.Changes {
				fmt.Fprintf(&b, "%s\n%s\n", c.Path, c.Diff)
			}
			o.Text = b.String()
		case "mcpToolCall", "dynamicToolCall":
			o.Kind, o.Title = "tool", strings.Trim(p.Item.Server+" / "+p.Item.Tool, " / ")
			if p.Item.Namespace != "" {
				o.Title = p.Item.Namespace + " / " + p.Item.Tool
			}
			if len(p.Item.Result) > 0 {
				o.Text = toolResultText(p.Item.Result)
			}
			if len(p.Item.Error) > 0 && string(p.Item.Error) != "null" {
				o.Text += "\n" + string(p.Item.Error)
				o.Status = "failed"
			}
			if len(p.Item.ContentItems) > 0 && string(p.Item.ContentItems) != "null" {
				o.Text = contentText(p.Item.ContentItems)
			}
			if p.Item.Success != nil && !*p.Item.Success {
				o.Status = "failed"
			}
		default:
			return nil
		}
		// A start event supplies metadata, not a replacement for already received output.
		if method == "item/started" {
			o.Mode = "start"
		}
	default:
		return nil
	}
	return o
}

func contentText(raw json.RawMessage) string {
	var content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &content) != nil {
		return string(raw)
	}
	var lines []string
	for _, c := range content {
		if c.Text != "" {
			lines = append(lines, c.Text)
		} else {
			lines = append(lines, "["+c.Type+" content]")
		}
	}
	return strings.Join(lines, "\n")
}
func toolResultText(raw json.RawMessage) string {
	var result struct {
		Content    json.RawMessage `json:"content"`
		Structured json.RawMessage `json:"structuredContent"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return string(raw)
	}
	text := contentText(result.Content)
	if len(result.Structured) > 0 && string(result.Structured) != "null" {
		var b strings.Builder
		if text != "" {
			b.WriteString(text)
			b.WriteString("\n")
		}
		b.Write(result.Structured)
		text = b.String()
	}
	return text
}
