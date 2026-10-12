package provider

import (
	"encoding/json"
	"testing"
)

func TestNormalizeOutput(t *testing.T) {
	cases := []struct{ method, raw, kind, mode, text, status string }{
		{"item/agentMessage/delta", `{"itemId":"a","delta":"hello"}`, "assistant", "delta", "hello", "running"},
		{"item/completed", `{"item":{"id":"a","type":"agentMessage","text":"hello"}}`, "assistant", "snapshot", "hello", "completed"},
		{"item/started", `{"item":{"id":"c","type":"commandExecution","command":"go test","status":"inProgress"}}`, "tool", "start", "", "running"},
		{"item/completed", `{"item":{"id":"c","type":"commandExecution","aggregatedOutput":"FAIL","status":"completed","exitCode":1}}`, "tool", "snapshot", "FAIL", "failed"},
		{"turn/completed", `{"turn":{"id":"t","status":"failed","error":{"message":"boom"},"durationMs":1500}}`, "turn", "snapshot", "boom", "failed"},
		{"item/completed", `{"item":{"id":"f","type":"fileChange","changes":[{"path":"x.go","diff":"+hello"}]}}`, "diff", "snapshot", "x.go\n+hello\n", "completed"},
	}
	for _, c := range cases {
		t.Run(c.method+c.kind, func(t *testing.T) {
			o := NormalizeOutput(c.method, json.RawMessage(c.raw), "turn")
			if o == nil || o.Kind != c.kind || o.Mode != c.mode || o.Text != c.text || o.Status != c.status {
				t.Fatalf("%+v", o)
			}
		})
	}
	if NormalizeOutput("unknown", json.RawMessage(`{}`), "turn") != nil {
		t.Fatal("unknown event normalized")
	}
}
