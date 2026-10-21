package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/openai"
)

const maxResponseBody = 64 << 20

// streamResult summarises a relayed SSE stream.
type streamResult struct {
	usage  *openai.Usage // from the final usage chunk, if any
	chunks int64         // chunks that carried choices
}

// relayStream copies a server-sent-event stream to the client, flushing
// after every event so tokens reach the client as they are generated. It
// records the usage chunk and, if stripUsage is set, does not forward the
// usage-only chunk (the client didn't ask for it). onFirst runs when the
// first chunk with choices is forwarded.
func relayStream(w http.ResponseWriter, upstream io.Reader, stripUsage bool, onFirst func()) streamResult {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	var (
		res   streamResult
		event bytes.Buffer // raw bytes of the event being read
		data  bytes.Buffer // its data: payload
	)
	emit := func() bool {
		defer func() { event.Reset(); data.Reset() }()
		if payload := data.Bytes(); len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) {
			var chunk struct {
				Choices []json.RawMessage `json:"choices"`
				Usage   *openai.Usage     `json:"usage"`
			}
			if json.Unmarshal(payload, &chunk) == nil {
				if chunk.Usage != nil {
					res.usage = chunk.Usage
				}
				if len(chunk.Choices) == 0 && chunk.Usage != nil && stripUsage {
					return true
				}
				if len(chunk.Choices) > 0 {
					res.chunks++
					if res.chunks == 1 && onFirst != nil {
						onFirst()
					}
				}
			}
		}
		if _, err := w.Write(event.Bytes()); err != nil {
			return false // client went away
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}

	rd := bufio.NewReaderSize(upstream, 64<<10)
	for {
		line, err := rd.ReadBytes('\n')
		if len(line) > 0 {
			event.Write(line)
			content := bytes.TrimRight(line, "\r\n")
			switch {
			case len(content) == 0: // blank line ends an event
				if !emit() {
					return res
				}
			case bytes.HasPrefix(content, []byte("data:")):
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.Write(bytes.TrimPrefix(bytes.TrimPrefix(content, []byte("data:")), []byte(" ")))
			}
		}
		if err != nil {
			if event.Len() > 0 {
				emit()
			}
			return res
		}
	}
}

// relayBuffered forwards a complete JSON response. It returns the usage it
// reported and the status sent to the client.
func relayBuffered(w http.ResponseWriter, resp *http.Response) (*openai.Usage, int) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		openai.WriteError(w, http.StatusBadGateway, openai.TypeServer, "upstream_error", "", "reading model server response failed")
		return nil, http.StatusBadGateway
	}
	var parsed struct {
		Usage *openai.Usage `json:"usage"`
	}
	_ = json.Unmarshal(body, &parsed)
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
	return parsed.Usage, resp.StatusCode
}

// relayError forwards a non-200 upstream response unchanged.
func relayError(w http.ResponseWriter, resp *http.Response) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}
