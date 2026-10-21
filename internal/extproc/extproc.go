// Package extproc implements the Gateway API Inference Extension
// endpoint-picker protocol as an Envoy external processor.
//
// Envoy streams each HTTP request to Process. The request body is buffered
// (Envoy is expected to use FULL_DUPLEX_STREAMED body mode, which is what
// Envoy AI Gateway and the reference picker use), parsed as an OpenAI
// request, and an endpoint is chosen with the shared picker package. The
// choice is returned both as the x-gateway-destination-endpoint header and
// as envoy.lb dynamic metadata; Envoy's original-destination cluster then
// connects to that ip:port. If the model name maps to a LoRA adapter, the
// body's "model" is rewritten to the adapter name.
package extproc

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/endpoint"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/openai"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/picker"
	"github.com/nitishsjsucs/multi-tenant-inference-gateway/internal/routing"
)

// Protocol keys from the endpoint picker protocol.
const (
	DestinationHeader     = "x-gateway-destination-endpoint"
	DestinationNamespace  = "envoy.lb"
	SubsetHintNamespace   = "envoy.lb.subset_hint"
	SubsetHintKey         = "x-gateway-destination-endpoint-subset"
	maxBodyBytes          = 16 << 20
	streamedBodyChunkSize = 62000 // keep each streamed body message well under Envoy's buffer limits
)

// Server is the ext_proc service.
type Server struct {
	extprocv3.UnimplementedExternalProcessorServer

	pool     *endpoint.Pool
	picker   picker.Picker
	resolver *routing.Resolver
	log      *slog.Logger

	requests  *prometheus.CounterVec
	decisions *prometheus.CounterVec
}

// New builds an endpoint picker for one pool.
func New(pool *endpoint.Pool, p picker.Picker, resolver *routing.Resolver, reg prometheus.Registerer, log *slog.Logger) *Server {
	s := &Server{
		pool: pool, picker: p, resolver: resolver, log: log,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "epp_requests_total",
			Help: "Requests handled by the endpoint picker, by result.",
		}, []string{"result"}),
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "epp_picker_decisions_total",
			Help: "Endpoint picker decisions by picker and endpoint.",
		}, []string{"picker", "endpoint"}),
	}
	reg.MustRegister(s.requests, s.decisions)
	return s
}

// stream is the state of one HTTP request.
type stream struct {
	subset         map[string]bool // allowed ip:port from the subset hint; nil = any
	body           []byte
	headersPending bool // the request-headers reply waits for the full body
	picked         *endpoint.Endpoint
}

// Process implements the ext_proc bidirectional stream.
func (s *Server) Process(srv extprocv3.ExternalProcessor_ProcessServer) error {
	st := &stream{}
	defer func() {
		if st.picked != nil {
			st.picked.Release()
		}
	}()
	for {
		req, err := srv.Recv()
		if errors.Is(err, io.EOF) || status.Code(err) == codes.Canceled {
			return nil
		}
		if err != nil {
			return err
		}
		for _, resp := range s.handle(st, req) {
			if err := srv.Send(resp); err != nil {
				return err
			}
		}
	}
}

