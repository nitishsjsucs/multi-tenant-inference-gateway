package prompts

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const doc = `
id: greeter
labels: {production: 2, staging: 3}
versions:
  - version: 1
    messages:
      - {role: system, content: "v1 for {{ .name }}"}
  - version: 2
    params: {temperature: 0.2}
    messages:
      - {role: system, content: "v2 for {{ .name }}"}
      - {role: user, content: "Hello from {{ .city }}"}
  - version: 3
    text: "v3 text for {{ .name }}"
`

func registry(t *testing.T, docs ...string) *Registry {
	t.Helper()
	dir := t.TempDir()
	for _, d := range docs {
		p, err := Parse([]byte(d))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p.ID+".yaml"), []byte(d), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestResolveVersionAndLabel(t *testing.T) {
	r := registry(t, doc)
	cases := []struct {
		ref  string
		want int
		err  error
	}{
		{`{"id":"greeter","version":"1"}`, 1, nil},
		{`{"id":"greeter","version":3}`, 3, nil},
		{`{"id":"greeter","version":"v2"}`, 2, nil},
		{`{"id":"greeter","label":"staging"}`, 3, nil},
		{`{"id":"greeter"}`, 2, nil}, // defaults to the production label
		{`{"id":"greeter","version":"9"}`, 0, ErrNoVersion},
		{`{"id":"greeter","label":"canary"}`, 0, ErrNoLabel},
		{`{"id":"nope"}`, 0, ErrNotFound},
	}
	for _, tc := range cases {
		var ref Ref
		if err := json.Unmarshal([]byte(tc.ref), &ref); err != nil {
			t.Fatal(err)
		}
		v, err := r.Resolve(ref)
		if !errors.Is(err, tc.err) {
			t.Errorf("%s: err = %v, want %v", tc.ref, err, tc.err)
			continue
		}
		if err == nil && v.Version != tc.want {
			t.Errorf("%s: version = %d, want %d", tc.ref, v.Version, tc.want)
		}
	}
}

func TestResolveLatestWithoutProductionLabel(t *testing.T) {
	r := registry(t, strings.Replace(doc, "production: 2, ", "", 1))
	v, err := r.Resolve(Ref{ID: "greeter"})
	if err != nil || v.Version != 3 {
		t.Fatalf("got %v, %v; want latest version 3", v, err)
	}
}

func TestRenderChat(t *testing.T) {
	r := registry(t, doc)
	out, err := r.RenderChat(Ref{ID: "greeter", Label: "production", Variables: map[string]any{"name": "Ada", "city": "Paris"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Version != 2 || len(out.Messages) != 2 ||
		out.Messages[0] != (Message{Role: "system", Content: "v2 for Ada"}) ||
		out.Messages[1] != (Message{Role: "user", Content: "Hello from Paris"}) {
		t.Fatalf("rendered = %+v", out)
	}
	if out.Params["temperature"] != 0.2 {
		t.Errorf("params = %v", out.Params)
	}
}

func TestRenderMissingVariableFails(t *testing.T) {
	r := registry(t, doc)
	_, err := r.RenderChat(Ref{ID: "greeter", Version: "2", Variables: map[string]any{"name": "Ada"}})
	if err == nil || !strings.Contains(err.Error(), "city") {
		t.Fatalf("want a missing-key error naming the variable, got %v", err)
	}
}

func TestRenderWrongTemplateKind(t *testing.T) {
	r := registry(t, doc)
	if _, err := r.RenderChat(Ref{ID: "greeter", Version: "3"}); !errors.Is(err, ErrWrongTemplate) {
		t.Errorf("chat render of a text-only version: err = %v", err)
	}
	if _, err := r.RenderText(Ref{ID: "greeter", Version: "1"}); !errors.Is(err, ErrWrongTemplate) {
		t.Errorf("text render of a chat-only version: err = %v", err)
	}
	out, err := r.RenderText(Ref{ID: "greeter", Label: "staging", Variables: map[string]any{"name": "Bo"}})
	if err != nil || out.Text != "v3 text for Bo" {
		t.Errorf("RenderText = %+v, %v", out, err)
	}
}

func TestParseRejectsInvalid(t *testing.T) {
	for name, d := range map[string]string{
		"dangling label": "id: x\nlabels: {production: 4}\nversions: [{version: 1, text: hi}]",
		"duplicate":      "id: x\nversions: [{version: 1, text: a}, {version: 1, text: b}]",
		"bad role":       "id: x\nversions: [{version: 1, messages: [{role: robot, content: hi}]}]",
		"bad template":   "id: x\nversions: [{version: 1, text: '{{ .a '}]",
		"empty version":  "id: x\nversions: [{version: 1}]",
		"unknown field":  "id: x\nversion: [{version: 1, text: a}]",
	} {
		if _, err := Parse([]byte(d)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestLoadDirChecksFileName(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "other.yaml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDir(dir); err == nil {
		t.Fatal("expected an id/file name mismatch error")
	}
}

func TestSamplePromptsLoad(t *testing.T) {
	r, err := LoadDir("../../prompts")
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.RenderChat(Ref{ID: "support-agent", Variables: map[string]any{"company": "Acme", "tier": "enterprise"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Version != 2 || !strings.Contains(out.Messages[0].Content, "named technical account manager") {
		t.Errorf("unexpected render: v%d", out.Version)
	}
}
