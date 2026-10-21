// Package openai holds the small slice of the OpenAI wire format the gateway
// needs: error bodies, a field-preserving request body, prompt extraction
// for prefix hashing and a token estimator.
package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Error types used in error bodies.
const (
	TypeInvalidRequest = "invalid_request_error"
	TypeAuthentication = "authentication_error"
	TypeRateLimit      = "rate_limit_error"
	TypeNotFound       = "not_found_error"
	TypeServer         = "server_error"
	TypeUnavailable    = "service_unavailable"
)

// ErrorBody is the OpenAI error envelope.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail is the content of an error envelope.
type ErrorDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

// WriteError writes an OpenAI-style error response.
func WriteError(w http.ResponseWriter, status int, typ, code, param, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(NewError(typ, code, param, msg))
}

// NewError builds an error envelope; empty code/param encode as null.
func NewError(typ, code, param, msg string) ErrorBody {
	d := ErrorDetail{Message: msg, Type: typ}
	if code != "" {
		d.Code = &code
	}
	if param != "" {
		d.Param = &param
	}
	return ErrorBody{Error: d}
}

// Body is a request body decoded only one level deep, so that every field
// the gateway does not touch is forwarded byte-for-byte.
type Body map[string]json.RawMessage

// ParseBody decodes a JSON object.
func ParseBody(data []byte) (Body, error) {
	var b Body
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("request body must be a JSON object: %w", err)
	}
	if b == nil {
		return nil, fmt.Errorf("request body must be a JSON object")
	}
	return b, nil
}

// Has reports whether key is present.
func (b Body) Has(key string) bool { _, ok := b[key]; return ok }

// Get decodes key into v; it returns false if the key is absent or null.
func (b Body) Get(key string, v any) (bool, error) {
	raw, ok := b[key]
	if !ok || string(raw) == "null" {
		return false, nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return true, fmt.Errorf("invalid %q: %w", key, err)
	}
	return true, nil
}

// Set encodes v under key.
func (b Body) Set(key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b[key] = raw
	return nil
}

// Marshal re-encodes the body.
func (b Body) Marshal() ([]byte, error) { return json.Marshal(map[string]json.RawMessage(b)) }

// Usage is the token accounting block of a response.
type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// Message is the subset of a chat message needed to read its text.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// Text returns the message's text: a plain string, or the concatenated
// "text" parts of a content-part array.
func (m Message) Text() string {
	if len(m.Content) == 0 || string(m.Content) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// ChatPrompt serialises messages the way a chat template would, preserving
// prefixes: two conversations that share their first k messages produce
// strings that share a prefix, which is what prefix routing relies on.
func ChatPrompt(messages []Message) string {
	var b strings.Builder
	for _, m := range messages {
		b.WriteString("<|")
		b.WriteString(m.Role)
		b.WriteString("|>\n")
		b.WriteString(m.Text())
		b.WriteString("\n")
	}
	return b.String()
}

// EstimateTokens approximates a tokenizer at four characters per token,
// which is OpenAI's rule of thumb for English text. Budgets are reconciled
// against the server-reported usage afterwards, so the estimate only has to
// be in the right ballpark.
func EstimateTokens(text string) int64 { return int64((len(text) + 3) / 4) }

// EstimateChatTokens adds the per-message framing overhead of chat templates.
func EstimateChatTokens(messages []Message) int64 {
	var n int64 = 3
	for _, m := range messages {
		n += 4 + EstimateTokens(m.Text())
	}
	return n
}