// handle returns the responses for one message from Envoy.
func (s *Server) handle(st *stream, req *extprocv3.ProcessingRequest) []*extprocv3.ProcessingResponse {
	switch v := req.Request.(type) {
	case *extprocv3.ProcessingRequest_RequestHeaders:
		st.subset = subsetHint(req.GetMetadataContext())
		if v.RequestHeaders.GetEndOfStream() {
			return s.finish(st) // no body, e.g. GET /v1/models
		}
		st.headersPending = true
		return nil
	case *extprocv3.ProcessingRequest_RequestBody:
		if len(st.body)+len(v.RequestBody.GetBody()) > maxBodyBytes {
			s.requests.WithLabelValues("too_large").Inc()
			return []*extprocv3.ProcessingResponse{immediate(typev3.StatusCode_PayloadTooLarge,
				openai.NewError(openai.TypeInvalidRequest, "request_too_large", "", "request body exceeds 16 MiB"))}
		}
		st.body = append(st.body, v.RequestBody.GetBody()...)
		if v.RequestBody.GetEndOfStream() {
			return s.finish(st)
		}
		return nil
	case *extprocv3.ProcessingRequest_RequestTrailers:
		var out []*extprocv3.ProcessingResponse
		if st.headersPending {
			out = s.finish(st) // the body ended without end_of_stream
		}
		return append(out, &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_RequestTrailers{
			RequestTrailers: &extprocv3.TrailersResponse{}}})
	case *extprocv3.ProcessingRequest_ResponseHeaders:
		return []*extprocv3.ProcessingResponse{{Response: &extprocv3.ProcessingResponse_ResponseHeaders{
			ResponseHeaders: &extprocv3.HeadersResponse{}}}}
	case *extprocv3.ProcessingRequest_ResponseBody:
		// Full-duplex mode: the response body must be streamed back.
		var out []*extprocv3.ProcessingResponse
		for _, c := range streamedChunks(v.ResponseBody.GetBody(), v.ResponseBody.GetEndOfStream()) {
			out = append(out, &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_ResponseBody{
				ResponseBody: &extprocv3.BodyResponse{Response: c}}})
		}
		return out
	case *extprocv3.ProcessingRequest_ResponseTrailers:
		return []*extprocv3.ProcessingResponse{{Response: &extprocv3.ProcessingResponse_ResponseTrailers{
			ResponseTrailers: &extprocv3.TrailersResponse{}}}}
	}
	return nil
}

// finish picks an endpoint for the buffered request and emits the deferred
// headers response followed by the (possibly rewritten) body.
func (s *Server) finish(st *stream) []*extprocv3.ProcessingResponse {
	hadBody := st.headersPending
	st.headersPending = false
	body := st.body
	preq := &picker.Request{}

	if len(body) > 0 {
		parsed, err := openai.ParseBody(body)
		if err != nil {
			s.requests.WithLabelValues("bad_request").Inc()
			return []*extprocv3.ProcessingResponse{immediate(typev3.StatusCode_BadRequest,
				openai.NewError(openai.TypeInvalidRequest, "", "", err.Error()))}
		}
		var model string
		if ok, _ := parsed.Get("model", &model); ok {
			route, err := s.resolver.Resolve(nil, model)
			if err != nil {
				s.requests.WithLabelValues("model_not_found").Inc()
				return []*extprocv3.ProcessingResponse{immediate(typev3.StatusCode_NotFound,
					openai.NewError(openai.TypeInvalidRequest, "model_not_found", "model", "The model `"+model+"` does not exist."))}
			}
			preq.Model, preq.Adapter = route.Upstream, route.Adapter
			if route.Upstream != model {
				_ = parsed.Set("model", route.Upstream)
				if body, err = parsed.Marshal(); err != nil {
					s.requests.WithLabelValues("bad_request").Inc()
					return []*extprocv3.ProcessingResponse{immediate(typev3.StatusCode_BadRequest,
						openai.NewError(openai.TypeInvalidRequest, "", "", err.Error()))}
				}
			}
		}
		preq.Prompt = promptText(parsed)
	}

	candidates := s.pool.Ready()
	if st.subset != nil {
		var allowed []*endpoint.Endpoint
		for _, ep := range candidates {
			if st.subset[ep.Addr] {
				allowed = append(allowed, ep)
			}
		}
		candidates = allowed
	}
	choice, err := s.picker.Pick(preq, candidates)
	if err != nil {
		s.requests.WithLabelValues("no_endpoints").Inc()
		return []*extprocv3.ProcessingResponse{immediate(typev3.StatusCode_ServiceUnavailable,
			openai.NewError(openai.TypeUnavailable, "no_endpoints", "", "no ready model server endpoints"))}
	}
	st.picked = choice.Endpoint
	st.picked.Acquire()
	s.requests.WithLabelValues("picked").Inc()
	s.decisions.WithLabelValues(s.picker.Name(), st.picked.Addr).Inc()
	s.log.Debug("picked endpoint", "endpoint", st.picked.Addr, "model", preq.Model,
		"matched_blocks", choice.MatchedBlocks, "total_blocks", choice.TotalBlocks)

	setHeaders := []*corev3.HeaderValueOption{header(DestinationHeader, st.picked.Addr)}
	if hadBody {
		setHeaders = append(setHeaders, header("content-length", strconv.Itoa(len(body))))
	}
	out := []*extprocv3.ProcessingResponse{{
		Response: &extprocv3.ProcessingResponse_RequestHeaders{RequestHeaders: &extprocv3.HeadersResponse{
			Response: &extprocv3.CommonResponse{
				ClearRouteCache: true,
				HeaderMutation:  &extprocv3.HeaderMutation{SetHeaders: setHeaders},
			},
		}},
		DynamicMetadata: &structpb.Struct{Fields: map[string]*structpb.Value{
			DestinationNamespace: structpb.NewStructValue(&structpb.Struct{Fields: map[string]*structpb.Value{
				DestinationHeader: structpb.NewStringValue(st.picked.Addr),
			}}),
		}},
	}}
	if hadBody {
		for _, c := range streamedChunks(body, true) {
			out = append(out, &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_RequestBody{
				RequestBody: &extprocv3.BodyResponse{Response: c}}})
		}
	}
	st.body = nil
	return out
}

