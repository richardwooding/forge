package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/richardwooding/forge/internal/binding"
	"github.com/richardwooding/forge/internal/labels"
	"github.com/richardwooding/forge/internal/surface/cli"
	"github.com/richardwooding/forge/internal/surface/grpcsvc"
	"github.com/richardwooding/forge/internal/surface/mcpsrv"
	"github.com/richardwooding/forge/internal/surface/rest"
	"github.com/richardwooding/forge/internal/toolkit"
	"github.com/richardwooding/forge/internal/view"

	forgev1 "github.com/richardwooding/forge/grpcapi/forge/v1"
)

// Setup is what every driver is built from.
type Setup struct {
	Toolkit  *toolkit.Toolkit
	Views    *view.Store
	Selector labels.Selector
}

// ---------------------------------------------------------------- CLI

// CLIDriver runs lines through the command tree, as a terminal would.
type CLIDriver struct {
	d   *cli.Dispatcher
	set Setup
}

// NewCLIDriver returns a driver over the CLI.
func NewCLIDriver(s Setup) *CLIDriver {
	return &CLIDriver{
		d:   cli.NewDispatcher(cli.Options{Toolkit: s.Toolkit, Selector: s.Selector}),
		set: s,
	}
}

func (d *CLIDriver) Name() string { return "cli" }

func (d *CLIDriver) List(ctx context.Context) ([]string, error) {
	return surfaceNames(d.set)
}

func (d *CLIDriver) Describe(ctx context.Context, name string) ([]byte, error) {
	return canonicalSchema(d.set, name)
}

// Invoke goes through `forge run`, which takes a whole document, rather than
// through flags. Flags are covered by binding's own round-trip fuzz; what this
// harness compares is the shared path every surface reaches afterwards, and
// routing the CLI through flags would make it the only driver testing its own
// encoder as well.
func (d *CLIDriver) Invoke(ctx context.Context, name string, input json.RawMessage) (Outcome, error) {
	tool, op, err := splitSurfaceName(d.set, name)
	if err != nil {
		// Not a harness failure: "no such tool in this view" is an outcome
		// every surface has to report, and comparing them is the point.
		return Outcome{Class: ClassNotFound}, nil //nolint:nilerr // not-found is an outcome, not an error
	}

	stdout, stderr, runErr := d.d.DispatchInput(ctx, []string{"run", tool, op}, input)
	if runErr != nil {
		if errors.Is(runErr, cli.ErrToolFailed) {
			// The tool's own words went to stderr; the classification came
			// back as the error.
			return Outcome{Class: ClassToolError, Message: strings.TrimSpace(stderr)}, nil
		}
		return classifyCLI(runErr), nil
	}
	return Outcome{Class: ClassOK, Output: canonJSON([]byte(stdout))}, nil
}

func classifyCLI(err error) Outcome {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "is installed but not in"),
		strings.Contains(msg, "no tool named"),
		strings.Contains(msg, "unknown command"):
		return Outcome{Class: ClassNotFound}
	case strings.Contains(msg, "required property"),
		strings.Contains(msg, "expected "),
		strings.Contains(msg, "problems with the input"),
		strings.Contains(msg, "not one of"):
		return Outcome{Class: ClassInvalidInput}
	case strings.Contains(msg, "capability"):
		return Outcome{Class: ClassInvalidInput}
	default:
		return Outcome{Class: ClassToolError, Message: msg}
	}
}

// ---------------------------------------------------------------- MCP

// MCPDriver drives a real client over the SDK's in-memory transport.
type MCPDriver struct {
	sess *mcp.ClientSession
	set  Setup
}

// NewMCPDriver connects a client to a server for this setup.
func NewMCPDriver(ctx context.Context, s Setup) (*MCPDriver, func(), error) {
	mgr := mcpsrv.New(mcpsrv.Options{Toolkit: s.Toolkit, Views: s.Views})
	srv, err := mgr.Server("", s.Selector)
	if err != nil {
		return nil, nil, err
	}

	clientT, serverT := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverT) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "conformance", Version: "0"}, nil)
	sess, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		return nil, nil, err
	}
	return &MCPDriver{sess: sess, set: s}, func() { _ = sess.Close() }, nil
}

func (d *MCPDriver) Name() string { return "mcp" }

func (d *MCPDriver) List(ctx context.Context) ([]string, error) {
	res, err := d.sess.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, t := range res.Tools {
		if strings.HasPrefix(t.Name, "forge_") {
			continue // forge's own meta-tools are not the tools under test
		}
		names = append(names, t.Name)
	}
	return sortedCopy(names), nil
}

func (d *MCPDriver) Describe(ctx context.Context, name string) ([]byte, error) {
	res, err := d.sess.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	for _, t := range res.Tools {
		if t.Name == name {
			return json.Marshal(t.InputSchema)
		}
	}
	return nil, fmt.Errorf("mcp: no tool named %q", name)
}

