// Package hostsvc implements the services behind forge's capability host
// functions: HTTP, key/value storage, secrets and tool-to-tool calls.
//
// They live outside hostabi so that the ABI plumbing has no opinions about
// policy, and so that a surface can supply a different implementation -- a
// test can hand in an HTTP service that answers from a table without going
// near a socket.
package hostsvc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/richardwooding/hostrate"
	"github.com/richardwooding/ssrfguard"
	"golang.org/x/time/rate"

	"github.com/richardwooding/forge/internal/capability"
	"github.com/richardwooding/forge/internal/wasmrt/hostabi"
)

// HTTPConfig configures the HTTP service.
type HTTPConfig struct {
	// MaxBodyBytes caps a response body.
	MaxBodyBytes int64
	// Timeout bounds one request.
	Timeout time.Duration
	// PerHostRate limits requests to any one host.
	PerHostRate rate.Limit
	// PerHostBurst is how many may go at once.
	PerHostBurst int
	// AllowPrivate permits loopback and private ranges, for a test or a tool
	// pointed at a service on this machine. It does not unblock link-local:
	// the cloud metadata endpoint stays refused whatever this says, because
	// someone enabling loopback for local development is not asking for that.
	AllowPrivate bool
	// Credentials resolves the secrets a request names in its Credential.
	// Without it, a request carrying one fails rather than going out bare.
	Credentials hostabi.CredentialSource
}

func (c HTTPConfig) withDefaults() HTTPConfig {
	if c.MaxBodyBytes == 0 {
		c.MaxBodyBytes = 16 << 20
	}
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	if c.PerHostRate == 0 {
		c.PerHostRate = 10
	}
	if c.PerHostBurst == 0 {
		c.PerHostBurst = 20
	}
	return c
}

// HTTP performs requests for tools that hold the net.http capability.
type HTTP struct {
	cfg    HTTPConfig
	guard  *ssrfguard.Guard
	client *http.Client
}

// NewHTTP builds the service.
//
// Two of the user's own libraries do the load-bearing work: ssrfguard resolves
// and screens addresses, and hostrate keeps one tool from hammering one host.
// Writing either again here would be a worse version of something already
// tested.
func NewHTTP(cfg HTTPConfig) *HTTP {
	cfg = cfg.withDefaults()

	guard := ssrfguard.New(
		ssrfguard.WithSchemes("https", "http"),
		ssrfguard.WithAllowPrivate(cfg.AllowPrivate),
	)

	// The dialler is the important part. Checking the URL's hostname and then
	// letting the transport resolve it again leaves a window in which DNS can
	// answer differently the second time -- the rebinding attack. Control runs
	// on the address the transport is about to connect to, so the address that
	// was checked is the address that is used.
	base := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
			Control:   control(guard),
		}).DialContext,
		// No proxy. An inherited HTTP_PROXY would send a tool's traffic
		// somewhere the address screening never looked at.
		Proxy:                 nil,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		DisableCompression:    false,
	}

	return &HTTP{
		cfg:   cfg,
		guard: guard,
		client: &http.Client{
			// WithIdleTimeout bounds the limiter map. A tool that walks a list of
			// a thousand hosts would otherwise leave a limiter behind for each
			// one for the life of the process.
			Transport: hostrate.New(base, cfg.PerHostRate, cfg.PerHostBurst,
				hostrate.WithKeyFunc(hostrate.KeyByHostPort),
				hostrate.WithIdleTimeout(10*time.Minute)),
			Timeout: cfg.Timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				// Redirects are not followed. A permitted host could otherwise
				// redirect a tool anywhere, and the grant the user gave named
				// that host rather than wherever it chooses to point. The
				// guest sees the 3xx and its Location, and may request it
				// itself -- which puts the new URL through the allowlist.
				return http.ErrUseLastResponse
			},
		},
	}
}

// control screens the address the transport is about to connect to.
//
// It refuses link-local before delegating, so 169.254.169.254 and its IPv6
// equivalent stay blocked even under AllowPrivate. That switch exists so a
// tool can reach a service on this machine; it is not a decision to expose
// cloud credentials, and one flag should not mean both.
func control(guard *ssrfguard.Guard) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("cannot read the address %q: %w", address, err)
		}
		ip, err := netip.ParseAddr(host)
		if err != nil {
			// Control is always handed a literal address; anything else means
			// resolution did not happen where it was expected to, and that is
			// not a condition to continue through.
			return fmt.Errorf("%q is not an IP address", host)
		}
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return fmt.Errorf("%w: %s is link-local, which is where cloud credentials live",
				ssrfguard.ErrBlockedAddress, ip)
		}
		return guard.Control(network, address, c)
	}
}

// headersRefused are headers a guest may not set.
//
// Authorization and Cookie because credentials travel as a Credential, where
// the host reads the secret, checks its binding and sets the header, rather
// than as a value the tool has seen and could send elsewhere; Host because it decides which virtual host is addressed
// and would make the allowlist check meaningless; the Proxy- family because
// they steer the request past everything above.
var headersRefused = []string{"authorization", "cookie", "host", "proxy-authorization", "proxy-connection"}

