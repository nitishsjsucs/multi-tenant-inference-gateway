package openai

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMessageText(t *testing.T) {
	var msgs []Message
	raw := `[{"role":"user","content":"plain"},
		{"role":"user","content":[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":"b"}]},
		{"role":"assistant","content":null,"tool_calls":[]}]`
	if err := json.Unmarshal([]byte(raw), &msgs); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"plain", "ab", ""} {
		if got := msgs[i].Text(); got != want {
			t.Errorf("message %d: %q, want %q", i, got, want)
		}
	}
}

func TestChatPromptPreservesPrefixes(t *testing.T) {
	m := func(role, text string) Message {
		b, _ := json.Marshal(text)
		return Message{Role: role, Content: b}
	}
	turn1 := []Message{m("system", "sys"), m("user", "q1")}
	turn2 := append(append([]Message{}, turn1...), m("assistant", "a1"), m("user", "q2"))
	if !strings.HasPrefix(ChatPrompt(turn2), ChatPrompt(turn1)) {
		t.Error("a conversation's next turn must extend the previous prompt")
	}
	if EstimateChatTokens(turn1) != 3+(4+1)+(4+1) {
		t.Errorf("EstimateChatTokens = %d", EstimateChatTokens(turn1))
	}
}

func TestBodyPreservesUnknownFields(t *testing.T) {
	b, err := ParseBody([]byte(`{"model":"a","x_ext":{"nested":[1,{"k":"v"}]},"n":2}`))
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if ok, err := b.Get("n", &n); !ok || err != nil || n != 2 {
		t.Errorf("Get n = %d %v %v", n, ok, err)
	}
	_ = b.Set("model", "b")
	out, _ := b.Marshal()
	if string(out) != `{"model":"b","n":2,"x_ext":{"nested":[1,{"k":"v"}]}}` {
		t.Errorf("re-encoded body = %s", out)
	}
	if _, err := ParseBody([]byte(`[1]`)); err == nil {
		t.Error("arrays are not request bodies")
	}
	if _, err := ParseBody([]byte(`null`)); err == nil {
		t.Error("null is not a request body")
	}
}

func TestWriteError(t *testing.T) {
	w := httptest.NewRecorder()
	WriteError(w, 404, TypeInvalidRequest, "model_not_found", "model", "nope")
	if w.Code != 404 || w.Body.String() != `{"error":{"message":"nope","type":"invalid_request_error","param":"model","code":"model_not_found"}}`+"\n" {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
}
