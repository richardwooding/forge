package tool

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// service names one of the host's capability-backed calls.
type service int

const (
	svcHTTP service = iota
	svcKV
	svcSecret
	svcInvoke
)

// errHostCall reports that the host call itself did not complete -- as opposed
// to completing and reporting a denial or a failure, which arrive as values in
// the envelope. Off wasm it is what every capability call returns.
var errHostCall = errors.New("forge: the host is not reachable from here")

// Envelope framing, mirroring internal/wasmrt/hostabi. The header is
// [version][status][reserved 2][little-endian u32 body length].
const (
	envelopeHeader  = 8
	envelopeVersion = 1

	statusOK     = 0
	statusDenied = 1
	statusError  = 2
)

// Deny codes, naming why a capability was refused. They matter because the
// three call for different things: a missing grant is fixed by granting it, an
// out-of-scope request by asking for something else, and a spent budget by
// doing less. A tool that cannot tell them apart will advise the wrong fix.
const (
	// DenyNoGrant means the tool holds no grant of this kind at all.
	DenyNoGrant = "no_grant"
	// DenyOutOfScope means it holds one, but not covering this subject.
	DenyOutOfScope = "out_of_scope"
	// DenyBudget means it was allowed but the allowance is spent.
	DenyBudget = "budget"
	// DenyFloor means forge refuses this outright. No grant will change it, so
	// a tool should not suggest one.
	DenyFloor = "floor"
)

// DeniedError reports that forge refused a capability the tool asked to use.
//
// It is a distinct type because a denial is usually recoverable: a tool that
// cannot reach the network may still be able to do something useful, and one
// that cannot read a secret can say which secret it wanted. Check for it with
// Denied rather than treating every failure as fatal.
type DeniedError struct {
	// Capability is the kind that was refused, such as "net.http".
	Capability string
	// Subject is what it was refused for: a host, a namespace, a secret name.
	Subject string
	// Reason is the host's explanation, suitable for showing a user.
	Reason string
	// Code is one of the Deny constants, or empty if forge did not say.
	Code string
}

// wireDenial mirrors the host's refusal envelope.
type wireDenial struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

func (e *DeniedError) Error() string {
	if e.Subject == "" {
		return fmt.Sprintf("forge denied %s: %s", e.Capability, e.Reason)
	}
	return fmt.Sprintf("forge denied %s for %q: %s", e.Capability, e.Subject, e.Reason)
}

// Denied reports whether err is a capability denial, and returns it.
func Denied(err error) (*DeniedError, bool) {
	var d *DeniedError
	if errors.As(err, &d) {
		return d, true
	}
	return nil, false
}

// call performs one capability call: encode the request, hand it to the host,
// decode the envelope, and either unmarshal the result or turn the status into
// the right kind of error.
func call(svc service, capName, subject string, req, out any) error {
	raw, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("forge: cannot encode the %s request: %w", capName, err)
	}

	env, err := hostCall(svc, raw)
	if err != nil {
		return err
	}
	status, body, err := decodeEnvelope(env)
	if err != nil {
		return err
	}

	switch status {
	case statusOK:
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("forge: cannot read the %s result: %w", capName, err)
		}
		return nil
	case statusDenied:
		d := &DeniedError{Capability: capName, Subject: subject, Reason: string(body)}
		// Older forge hosts sent the reason as bare text. Decoding what is
		// there and keeping the raw bytes otherwise means a tool built against
		// this SDK still reports something useful on one of them.
		var wire wireDenial
		if err := json.Unmarshal(body, &wire); err == nil && wire.Detail != "" {
			d.Code, d.Reason = wire.Code, wire.Detail
		}
		return d
	default:
		return fmt.Errorf("forge: %s failed: %s", capName, body)
	}
}

