package extproc

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/config"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/endpoint"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/picker"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/routing"
)

type fixture struct {
	client extprocv3.ExternalProcessorClient
	pool   *endpoint.Pool
}

func newFixture(t *testing.T, urls ...string) *fixture {
	t.Helper()
	cfg := &config.Config{
		Pools:  []config.Pool{{Name: "llama", BaseModel: "meta-llama/Llama-3.1-8B-Instruct", Adapters: []string{"support-lora-v2"}}},
		Models: []config.Model{{Name: "support-bot", Pool: "llama", Adapter: "support-lora-v2"}},
	}
	pool, err := endpoint.NewPool(endpoint.PoolOptions{Name: "llama", URLs: urls})
	if err != nil {
		t.Fatal(err)
	}
	srv := New(pool, &picker.RoundRobin{}, routing.NewResolver(cfg), prometheus.NewRegistry(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	extprocv3.RegisterExternalProcessorServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &fixture{client: extprocv3.NewExternalProcessorClient(conn), pool: pool}
}

func (f *fixture) open(t *testing.T) extprocv3.ExternalProcessor_ProcessClient {
	t.Helper()
	stream, err := f.client.Process(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return stream
}

func send(t *testing.T, s extprocv3.ExternalProcessor_ProcessClient, req *extprocv3.ProcessingRequest) {
	t.Helper()
	if err := s.Send(req); err != nil {
		t.Fatal(err)
	}
}

func recv(t *testing.T, s extprocv3.ExternalProcessor_ProcessClient) *extprocv3.ProcessingResponse {
	t.Helper()
	resp, err := s.Recv()
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func headers(eos bool) *extprocv3.ProcessingRequest {
	return &extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_RequestHeaders{
		RequestHeaders: &extprocv3.HttpHeaders{
			Headers:     &corev3.HeaderMap{Headers: []*corev3.HeaderValue{{Key: ":path", RawValue: []byte("/v1/chat/completions")}}},
			EndOfStream: eos,
		}}}
}

func bodyChunk(b string, eos bool) *extprocv3.ProcessingRequest {
	return &extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_RequestBody{
		RequestBody: &extprocv3.HttpBody{Body: []byte(b), EndOfStream: eos}}}
}

func destination(t *testing.T, resp *extprocv3.ProcessingResponse) (header, metadata string, mutation map[string]string) {
	t.Helper()
	hr := resp.GetRequestHeaders()
	if hr == nil {
		t.Fatalf("want a request-headers response, got %v", resp)
	}
	mutation = map[string]string{}
	for _, h := range hr.GetResponse().GetHeaderMutation().GetSetHeaders() {
		mutation[h.GetHeader().GetKey()] = string(h.GetHeader().GetRawValue())
	}
	md := resp.GetDynamicMetadata().GetFields()[DestinationNamespace].GetStructValue().GetFields()[DestinationHeader].GetStringValue()
	return mutation[DestinationHeader], md, mutation
}

func TestFullDuplexRequestRewritesModelAndPicks(t *testing.T) {
	f := newFixture(t, "http://10.0.0.1:8000", "http://10.0.0.2:8000")
	s := f.open(t)

	body := `{"model":"support-bot","stream":true,"messages":[{"role":"user","content":"` + strings.Repeat("x", 100) + `"}],"user":"u-7"}`
	send(t, s, headers(false))
	send(t, s, bodyChunk(body[:40], false)) // the body arrives in pieces
	send(t, s, bodyChunk(body[40:], true))

	header, md, mutation := destination(t, recv(t, s))
	if header != "10.0.0.1:8000" || md != header {
		t.Fatalf("destination header %q, metadata %q", header, md)
	}
	var rewritten []byte
	for {
		resp := recv(t, s)
		sr := resp.GetRequestBody().GetResponse().GetBodyMutation().GetStreamedResponse()
		if sr == nil {
			t.Fatalf("want a streamed body response, got %v", resp)
		}
		rewritten = append(rewritten, sr.GetBody()...)
		if sr.GetEndOfStream() {
			break
		}
	}
	var got map[string]any
	if err := json.Unmarshal(rewritten, &got); err != nil {
		t.Fatalf("rewritten body: %v: %s", err, rewritten)
	}
	if got["model"] != "support-lora-v2" || got["user"] != "u-7" || got["stream"] != true {
		t.Errorf("rewritten body = %v", got)
	}
	if mutation["content-length"] != strconv.Itoa(len(rewritten)) {
		t.Errorf("content-length %q, body is %d bytes", mutation["content-length"], len(rewritten))
	}
	if f.pool.Endpoints()[0].InFlight() != 1 {
		t.Error("picked endpoint should count the in-flight request")
	}

	// Response phases are passed through.
	send(t, s, &extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_ResponseHeaders{ResponseHeaders: &extprocv3.HttpHeaders{}}})
	if recv(t, s).GetResponseHeaders() == nil {
		t.Error("want a response-headers reply")
	}
	send(t, s, &extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_ResponseBody{
		ResponseBody: &extprocv3.HttpBody{Body: []byte("data: [DONE]\n\n"), EndOfStream: true}}})
	echo := recv(t, s).GetResponseBody().GetResponse().GetBodyMutation().GetStreamedResponse()
	if string(echo.GetBody()) != "data: [DONE]\n\n" || !echo.GetEndOfStream() {
		t.Errorf("response body echo = %v", echo)
	}
	if err := s.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recv(); err != io.EOF {
		t.Errorf("stream should end cleanly, got %v", err)
	}
	if f.pool.Endpoints()[0].InFlight() != 0 {
		t.Error("in-flight count not released when the stream ended")
	}

	// The next request goes to the other endpoint (round-robin) and keeps a
	// model that needs no rewrite untouched.
	s2 := f.open(t)
	send(t, s2, headers(false))
	send(t, s2, bodyChunk(`{"model":"meta-llama/Llama-3.1-8B-Instruct","prompt":"hi"}`, true))
	if header, _, _ := destination(t, recv(t, s2)); header != "10.0.0.2:8000" {
		t.Errorf("second pick = %s", header)
	}
	if b := recv(t, s2).GetRequestBody().GetResponse().GetBodyMutation().GetStreamedResponse().GetBody(); string(b) != `{"model":"meta-llama/Llama-3.1-8B-Instruct","prompt":"hi"}` {
		t.Errorf("body changed: %s", b)
	}
}

