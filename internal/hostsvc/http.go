// Package hostsvc implements the services behind forge's capability host
// functions: HTTP, key/value storage, secrets and tool-to-tool calls.
//
// They live outside hostabi so that the ABI plumbing has no opinions about
// policy, and so that a surface can supply a different implementation -- a
// test can hand in an HTTP service that answers from a table without going
// near a socket.
package hostsvc

import (
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
// Authorization and Cookie because a tool should get credentials from the
// secret capability, where they are named and auditable, rather than smuggling
// them into a header; Host because it decides which virtual host is addressed
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
				"%s may not set the %s header; use the secret capability for credentials", tool, k)
		}
		hreq.Header.Set(k, v)
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

	return hostabi.HTTPResponse{
		Status:    res.StatusCode,
		Headers:   flatten(res.Header),
		Body:      data,
		Truncated: truncated,
	}, nil
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
