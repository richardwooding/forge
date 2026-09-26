// Package grpcsvc serves forge's tools over gRPC.
//
// One service for every tool, rather than a service generated per tool. That
// is not a simplification: gRPC refuses RegisterService once Serve has begun
// -- it calls logger.Fatalf, which ends the process -- so a tool installed
// while the server is running could never be given a service of its own. A
// fixed service with dynamic payloads is the only shape that survives tools
// coming and going, which is forge's entire premise.
//
// Typed per-tool methods are still possible, through UnknownServiceHandler and
// dynamicpb, and are deliberately not done here. JSON Schema maps onto
// protobuf lossily exactly where the interesting tools live, field numbers
// would need a persisted ledger to stay stable across upgrades, and reflection
// has no list_changed so clients cache descriptors per connection anyway.
// `forge proto export` gives most of that value at a fraction of the risk.
package grpcsvc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/richardwooding/forge/internal/binding"
	"github.com/richardwooding/forge/internal/core"
	"github.com/richardwooding/forge/internal/labels"
	"github.com/richardwooding/forge/internal/policy"
	"github.com/richardwooding/forge/internal/store"
	"github.com/richardwooding/forge/internal/toolkit"
	"github.com/richardwooding/forge/internal/view"
	"github.com/richardwooding/forge/internal/wasmrt/hostabi"

	forgev1 "github.com/richardwooding/forge/grpcapi/forge/v1"
)

// Options configure the service.
type Options struct {
	Toolkit *toolkit.Toolkit

	// Views resolves a named view. Nil means every request gets Default.
	Views *view.Store

	// Default is the selector for requests that name no view.
	Default labels.Selector
}

// Service implements forge.v1.ToolService.
type Service struct {
	forgev1.UnimplementedToolServiceServer
	opts Options
}

// New returns a service.
func New(opts Options) *Service {
	if opts.Default == nil {
		opts.Default = labels.All
	}
	return &Service{opts: opts}
}

// selectorFor resolves the view a request names.
func (s *Service) selectorFor(name string) (labels.Selector, error) {
	if name == "" || s.opts.Views == nil {
		return s.opts.Default, nil
	}
	sel, _, err := s.opts.Views.Resolve(name, "")
	if err != nil {
		// An unknown view serves nothing rather than everything: a typo in a
		// client's config must not widen what is exposed.
		return labels.None, status.Errorf(codes.NotFound, "no view named %q", name)
	}
	return sel, nil
}

// ListTools implements the service.
func (s *Service) ListTools(ctx context.Context, req *forgev1.ListToolsRequest) (*forgev1.ListToolsResponse, error) {
	sel, err := s.selectorFor(req.GetView())
	if err != nil {
		return nil, err
	}
	records, err := s.opts.Toolkit.List(sel)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	out := &forgev1.ListToolsResponse{}
	for _, rec := range records {
		for _, op := range rec.Spec.Ops {
			t, err := s.describe(rec, op)
			if err != nil {
				// A tool forge cannot bind is left out rather than described
				// wrongly; a client generating from a schema forge will not
				// honour is worse than one tool missing.
				continue
			}
			out.Tools = append(out.Tools, t)
		}
	}
	return out, nil
}

// DescribeTool implements the service.
func (s *Service) DescribeTool(ctx context.Context, req *forgev1.DescribeToolRequest) (*forgev1.DescribeToolResponse, error) {
	sel, err := s.selectorFor(req.GetView())
	if err != nil {
		return nil, err
	}
	rec, op, err := s.find(sel, req.GetName())
	if err != nil {
		return nil, err
	}
	t, derr := s.describe(rec, op)
	if derr != nil {
		return nil, status.Error(codes.Internal, derr.Error())
	}
	return &forgev1.DescribeToolResponse{Tool: t, Description: rec.Spec.Description}, nil
}

// find locates the operation a name refers to, within a view.
func (s *Service) find(sel labels.Selector, name string) (store.Record, core.OpSpec, error) {
	records, err := s.opts.Toolkit.List(sel)
	if err != nil {
		return store.Record{}, core.OpSpec{}, status.Error(codes.Internal, err.Error())
	}
	for _, rec := range records {
		for _, op := range rec.Spec.Ops {
			if binding.SurfaceName(rec.Spec.Name, op.Name, len(rec.Spec.Ops) == 1) != name {
				continue
			}
			// Bind now and discard it: a tool that cannot be bound must not be
			// advertised and then fail on the first call.
			if _, err := s.opts.Toolkit.Bound(rec.Spec.Name, op.Name); err != nil {
				return rec, op, status.Error(codes.Internal, err.Error())
			}
			return rec, op, nil
		}
	}
	// Out of view answers NotFound, like every surface but the CLI: a narrow
	// view must not leak the existence of what it hides.
	return store.Record{}, core.OpSpec{}, status.Errorf(codes.NotFound, "no tool named %q", name)
}

func (s *Service) describe(rec store.Record, op core.OpSpec) (*forgev1.Tool, error) {
	b, err := s.opts.Toolkit.Bound(rec.Spec.Name, op.Name)
	if err != nil {
		return nil, err
	}
	summary := op.Summary
	if summary == "" {
		summary = rec.Spec.Summary
	}
	var requires []string
	for _, req := range rec.Spec.Requires {
		requires = append(requires, string(req.Kind))
	}
	return &forgev1.Tool{
		Name:    b.Name,
		Tool:    rec.Spec.Name,
		Op:      op.Name,
		Summary: summary,
		Labels:  rec.Labels(),
		// The canonical bytes, as text. Every surface advertises exactly these.
		InputSchemaJson: string(b.CanonJSON),
		OutputKind:      string(op.OutputKind),
		OutputMediaType: op.OutputMediaType,
		Requires:        requires,
		ReadOnly:        op.Annotations.ReadOnly,
		Destructive:     op.Annotations.Destructive,
		Idempotent:      op.Annotations.Idempotent,
	}, nil
}