func (d *MCPDriver) Invoke(ctx context.Context, name string, input json.RawMessage) (Outcome, error) {
	var args map[string]any
	if len(input) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(input)))
		dec.UseNumber()
		if err := dec.Decode(&args); err != nil {
			return Outcome{}, err
		}
	}

	res, err := d.sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return classifyMessage(err.Error()), nil
	}
	if res.IsError {
		return Outcome{Class: ClassToolError, Message: textOf(res.Content)}, nil
	}
	if blob := blobOf(res.Content); blob != nil {
		return Outcome{Class: ClassOK, Output: string(blob)}, nil
	}

	// The text block in preference to StructuredContent, for a reason worth
	// stating: the SDK decodes structuredContent into an `any`, and Go's JSON
	// decoder turns every number in an `any` into a float64. A value past 2^53
	// is therefore already rounded by the time a Go client can look at it,
	// whatever the server sent.
	//
	// forge's wire is correct -- the raw JSON-RPC frame carries the exact
	// digits in both structuredContent and the text copy -- so reading the
	// rounded one here would be measuring the SDK's typing rather than forge's
	// output. The text copy exists for clients that ignore structuredContent,
	// and it happens to be the lossless of the two in Go.
	if text := textOf(res.Content); text != "" {
		return Outcome{Class: ClassOK, Output: canonJSON([]byte(text))}, nil
	}
	if res.StructuredContent != nil {
		raw, merr := json.Marshal(res.StructuredContent)
		if merr != nil {
			return Outcome{}, merr
		}
		return Outcome{Class: ClassOK, Output: canonJSON(raw)}, nil
	}
	return Outcome{Class: ClassOK}, nil
}

func textOf(content []mcp.Content) string {
	for _, c := range content {
		if t, ok := c.(*mcp.TextContent); ok {
			return t.Text
		}
	}
	return ""
}

// blobOf pulls binary out of the blocks MCP uses for it.
//
// MCP has no generic binary content type, so forge sends an embedded resource
// for anything that is not image or audio. That framing is the protocol's, not
// a divergence: what has to match across surfaces is the bytes, not how they
// are wrapped.
func blobOf(content []mcp.Content) []byte {
	for _, c := range content {
		switch v := c.(type) {
		case *mcp.EmbeddedResource:
			if v.Resource != nil && len(v.Resource.Blob) > 0 {
				return v.Resource.Blob
			}
		case *mcp.ImageContent:
			return v.Data
		case *mcp.AudioContent:
			return v.Data
		}
	}
	return nil
}

// ---------------------------------------------------------------- REST

// RESTDriver drives a real HTTP server.
type RESTDriver struct {
	srv *httptest.Server
	set Setup
}

// NewRESTDriver starts a server for this setup.
func NewRESTDriver(s Setup) (*RESTDriver, func()) {
	srv := httptest.NewServer(rest.New(rest.Options{
		Toolkit: s.Toolkit, Views: s.Views, Default: s.Selector,
	}).Handler())
	return &RESTDriver{srv: srv, set: s}, srv.Close
}

func (d *RESTDriver) Name() string { return "rest" }

func (d *RESTDriver) List(ctx context.Context) ([]string, error) {
	body, _, err := d.do(ctx, http.MethodGet, "/v1/tools", nil)
	if err != nil {
		return nil, err
	}
	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	var names []string
	for _, t := range list.Tools {
		names = append(names, t.Name)
	}
	return sortedCopy(names), nil
}

func (d *RESTDriver) Describe(ctx context.Context, name string) ([]byte, error) {
	body, code, err := d.do(ctx, http.MethodGet, "/v1/tools/"+name, nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("rest: describe %s: %d", name, code)
	}
	var out struct {
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out.InputSchema, nil
}

func (d *RESTDriver) Invoke(ctx context.Context, name string, input json.RawMessage) (Outcome, error) {
	body, code, err := d.do(ctx, http.MethodPost, "/v1/tools/"+name+"/invoke", input)
	if err != nil {
		return Outcome{}, err
	}
	switch code {
	case http.StatusOK:
		return Outcome{Class: ClassOK, Output: canonJSON(body)}, nil
	case http.StatusUnprocessableEntity:
		return Outcome{Class: ClassToolError, Message: problemDetail(body)}, nil
	case http.StatusBadRequest:
		return Outcome{Class: ClassInvalidInput}, nil
	case http.StatusNotFound:
		return Outcome{Class: ClassNotFound}, nil
	default:
		return Outcome{Class: ClassFailed, Message: problemDetail(body)}, nil
	}
}

func problemDetail(body []byte) string {
	var p struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return string(body)
	}
	return p.Detail
}