func decodeEnvelope(env []byte) (status byte, body []byte, err error) {
	if len(env) < envelopeHeader {
		return 0, nil, errHostCall
	}
	if env[0] != envelopeVersion {
		return 0, nil, fmt.Errorf("forge: this tool speaks host ABI v%d but forge sent v%d; rebuild the tool",
			envelopeVersion, env[0])
	}
	n := binary.LittleEndian.Uint32(env[4:8])
	if int(n) > len(env)-envelopeHeader {
		return 0, nil, errHostCall
	}
	return env[1], env[envelopeHeader : envelopeHeader+int(n)], nil
}

// --- HTTP ---------------------------------------------------------------

// HTTPRequest is a request for the host to make on the tool's behalf.
type HTTPRequest struct {
	// Method defaults to GET.
	Method string `json:"method,omitempty"`
	// URL must be absolute and https, unless plain http was granted.
	URL string `json:"url"`
	// Headers are set on the request. forge refuses Authorization, Cookie,
	// Host and the Proxy- family: credentials belong in a secret, where they
	// are named and audited.
	Headers map[string]string `json:"headers,omitempty"`
	// Body is sent as-is.
	Body []byte `json:"body,omitempty"`
}

// HTTPResponse is what comes back.
type HTTPResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`

	// Truncated reports that the body hit forge's size limit and was cut, so a
	// tool does not parse half a document as though it were whole.
	Truncated bool `json:"truncated,omitempty"`
}

// OK reports a 2xx status.
func (r HTTPResponse) OK() bool { return r.Status >= 200 && r.Status < 300 }

// HTTP makes an HTTP request through the host.
//
// The tool must declare the net.http capability for the host in the URL. forge
// does not follow redirects: a 3xx comes back with its Location header, and
// requesting that URL puts it through the allowlist in its own right.
func HTTP(req HTTPRequest) (HTTPResponse, error) {
	if req.URL == "" {
		return HTTPResponse{}, errors.New("forge: the request has no URL")
	}
	var res HTTPResponse
	if err := call(svcHTTP, "net.http", hostOf(req.URL), req, &res); err != nil {
		return HTTPResponse{}, err
	}
	return res, nil
}

// Get is HTTP for the common case.
func Get(url string) (HTTPResponse, error) {
	return HTTP(HTTPRequest{Method: "GET", URL: url})
}

// GetJSON fetches a URL and decodes the body into out. A non-2xx status is an
// error, because a tool that decodes an error page as though it were the
// document is the bug this exists to prevent.
func GetJSON(url string, out any) error {
	res, err := Get(url)
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("forge: %s returned %d", url, res.Status)
	}
	if res.Truncated {
		return fmt.Errorf("forge: the response from %s was truncated at forge's size limit", url)
	}
	if err := json.Unmarshal(res.Body, out); err != nil {
		return fmt.Errorf("forge: %s did not return usable JSON: %w", url, err)
	}
	return nil
}

// hostOf pulls the host out of a URL for error messages, without importing
// net/url -- which drags a surprising amount into a guest binary for the sake
// of a string in an error.
func hostOf(raw string) string {
	s := raw
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// --- Key/value ----------------------------------------------------------

type kvRequest struct {
	Op        string `json:"op"`
	Namespace string `json:"namespace,omitempty"`
	Key       string `json:"key,omitempty"`
	Value     []byte `json:"value,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
}

type kvResponse struct {
	Found bool     `json:"found,omitempty"`
	Value []byte   `json:"value,omitempty"`
	Keys  []string `json:"keys,omitempty"`
}

// Store is a key/value namespace belonging to this tool. Values persist
// between invocations and between surfaces; a tool's namespaces are its own.
type Store struct{ ns string }

// OpenKV opens a namespace. An empty name means "default". The tool must declare
// the kv capability for that namespace.
func OpenKV(namespace string) *Store { return &Store{ns: namespace} }

// Get reads a key. The second result reports whether it was there, so an empty
// value and a missing key are distinguishable.
func (s *Store) Get(key string) ([]byte, bool, error) {
	var res kvResponse
	err := call(svcKV, "kv", s.ns, kvRequest{Op: "get", Namespace: s.ns, Key: key}, &res)
	if err != nil {
		return nil, false, err
	}
	return res.Value, res.Found, nil
}

