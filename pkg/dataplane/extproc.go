package dataplane

import (
	"errors"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ExtProcServer implements Envoy's ExternalProcessor service. It only acts on
// the request headers phase; all other phases are acknowledged unchanged.
type ExtProcServer struct {
	extprocv3.UnimplementedExternalProcessorServer
	evaluator *Evaluator
	metrics   *Metrics
}

// NewExtProcServer wraps an Evaluator as an Envoy ext_proc server.
func NewExtProcServer(e *Evaluator) *ExtProcServer {
	return &ExtProcServer{evaluator: e}
}

// WithMetrics enables metrics and returns the server. nil disables them.
func (s *ExtProcServer) WithMetrics(m *Metrics) *ExtProcServer {
	s.metrics = m
	return s
}

// streamState is what one HTTP stream's request-headers phase learned, kept
// so the response-headers phase can attribute the upstream status.
type streamState struct {
	result Result
	seen   bool
}

// Process handles one HTTP stream from Envoy.
func (s *ExtProcServer) Process(stream extprocv3.ExternalProcessor_ProcessServer) error {
	var state streamState
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if status.Code(err) == codes.Canceled {
				return nil
			}
			return err
		}

		var resp *extprocv3.ProcessingResponse
		switch r := req.Request.(type) {
		case *extprocv3.ProcessingRequest_RequestHeaders:
			res := s.evaluator.Evaluate(headerMap(r.RequestHeaders))
			state = streamState{result: res, seen: true}
			s.metrics.observeDecision(res)
			resp = s.responseFor(res)
		case *extprocv3.ProcessingRequest_RequestBody:
			resp = &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_RequestBody{RequestBody: &extprocv3.BodyResponse{}}}
		case *extprocv3.ProcessingRequest_RequestTrailers:
			resp = &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_RequestTrailers{RequestTrailers: &extprocv3.TrailersResponse{}}}
		case *extprocv3.ProcessingRequest_ResponseHeaders:
			s.observeResponse(state, r.ResponseHeaders)
			resp = &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_ResponseHeaders{ResponseHeaders: &extprocv3.HeadersResponse{}}}
		case *extprocv3.ProcessingRequest_ResponseBody:
			resp = &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_ResponseBody{ResponseBody: &extprocv3.BodyResponse{}}}
		case *extprocv3.ProcessingRequest_ResponseTrailers:
			resp = &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_ResponseTrailers{ResponseTrailers: &extprocv3.TrailersResponse{}}}
		default:
			return status.Errorf(codes.InvalidArgument, "unknown ext_proc request type %T", r)
		}

		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

// observeResponse records the upstream status of an admitted request. Sheds
// are answered by the governor itself (ImmediateResponse), so Envoy never
// sends their response headers here and they cannot be miscounted as
// upstream 429s.
func (s *ExtProcServer) observeResponse(state streamState, h *extprocv3.HttpHeaders) {
	if !state.seen || state.result.Shed || state.result.Target == "" {
		return
	}
	code, err := strconv.Atoi(headerMap(h)[":status"])
	if err != nil {
		return
	}
	s.metrics.observeUpstream(state.result.Target, state.result.Priority, code)
	if code == 429 {
		slog.Warn("upstream 429 from Vertex AI",
			"target", state.result.Target,
			"priority", state.result.Priority,
			"decision", state.result.Decision,
			"estimated_tokens", state.result.Tokens)
	}
}

func (s *ExtProcServer) handleRequestHeaders(h *extprocv3.HttpHeaders) *extprocv3.ProcessingResponse {
	return s.responseFor(s.evaluator.Evaluate(headerMap(h)))
}

func (s *ExtProcServer) responseFor(res Result) *extprocv3.ProcessingResponse {
	if res.Shed {
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_ImmediateResponse{
				ImmediateResponse: &extprocv3.ImmediateResponse{
					Status:  &typev3.HttpStatus{Code: typev3.StatusCode_TooManyRequests},
					Headers: &extprocv3.HeaderMutation{SetHeaders: toHeaderOptions(res.SetHeaders)},
					Body:    []byte(res.SetHeaders[HeaderReason] + "\n"),
					Details: "governor_shed",
				},
			},
		}
	}

	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extprocv3.HeadersResponse{
				Response: &extprocv3.CommonResponse{
					HeaderMutation: &extprocv3.HeaderMutation{
						SetHeaders:    toHeaderOptions(res.SetHeaders),
						RemoveHeaders: res.RemoveHeaders,
					},
					ClearRouteCache: res.RouteChanged,
				},
			},
		},
	}
}

// headerMap flattens Envoy headers into a lower-case map. Envoy may send
// values in either Value or RawValue depending on version/config.
func headerMap(h *extprocv3.HttpHeaders) map[string]string {
	out := map[string]string{}
	if h == nil || h.GetHeaders() == nil {
		return out
	}
	for _, hv := range h.GetHeaders().GetHeaders() {
		v := hv.GetValue()
		if v == "" && len(hv.GetRawValue()) > 0 {
			v = string(hv.GetRawValue())
		}
		out[strings.ToLower(hv.GetKey())] = v
	}
	return out
}

// toHeaderOptions converts a header map to Envoy overwrite-or-add mutations,
// sorted by key for deterministic output.
func toHeaderOptions(m map[string]string) []*corev3.HeaderValueOption {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	opts := make([]*corev3.HeaderValueOption, 0, len(keys))
	for _, k := range keys {
		opts = append(opts, &corev3.HeaderValueOption{
			Header:       &corev3.HeaderValue{Key: k, RawValue: []byte(m[k])},
			AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		})
	}
	return opts
}
