// Package prompts is a registry of versioned prompt templates loaded from
// YAML files. Requests reference a prompt by id plus an explicit version or
// a label such as "production"; the gateway renders it with the request's
// variables.
package prompts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

// Errors returned by Resolve and Render.
var (
	ErrNotFound      = errors.New("prompt not found")
	ErrNoVersion     = errors.New("prompt version not found")
	ErrNoLabel       = errors.New("prompt label not found")
	ErrWrongTemplate = errors.New("prompt has no template for this endpoint")
)

// DefaultLabel is used when a reference names neither a version nor a label.
const DefaultLabel = "production"

// Message is one chat message of a template.
type Message struct {
	Role    string `yaml:"role" json:"role"`
	Content string `yaml:"content" json:"content"`
}

// Version is one immutable revision of a prompt.
type Version struct {
	Version     int    `yaml:"version"`
	Description string `yaml:"description"`
	// Messages is the chat form, rendered in front of the request's messages.
	Messages []Message `yaml:"messages"`
	// Text is the completions form, rendered into the request's prompt.
	Text string `yaml:"text"`
	// Params are request defaults (temperature, max_tokens, ...) applied when
	// the request does not set them.
	Params map[string]any `yaml:"params"`

	messageTmpls []*template.Template
	textTmpl     *template.Template
}

// Prompt is every version of one prompt id.
type Prompt struct {
	ID          string         `yaml:"id"`
	Description string         `yaml:"description"`
	Labels      map[string]int `yaml:"labels"`
	Versions    []*Version     `yaml:"versions"`
}

// Ref is the "prompt" extension field of a request.
type Ref struct {
	ID        string         `json:"id"`
	Version   VersionSpec    `json:"version,omitempty"`
	Label     string         `json:"label,omitempty"`
	Variables map[string]any `json:"variables,omitempty"`
}

// VersionSpec accepts a version as a JSON number or string ("3" or 3).
type VersionSpec string

// UnmarshalJSON implements json.Unmarshaler.
func (v *VersionSpec) UnmarshalJSON(b []byte) error {
	var n json.Number
	if err := json.Unmarshal(b, &n); err == nil {
		*v = VersionSpec(n.String())
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("prompt version must be a number or string")
	}
	*v = VersionSpec(s)
	return nil
}

// Rendered is a resolved and rendered prompt.
type Rendered struct {
	ID       string
	Version  int
	Messages []Message
	Text     string
	Params   map[string]any
}

// Registry holds every loaded prompt.
type Registry struct {
	prompts map[string]*Prompt
}

// LoadDir loads every *.yaml / *.yml file in dir. A missing dir yields an
// empty registry.
func LoadDir(dir string) (*Registry, error) {
	r := &Registry{prompts: map[string]*Prompt{}}
	if dir == "" {
		return r, nil
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		if e.IsDir() || (ext != ".yaml" && ext != ".yml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		p, err := Parse(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if want := strings.TrimSuffix(e.Name(), ext); p.ID != want {
			return nil, fmt.Errorf("%s: id %q must match the file name", e.Name(), p.ID)
		}
		r.prompts[p.ID] = p
	}
	return r, nil
}

// Parse decodes and compiles one prompt document.
func Parse(data []byte) (*Prompt, error) {
	var p Prompt
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, errors.New("id is required")
	}
	seen := map[int]bool{}
	for _, v := range p.Versions {
		if v.Version <= 0 || seen[v.Version] {
			return nil, fmt.Errorf("versions must be unique positive integers (got %d)", v.Version)
		}
		seen[v.Version] = true
		if err := v.compile(p.ID); err != nil {
			return nil, fmt.Errorf("version %d: %w", v.Version, err)
		}
	}
	if len(p.Versions) == 0 {
		return nil, errors.New("at least one version is required")
	}
	for label, n := range p.Labels {
		if !seen[n] {
			return nil, fmt.Errorf("label %q points at missing version %d", label, n)
		}
	}
	return &p, nil
}