// Invoke implements the service.
func (s *Service) Invoke(ctx context.Context, req *forgev1.InvokeRequest) (*forgev1.InvokeResponse, error) {
	sel, err := s.selectorFor(req.GetView())
	if err != nil {
		return nil, err
	}
	input, err := inputBytes(req.GetJson(), req.GetStructValue())
	if err != nil {
		return nil, err
	}
	return s.run(ctx, sel, req.GetName(), input, nil)
}

// InvokeStream implements the service.
func (s *Service) InvokeStream(req *forgev1.InvokeStreamRequest, stream forgev1.ToolService_InvokeStreamServer) error {
	sel, err := s.selectorFor(req.GetView())
	if err != nil {
		return err
	}
	input, err := inputBytes(req.GetJson(), req.GetStructValue())
	if err != nil {
		return err
	}

	send := func(ev *forgev1.InvokeStreamResponse) {
		// A send failure means the client went away; the invocation's context
		// is already cancelled, so there is nothing useful to do with it.
		_ = stream.Send(ev)
	}

	res, err := s.run(stream.Context(), sel, req.GetName(), input, send)
	if err != nil {
		return err
	}
	return stream.Send(&forgev1.InvokeStreamResponse{
		Event: &forgev1.InvokeStreamResponse_Result{Result: res},
	})
}

// inputBytes turns either form of input into canonical JSON.
func inputBytes(raw []byte, st *structpb.Struct) ([]byte, error) {
	if len(raw) > 0 {
		return raw, nil
	}
	if st == nil {
		return []byte("{}"), nil
	}
	// Struct's numbers are doubles, so this is where a value beyond 2^53 loses
	// precision. Documented on the proto field, and asserted by the
	// conformance suite so the divergence cannot quietly become undocumented.
	b, err := st.MarshalJSON()
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "cannot read struct input: %v", err)
	}
	return b, nil
}

// run invokes a tool and shapes the response.
func (s *Service) run(ctx context.Context, sel labels.Selector, name string, input []byte,
	send func(*forgev1.InvokeStreamResponse),
) (*forgev1.InvokeResponse, error) {
	rec, op, err := s.find(sel, name)
	if err != nil {
		return nil, err
	}

	call := toolkit.Call{Tool: rec.Spec.Name, Op: op.Name, Input: input}
	if send != nil {
		call.OnProgress = func(done, total int64, msg string) {
			send(&forgev1.InvokeStreamResponse{Event: &forgev1.InvokeStreamResponse_Progress{
				Progress: &forgev1.Progress{Done: done, Total: total, Message: msg},
			}})
		}
		call.OnLog = func(level hostabi.Level, tool, msg string) {
			send(&forgev1.InvokeStreamResponse{Event: &forgev1.InvokeStreamResponse_Log{
				Log: &forgev1.LogLine{Level: level.String(), Message: msg},
			}})
		}
	}

	res, err := s.opts.Toolkit.Invoke(ctx, call)
	if err != nil {
		return nil, statusFor(err)
	}

	out := &forgev1.InvokeResponse{
		Stdout:    res.Stdout,
		Stderr:    res.Stderr,
		Truncated: res.Truncated,
		WallNanos: res.Usage.Wall,
	}
	if res.Rendition.ToolError {
		// OK with is_error, not a non-OK status. A status would hide the
		// message in status.Details(), where most clients never look, and
		// would conflate "the tool said no" with "forge could not run it".
		out.IsError = true
		out.Error = res.Rendition.Message
		return out, nil
	}

	switch res.Rendition.Kind {
	case core.OutputText:
		out.Text = res.Rendition.Text
	case core.OutputBytes:
		out.Binary = res.Rendition.Bytes
		out.MediaType = res.Rendition.MediaType
	default:
		out.Json = res.Rendition.JSON
	}
	return out, nil
}

// statusFor maps an invocation failure onto a gRPC status, through binding's
// parity table rather than a mapping invented here.
func statusFor(err error) error {
	if needs, ok := errors.AsType[*policy.NeedsApproval](err); ok {
		return status.Error(codes.PermissionDenied, needs.Error())
	}
	f, ok := binding.AsFault(err)
	if !ok {
		return status.Error(codes.Internal, err.Error())
	}
	switch f.Code {
	case binding.FaultInvalidInput:
		return status.Error(codes.InvalidArgument, violationDetail(f))
	case binding.FaultNotFound, binding.FaultNotInView:
		return status.Error(codes.NotFound, f.Message)
	case binding.FaultDeadline:
		return status.Error(codes.DeadlineExceeded, f.Message)
	case binding.FaultExhausted:
		return status.Error(codes.ResourceExhausted, f.Message)
	default:
		return status.Error(codes.Internal, f.Message)
	}
}

// violationDetail appends the JSON Pointers to the message.
//
// They go in the message rather than in status details because a client that
// does not unpack details would otherwise be told only that something was
// wrong, and forge's other surfaces all say which field.
func violationDetail(f *binding.Fault) string {
	if len(f.Violations) == 0 {
		return f.Message
	}
	var msg strings.Builder
	msg.WriteString(f.Message)
	for _, v := range f.Violations {
		fmt.Fprintf(&msg, "\n  %s: %s", v.Pointer, v.Message)
	}
	return msg.String()
}