func TestHeadersOnlyRequest(t *testing.T) {
	f := newFixture(t, "http://10.0.0.1:8000")
	s := f.open(t)
	send(t, s, headers(true))
	header, _, mutation := destination(t, recv(t, s))
	if header != "10.0.0.1:8000" {
		t.Errorf("destination %q", header)
	}
	if _, ok := mutation["content-length"]; ok {
		t.Error("no body, so no content-length mutation")
	}
}

func TestTrailersCompleteBufferedBody(t *testing.T) {
	f := newFixture(t, "http://10.0.0.1:8000")
	s := f.open(t)
	send(t, s, headers(false))
	send(t, s, bodyChunk(`{"model":"support-bot","prompt":"x"}`, false))
	send(t, s, &extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_RequestTrailers{RequestTrailers: &extprocv3.HttpTrailers{}}})
	if header, _, _ := destination(t, recv(t, s)); header != "10.0.0.1:8000" {
		t.Errorf("destination %q", header)
	}
	if recv(t, s).GetRequestBody() == nil {
		t.Error("want the body response")
	}
	if recv(t, s).GetRequestTrailers() == nil {
		t.Error("want the trailers response")
	}
}

func TestSubsetHint(t *testing.T) {
	f := newFixture(t, "http://10.0.0.1:8000", "http://10.0.0.2:8000", "http://10.0.0.3:8000")
	hint := func(v *structpb.Value) *extprocv3.ProcessingRequest {
		req := headers(true)
		req.MetadataContext = &corev3.Metadata{FilterMetadata: map[string]*structpb.Struct{
			SubsetHintNamespace: {Fields: map[string]*structpb.Value{SubsetHintKey: v}},
		}}
		return req
	}
	list, _ := structpb.NewList([]any{"10.0.0.3:8000"})
	for range 3 {
		s := f.open(t)
		send(t, s, hint(structpb.NewListValue(list)))
		if header, _, _ := destination(t, recv(t, s)); header != "10.0.0.3:8000" {
			t.Fatalf("picked %s outside the subset", header)
		}
	}
	s := f.open(t)
	send(t, s, hint(structpb.NewStringValue("10.9.9.9:8000")))
	if ir := recv(t, s).GetImmediateResponse(); ir.GetStatus().GetCode() != typev3.StatusCode_ServiceUnavailable {
		t.Errorf("empty subset: want 503, got %v", ir)
	}
}

func TestImmediateErrors(t *testing.T) {
	cases := map[string]struct {
		urls []string
		body string
		code typev3.StatusCode
	}{
		"unknown model": {[]string{"http://10.0.0.1:8000"}, `{"model":"gpt-4o","messages":[]}`, typev3.StatusCode_NotFound},
		"invalid json":  {[]string{"http://10.0.0.1:8000"}, `{"model":`, typev3.StatusCode_BadRequest},
		"no endpoints":  {nil, `{"model":"support-bot","prompt":"x"}`, typev3.StatusCode_ServiceUnavailable},
	}
	for name, tc := range cases {
		f := newFixture(t, tc.urls...)
		s := f.open(t)
		send(t, s, headers(false))
		send(t, s, bodyChunk(tc.body, true))
		ir := recv(t, s).GetImmediateResponse()
		if ir.GetStatus().GetCode() != tc.code {
			t.Errorf("%s: status %v, want %v", name, ir.GetStatus().GetCode(), tc.code)
			continue
		}
		var e map[string]any
		if err := json.Unmarshal(ir.GetBody(), &e); err != nil || e["error"] == nil {
			t.Errorf("%s: body is not an OpenAI error: %s", name, ir.GetBody())
		}
	}
}

func TestStreamedChunksSplitLargeBodies(t *testing.T) {
	chunks := streamedChunks(make([]byte, 2*streamedBodyChunkSize+1), true)
	if len(chunks) != 3 {
		t.Fatalf("%d chunks", len(chunks))
	}
	for i, c := range chunks {
		if eos := c.GetBodyMutation().GetStreamedResponse().GetEndOfStream(); eos != (i == 2) {
			t.Errorf("chunk %d end_of_stream = %v", i, eos)
		}
	}
}