// Do implements hostabi.HTTPService.
func (h *HTTP) Do(ctx context.Context, tool string, grants capability.Set, req hostabi.HTTPRequest) (hostabi.HTTPResponse, error) {
	target, err := parseTarget(req.URL)
	if err != nil {
		return hostabi.HTTPResponse{}, err
	}

	// The grant names a host, so that is what is checked -- before the
	// address screening, because a host the user never allowed should be
	// refused whatever it resolves to.
	if d := grants.Allow(capability.NetHTTP, target.Hostname()); !d.OK {
		return hostabi.HTTPResponse{}, capability.Refuse(d)
	}
	if target.Scheme == "http" && !h.cfg.AllowPrivate {
		// Plain HTTP would send whatever the tool is carrying in clear, and a
		// grant for a host is not a decision to do that.
		return hostabi.HTTPResponse{}, capability.Refusef(capability.DenyFloor,
			"%s asked for %s over plain http; forge makes https requests only", tool, target.Hostname())
	}

	if err := h.guard.ValidateURLContext(ctx, req.URL); err != nil {
		return hostabi.HTTPResponse{}, capability.Refusef(capability.DenyFloor,
			"%s is not a permitted address: %v", req.URL, err)
	}

	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}

	var body io.Reader
	if len(req.Body) > 0 {
		body = strings.NewReader(string(req.Body))
	}
	hreq, err := http.NewRequestWithContext(ctx, method, req.URL, body)
	if err != nil {
		return hostabi.HTTPResponse{}, fmt.Errorf("cannot build the request: %w", err)
	}

	for k, v := range req.Headers {
		if refusedHeader(k) {
			return hostabi.HTTPResponse{}, capability.Refusef(capability.DenyFloor,
				"%s may not set the %s header; name a secret in the request's Credential instead", tool, k)
		}
		hreq.Header.Set(k, v)
	}
	var attached []string
	if req.Credential != nil {
		header, value, secret, err := h.credential(ctx, tool, grants, target.Hostname(), req)
		if err != nil {
			return hostabi.HTTPResponse{}, err
		}
		hreq.Header.Set(header, value)
		attached = []string{value, secret}
	}
	if hreq.Header.Get("User-Agent") == "" {
		// Say who is calling. A server operator seeing unexpected traffic
		// should be able to tell what it is.
		hreq.Header.Set("User-Agent", "forge-tool/"+tool)
	}

	res, err := h.client.Do(hreq)
	if err != nil {
		// A dial-time block is a refusal, not a malfunction. It reaches here
		// wrapped in *url.Error, and reporting it as "request failed" would
		// send a tool author looking for a network problem that is not there.
		if errors.Is(err, ssrfguard.ErrBlockedAddress) {
			return hostabi.HTTPResponse{}, capability.Refusef(capability.DenyFloor,
				"%s is not a permitted address: %v", req.URL, unwrapURLError(err))
		}
		return hostabi.HTTPResponse{}, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = res.Body.Close() }()

	limited := io.LimitReader(res.Body, h.cfg.MaxBodyBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return hostabi.HTTPResponse{}, fmt.Errorf("reading the response: %w", err)
	}
	truncated := int64(len(data)) > h.cfg.MaxBodyBytes
	if truncated {
		data = data[:h.cfg.MaxBodyBytes]
	}

	headers := flatten(res.Header)
	// Servers echo credentials back: Graph quotes a malformed token in its
	// error message. The tool was promised it never sees the value, so the
	// host scrubs it before the response crosses into the guest.
	for _, v := range attached {
		if len(v) < 6 {
			continue
		}
		data = bytes.ReplaceAll(data, []byte(v), []byte("[redacted]"))
		for k, hv := range headers {
			headers[k] = strings.ReplaceAll(hv, v, "[redacted]")
		}
	}

	return hostabi.HTTPResponse{
		Status:    res.StatusCode,
		Headers:   headers,
		Body:      data,
		Truncated: truncated,
	}, nil
}

// credential resolves the header a request's Credential asks for.
//
// The binding is checked against this request's host alone. Redirects are not
// followed, so a 3xx sends the guest back through here with the new URL, and
// that is what keeps a bound token from following a redirect off its host.
func (h *HTTP) credential(ctx context.Context, tool string, grants capability.Set, host string, req hostabi.HTTPRequest) (header, value, secret string, err error) {
	c := req.Credential
	if h.cfg.Credentials == nil {
		return "", "", "", errors.New("this forge has no secret source configured for credentials")
	}
	if d := grants.Allow(capability.Secret, c.Secret); !d.OK {
		return "", "", "", capability.Refuse(d)
	}

	header = strings.TrimSpace(c.Header)
	if header == "" {
		header = "Authorization"
	}
	lower := strings.ToLower(header)
	if lower == "host" || strings.HasPrefix(lower, "proxy-") {
		return "", "", "", capability.Refusef(capability.DenyFloor,
			"%s may not attach a credential as the %s header", tool, header)
	}
	for k := range req.Headers {
		if strings.EqualFold(k, header) {
			return "", "", "", fmt.Errorf("the request sets %s both as a header and as its credential", header)
		}
	}

	secret, err = h.cfg.Credentials.Credential(ctx, tool, c.Secret, host)
	if err != nil {
		return "", "", "", err
	}
	value, err = CredentialValue(header, c.Scheme, secret)
	if err != nil {
		return "", "", "", err
	}
	return header, value, secret, nil
}

// parseTarget reads a URL, refusing anything that is not an absolute http URL
// before any of it is used.
func parseTarget(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%q is not a URL: %w", raw, err)
	}
	if !u.IsAbs() || u.Host == "" {
		return nil, fmt.Errorf("%q is not an absolute URL", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%q is not http or https", u.Scheme)
	}
	return u, nil
}

// unwrapURLError strips the *url.Error wrapper, whose Error() repeats the
// method and URL the message already names.
func unwrapURLError(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return ue.Err
	}
	return err
}

func refusedHeader(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if strings.HasPrefix(lower, "proxy-") {
		return true
	}
	return slices.Contains(headersRefused, lower)
}

// flatten keeps the first value of each header. A tool that needs every
// Set-Cookie is not a tool forge is serving well anyway, and the alternative
// shape complicates the guest API for a case that has not come up.
func flatten(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}
