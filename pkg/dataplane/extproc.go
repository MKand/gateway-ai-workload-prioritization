package dataplane

import (
	"errors"
	"io"
	"sort"
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
}

// NewExtProcServer wraps an Evaluator as an Envoy ext_proc server.
func NewExtProcServer(e *Evaluator) *ExtProcServer {
	return &ExtProcServer{evaluator: e}
}

// Process handles one HTTP stream from Envoy.
func (s *ExtProcServer) Process(stream extprocv3.ExternalProcessor_ProcessServer) error {
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
			resp = s.handleRequestHeaders(r.RequestHeaders)
		case *extprocv3.ProcessingRequest_RequestBody:
			resp = &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_RequestBody{RequestBody: &extprocv3.BodyResponse{}}}
		case *extprocv3.ProcessingRequest_RequestTrailers:
			resp = &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_RequestTrailers{RequestTrailers: &extprocv3.TrailersResponse{}}}
		case *extprocv3.ProcessingRequest_ResponseHeaders:
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

func (s *ExtProcServer) handleRequestHeaders(h *extprocv3.HttpHeaders) *extprocv3.ProcessingResponse {
	res := s.evaluator.Evaluate(headerMap(h))

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