func (d *RESTDriver) do(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	var reader *strings.Reader
	if body == nil {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, d.srv.URL+path, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := d.srv.Client().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = res.Body.Close() }()
	out, err := readAll(res)
	return out, res.StatusCode, err
}

// ---------------------------------------------------------------- gRPC

// GRPCDriver drives a real client over bufconn.
type GRPCDriver struct {
	client forgev1.ToolServiceClient
	set    Setup
}

// NewGRPCDriver starts a server and dials it.
func NewGRPCDriver(s Setup) (*GRPCDriver, func(), error) {
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	forgev1.RegisterToolServiceServer(srv, grpcsvc.New(grpcsvc.Options{
		Toolkit: s.Toolkit, Views: s.Views, Default: s.Selector,
	}))
	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		srv.Stop()
		return nil, nil, err
	}
	return &GRPCDriver{client: forgev1.NewToolServiceClient(conn), set: s},
		func() { _ = conn.Close(); srv.Stop() }, nil
}

func (d *GRPCDriver) Name() string { return "grpc" }

func (d *GRPCDriver) List(ctx context.Context) ([]string, error) {
	res, err := d.client.ListTools(ctx, &forgev1.ListToolsRequest{})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, t := range res.GetTools() {
		names = append(names, t.GetName())
	}
	return sortedCopy(names), nil
}

func (d *GRPCDriver) Describe(ctx context.Context, name string) ([]byte, error) {
	res, err := d.client.DescribeTool(ctx, &forgev1.DescribeToolRequest{Name: name})
	if err != nil {
		return nil, err
	}
	return []byte(res.GetTool().GetInputSchemaJson()), nil
}

func (d *GRPCDriver) Invoke(ctx context.Context, name string, input json.RawMessage) (Outcome, error) {
	// The json field, not struct_value: struct_value is documented as lossy
	// beyond 2^53, and a harness that used it would be asserting agreement on
	// a value forge says will differ.
	res, err := d.client.Invoke(ctx, &forgev1.InvokeRequest{
		Name:  name,
		Input: &forgev1.InvokeRequest_Json{Json: input},
	})
	if err != nil {
		switch status.Code(err) {
		case codes.NotFound:
			return Outcome{Class: ClassNotFound}, nil
		case codes.InvalidArgument, codes.PermissionDenied:
			return Outcome{Class: ClassInvalidInput}, nil
		default:
			return Outcome{Class: ClassFailed, Message: status.Convert(err).Message()}, nil
		}
	}
	if res.GetIsError() {
		return Outcome{Class: ClassToolError, Message: res.GetError()}, nil
	}
	switch {
	case res.GetText() != "":
		return Outcome{Class: ClassOK, Output: canonJSON([]byte(res.GetText()))}, nil
	case len(res.GetBinary()) > 0:
		return Outcome{Class: ClassOK, Output: string(res.GetBinary())}, nil
	default:
		return Outcome{Class: ClassOK, Output: canonJSON(res.GetJson())}, nil
	}
}

// ---------------------------------------------------------------- shared

// surfaceNames lists what a view exposes, from the store.
func surfaceNames(s Setup) ([]string, error) {
	records, err := s.Toolkit.List(s.Selector)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, rec := range records {
		for _, op := range rec.Spec.Ops {
			names = append(names, binding.SurfaceName(rec.Spec.Name, op.Name, len(rec.Spec.Ops) == 1))
		}
	}
	return sortedCopy(names), nil
}

func splitSurfaceName(s Setup, name string) (tool, op string, err error) {
	records, err := s.Toolkit.List(s.Selector)
	if err != nil {
		return "", "", err
	}
	for _, rec := range records {
		for _, o := range rec.Spec.Ops {
			if binding.SurfaceName(rec.Spec.Name, o.Name, len(rec.Spec.Ops) == 1) == name {
				return rec.Spec.Name, o.Name, nil
			}
		}
	}
	return "", "", errors.New("no such tool")
}

func canonicalSchema(s Setup, name string) ([]byte, error) {
	tool, op, err := splitSurfaceName(s, name)
	if err != nil {
		return nil, err
	}
	b, err := s.Toolkit.Bound(tool, op)
	if err != nil {
		return nil, err
	}
	return b.CanonJSON, nil
}

func classifyMessage(msg string) Outcome {
	switch {
	case strings.Contains(msg, "no tool named"), strings.Contains(msg, "unknown tool"):
		return Outcome{Class: ClassNotFound}
	case strings.Contains(msg, "required property"), strings.Contains(msg, "expected "),
		strings.Contains(msg, "problems with the input"), strings.Contains(msg, "not one of"),
		strings.Contains(msg, "capability"):
		return Outcome{Class: ClassInvalidInput}
	default:
		return Outcome{Class: ClassFailed, Message: msg}
	}
}