// GetString is Get for text values.
func (s *Store) GetString(key string) (string, bool, error) {
	v, found, err := s.Get(key)
	return string(v), found, err
}

// Set writes a key.
func (s *Store) Set(key string, value []byte) error {
	return call(svcKV, "kv", s.ns, kvRequest{Op: "set", Namespace: s.ns, Key: key, Value: value}, nil)
}

// SetString is Set for text values.
func (s *Store) SetString(key, value string) error { return s.Set(key, []byte(value)) }

// GetJSON reads a key and decodes it. A missing key leaves out untouched.
func (s *Store) GetJSON(key string, out any) (bool, error) {
	v, found, err := s.Get(key)
	if err != nil || !found {
		return found, err
	}
	if err := json.Unmarshal(v, out); err != nil {
		return true, fmt.Errorf("forge: the value at %q is not usable JSON: %w", key, err)
	}
	return true, nil
}

// SetJSON encodes a value and writes it.
func (s *Store) SetJSON(key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("forge: cannot encode the value for %q: %w", key, err)
	}
	return s.Set(key, raw)
}

// Delete removes a key. Removing one that is not there is not an error.
func (s *Store) Delete(key string) error {
	return call(svcKV, "kv", s.ns, kvRequest{Op: "delete", Namespace: s.ns, Key: key}, nil)
}

// List returns the keys with the given prefix, in sorted order.
func (s *Store) List(prefix string) ([]string, error) {
	var res kvResponse
	err := call(svcKV, "kv", s.ns, kvRequest{Op: "list", Namespace: s.ns, Prefix: prefix}, &res)
	if err != nil {
		return nil, err
	}
	return res.Keys, nil
}

// --- Secrets ------------------------------------------------------------

type secretRequest struct {
	Name string `json:"name"`
}

type secretResponse struct {
	Found bool   `json:"found"`
	Value string `json:"value,omitempty"`
}

// GetSecret reads a named secret. The second result reports whether it exists.
//
// The tool must declare the secret capability for that name. Note that forge
// shows a tool asking for both secret and net.http prominently at approval
// time: it can send anything it reads anywhere it is allowed to reach.
func GetSecret(name string) (string, bool, error) {
	var res secretResponse
	if err := call(svcSecret, "secret", name, secretRequest{Name: name}, &res); err != nil {
		return "", false, err
	}
	return res.Value, res.Found, nil
}

// MustGetSecret reads a secret that the tool cannot work without, turning a
// missing one into an error that names it.
func MustGetSecret(name string) (string, error) {
	v, found, err := GetSecret(name)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("forge: the secret %q is not set; add it with: forge secret set %s", name, name)
	}
	return v, nil
}

// --- Tool to tool -------------------------------------------------------

type invokeRequest struct {
	Tool  string          `json:"tool"`
	Op    string          `json:"op,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type invokeResponse struct {
	Output json.RawMessage `json:"output,omitempty"`
}

// Call invokes another forge tool and decodes its output into out.
//
// The callee runs with at most the capabilities this tool holds, shares its
// deadline and its budget, and cannot call back into anything already on the
// call path. Pass nil for out to ignore the result.
func Call(toolName, op string, input any, out any) error {
	var raw json.RawMessage
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("forge: cannot encode the input for %s: %w", toolName, err)
		}
		raw = encoded
	}

	var res invokeResponse
	err := call(svcInvoke, "tool.invoke", toolName,
		invokeRequest{Tool: toolName, Op: op, Input: raw}, &res)
	if err != nil {
		return err
	}
	if out == nil || len(res.Output) == 0 {
		return nil
	}
	if err := json.Unmarshal(res.Output, out); err != nil {
		return fmt.Errorf("forge: cannot read the result from %s: %w", toolName, err)
	}
	return nil
}

// base64 is referenced so the guest's JSON encoding of []byte fields is the
// standard one the host expects; keeping the import explicit documents that
// HTTPRequest.Body and kv values travel base64-encoded in the envelope.
var _ = base64.StdEncoding