// promptText extracts the text used for prefix matching.
func promptText(b openai.Body) string {
	var msgs []openai.Message
	if ok, err := b.Get("messages", &msgs); ok && err == nil {
		return openai.ChatPrompt(msgs)
	}
	var s string
	if ok, err := b.Get("prompt", &s); ok && err == nil {
		return s
	}
	return ""
}

// subsetHint reads the optional list of endpoints the proxy allows.
func subsetHint(md *corev3.Metadata) map[string]bool {
	ns, ok := md.GetFilterMetadata()[SubsetHintNamespace]
	if !ok {
		return nil
	}
	v, ok := ns.GetFields()[SubsetHintKey]
	if !ok {
		return nil
	}
	allowed := map[string]bool{}
	add := func(s string) {
		for _, ep := range strings.Split(s, ",") {
			if ep = strings.TrimSpace(ep); ep != "" {
				allowed[ep] = true
			}
		}
	}
	add(v.GetStringValue())
	for _, item := range v.GetListValue().GetValues() {
		add(item.GetStringValue())
	}
	return allowed
}

func header(key, value string) *corev3.HeaderValueOption {
	return &corev3.HeaderValueOption{Header: &corev3.HeaderValue{Key: key, RawValue: []byte(value)}}
}

func streamedChunks(body []byte, eos bool) []*extprocv3.CommonResponse {
	var out []*extprocv3.CommonResponse
	for {
		n := min(len(body), streamedBodyChunkSize)
		last := n == len(body)
		out = append(out, &extprocv3.CommonResponse{BodyMutation: &extprocv3.BodyMutation{
			Mutation: &extprocv3.BodyMutation_StreamedResponse{StreamedResponse: &extprocv3.StreamedBodyResponse{
				Body: body[:n], EndOfStream: eos && last,
			}},
		}})
		if last {
			return out
		}
		body = body[n:]
	}
}

func immediate(code typev3.StatusCode, body openai.ErrorBody) *extprocv3.ProcessingResponse {
	payload, _ := json.Marshal(body)
	return &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_ImmediateResponse{
		ImmediateResponse: &extprocv3.ImmediateResponse{
			Status:  &typev3.HttpStatus{Code: code},
			Headers: &extprocv3.HeaderMutation{SetHeaders: []*corev3.HeaderValueOption{header("content-type", "application/json")}},
			Body:    payload,
		},
	}}
}
