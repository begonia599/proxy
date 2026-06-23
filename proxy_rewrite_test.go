package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestRewriteTopLevelModel(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		newModel string
		want     string
		wantOK   bool
	}{
		{
			name:     "simple",
			in:       `{"model":"sonnet","max_tokens":100}`,
			newModel: "claude-sonnet-4-5-20250929",
			want:     `{"model":"claude-sonnet-4-5-20250929","max_tokens":100}`,
			wantOK:   true,
		},
		{
			name:     "model not first key",
			in:       `{"max_tokens":100,"model":"opus"}`,
			newModel: "claude-opus-4-5",
			want:     `{"max_tokens":100,"model":"claude-opus-4-5"}`,
			wantOK:   true,
		},
		{
			name:     "whitespace around colon",
			in:       `{"model" : "x" }`,
			newModel: "y",
			want:     `{"model" : "y" }`,
			wantOK:   true,
		},
		{
			name:     "nested model key must be ignored",
			in:       `{"tools":[{"input_schema":{"properties":{"model":{"type":"string"}}}}],"model":"sonnet"}`,
			newModel: "REAL",
			want:     `{"tools":[{"input_schema":{"properties":{"model":{"type":"string"}}}}],"model":"REAL"}`,
			wantOK:   true,
		},
		{
			name:     "model value appearing in a string before the key",
			in:       `{"system":"use model wisely","model":"a"}`,
			newModel: "b",
			want:     `{"system":"use model wisely","model":"b"}`,
			wantOK:   true,
		},
		{
			name:     "no top-level model",
			in:       `{"messages":[],"max_tokens":1}`,
			newModel: "x",
			want:     `{"messages":[],"max_tokens":1}`,
			wantOK:   false,
		},
		{
			name:     "escaped quote in preceding string",
			in:       `{"system":"say \"model\" out loud","model":"a"}`,
			newModel: "b",
			want:     `{"system":"say \"model\" out loud","model":"b"}`,
			wantOK:   true,
		},
		{
			name:     "newModel containing quote is JSON-escaped",
			in:       `{"model":"x","max_tokens":1}`,
			newModel: `weird"name`,
			want:     `{"model":"weird\"name","max_tokens":1}`,
			wantOK:   true,
		},
		{
			name:     "newModel containing backslash is JSON-escaped",
			in:       `{"model":"x"}`,
			newModel: `a\b`,
			want:     `{"model":"a\\b"}`,
			wantOK:   true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := rewriteTopLevelModel([]byte(c.in), c.newModel)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if string(got) != c.want {
				t.Fatalf("got  %s\nwant %s", got, c.want)
			}
			// 改写后必须仍是合法 JSON，且顶层 model 等于新值
			if c.wantOK {
				var m map[string]any
				if err := json.Unmarshal(got, &m); err != nil {
					t.Fatalf("result not valid JSON: %v", err)
				}
				if m["model"] != c.newModel {
					t.Fatalf("top-level model = %v, want %v", m["model"], c.newModel)
				}
			}
		})
	}
}

func TestStripUnsignedThinkingBlocks(t *testing.T) {
	in := []byte(`{"model":"opus","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"bad","signature":""},{"type":"text","text":"keep"},{"type":"thinking","thinking":"good","signature":"signed"},{"type":"redacted_thinking","data":"keep"}]},{"role":"user","content":[{"type":"text","text":"next"}]}]}`)
	got, removed := stripUnsignedThinkingBlocks(in)
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	var body struct {
		Messages []struct {
			Content []struct {
				Type      string `json:"type"`
				Signature string `json:"signature"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got, &body); err != nil {
		t.Fatalf("result is invalid JSON: %v\n%s", err, got)
	}
	content := body.Messages[0].Content
	if len(content) != 3 || content[0].Type != "text" ||
		content[1].Type != "thinking" || content[1].Signature != "signed" ||
		content[2].Type != "redacted_thinking" {
		t.Fatalf("unexpected remaining blocks: %#v", content)
	}
	if !bytes.Contains(got, []byte(`{"type":"text","text":"keep"}`)) {
		t.Fatal("unrelated bytes were not preserved")
	}
}

func TestStripAdjacentUnsignedThinkingBlocks(t *testing.T) {
	in := []byte(`{"messages":[{"content":[{"type":"thinking","signature":""},{"type":"thinking","signature":""},{"type":"text","text":"keep"}]}]}`)
	got, removed := stripUnsignedThinkingBlocks(in)
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	if string(got) != `{"messages":[{"content":[{"type":"text","text":"keep"}]}]}` {
		t.Fatalf("got %s", got)
	}
}