func (v *Version) compile(id string) error {
	if len(v.Messages) == 0 && v.Text == "" {
		return errors.New("needs messages or text")
	}
	newTmpl := func(name, src string) (*template.Template, error) {
		// missingkey=error turns a forgotten variable into a 400 instead of
		// silently rendering "<no value>" into the prompt.
		return template.New(name).Option("missingkey=error").Parse(src)
	}
	for i, m := range v.Messages {
		switch m.Role {
		case "system", "developer", "user", "assistant":
		default:
			return fmt.Errorf("message %d: invalid role %q", i, m.Role)
		}
		t, err := newTmpl(fmt.Sprintf("%s/v%d/messages[%d]", id, v.Version, i), m.Content)
		if err != nil {
			return err
		}
		v.messageTmpls = append(v.messageTmpls, t)
	}
	if v.Text != "" {
		t, err := newTmpl(fmt.Sprintf("%s/v%d/text", id, v.Version), v.Text)
		if err != nil {
			return err
		}
		v.textTmpl = t
	}
	return nil
}

// Resolve finds the version a reference points at: an explicit version
// wins, then a label, then the "production" label, then the latest version.
func (r *Registry) Resolve(ref Ref) (*Version, error) {
	p, ok := r.prompts[ref.ID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, ref.ID)
	}
	want := 0
	switch {
	case ref.Version != "":
		n, err := strconv.Atoi(strings.TrimPrefix(string(ref.Version), "v"))
		if err != nil {
			return nil, fmt.Errorf("%w: %q has no version %q", ErrNoVersion, ref.ID, ref.Version)
		}
		want = n
	case ref.Label != "":
		n, ok := p.Labels[ref.Label]
		if !ok {
			return nil, fmt.Errorf("%w: %q has no label %q", ErrNoLabel, ref.ID, ref.Label)
		}
		want = n
	default:
		if n, ok := p.Labels[DefaultLabel]; ok {
			want = n
		} else {
			want = slices.MaxFunc(p.Versions, func(a, b *Version) int { return a.Version - b.Version }).Version
		}
	}
	for _, v := range p.Versions {
		if v.Version == want {
			return v, nil
		}
	}
	return nil, fmt.Errorf("%w: %q has no version %d", ErrNoVersion, ref.ID, want)
}

// RenderChat resolves ref and renders its chat messages.
func (r *Registry) RenderChat(ref Ref) (*Rendered, error) {
	return r.render(ref, true)
}

// RenderText resolves ref and renders its completions text.
func (r *Registry) RenderText(ref Ref) (*Rendered, error) {
	return r.render(ref, false)
}

func (r *Registry) render(ref Ref, chat bool) (*Rendered, error) {
	v, err := r.Resolve(ref)
	if err != nil {
		return nil, err
	}
	out := &Rendered{ID: ref.ID, Version: v.Version, Params: v.Params}
	vars := ref.Variables
	if vars == nil {
		vars = map[string]any{}
	}
	if chat {
		if len(v.messageTmpls) == 0 {
			return nil, fmt.Errorf("%w: %s v%d defines no messages", ErrWrongTemplate, ref.ID, v.Version)
		}
		for i, t := range v.messageTmpls {
			s, err := execute(t, vars)
			if err != nil {
				return nil, err
			}
			out.Messages = append(out.Messages, Message{Role: v.Messages[i].Role, Content: s})
		}
		return out, nil
	}
	if v.textTmpl == nil {
		return nil, fmt.Errorf("%w: %s v%d defines no text", ErrWrongTemplate, ref.ID, v.Version)
	}
	if out.Text, err = execute(v.textTmpl, vars); err != nil {
		return nil, err
	}
	return out, nil
}

func execute(t *template.Template, vars map[string]any) (string, error) {
	var b strings.Builder
	if err := t.Execute(&b, vars); err != nil {
		return "", fmt.Errorf("render %s: %w", t.Name(), err)
	}
	return b.String(), nil
}
