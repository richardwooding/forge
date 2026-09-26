// Command fetch is a forge tool for retrieving a URL.
//
// It is the counterpart to jsonfmt: where that one declares nothing and can
// reach nothing, this one declares net.http and so has to ask. That is the
// point of shipping it -- it is the smallest honest demonstration of what a
// granted capability looks like from both sides, and it is useful in its own
// right on every surface at once.
//
// The request itself is made by forge, not here. A wasm guest has no sockets
// at all, so there is nothing to bypass: the host resolves the name, checks
// every address it resolves to, dials the one it checked, and refuses to
// follow a redirect somewhere the grant never covered.
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/richardwooding/forge/sdk/tool"
)

type GetArgs struct {
	URL     string            `json:"url" jsonschema:"the URL to fetch; https unless plain http was granted"`
	Headers map[string]string `json:"headers,omitempty" jsonschema:"extra request headers"`
}

type GetOut struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body"`

	// Truncated reports that forge cut the body at its size limit, so nothing
	// downstream parses half a document as though it were whole.
	Truncated bool `json:"truncated,omitempty"`
}

type JSONArgs struct {
	URL string `json:"url" jsonschema:"the URL to fetch and parse as JSON"`
}

type JSONOut struct {
	Status int             `json:"status"`
	Data   json.RawMessage `json:"data"`
}

type HeadArgs struct {
	URL string `json:"url" jsonschema:"the URL to inspect"`
}

type HeadOut struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
}

var _ = tool.Register(
	tool.Spec{
		Name:    "fetch",
		Version: "0.1.0",
		Summary: "Retrieve a URL over HTTP",
		Description: "Fetches a URL through forge, which screens the address, refuses " +
			"internal ranges such as the cloud metadata endpoint, and does not follow " +
			"redirects. Redirects come back as a 3xx with their Location header so you " +
			"can decide whether to follow them.",
		Labels: []string{"net", "http", "web"},
		Needs: []tool.Need{{
			Kind:  tool.NetHTTP,
			Scope: []string{"*"},
			Reason: "fetch the URLs you ask it for. Narrow this to the hosts you " +
				"actually want it to reach if you would rather not grant all of them.",
		}},
	},
	// Every operation is read-only and open-world: it changes nothing here and
	// reaches something forge does not control. Those are the tool's own
	// claims -- they tell a person what to expect and never excuse an
	// approval.
	tool.Op("get", get,
		tool.Summary("Fetch a URL and return the body as text"),
		tool.ReadOnly(), tool.OpenWorld()),
	tool.Op("json", fetchJSON,
		tool.Summary("Fetch a URL and return the parsed JSON"),
		tool.ReadOnly(), tool.OpenWorld()),
	tool.Op("head", head,
		tool.Summary("Fetch a URL and return only the status and headers"),
		tool.ReadOnly(), tool.Idempotent(), tool.OpenWorld()),
)

func main() {}

func get(ctx *tool.Context, a GetArgs) (GetOut, error) {
	res, err := tool.HTTP(tool.HTTPRequest{Method: "GET", URL: a.URL, Headers: a.Headers})
	if err != nil {
		return GetOut{}, explain(err, a.URL)
	}
	return GetOut{
		Status:    res.Status,
		Headers:   res.Headers,
		Body:      string(res.Body),
		Truncated: res.Truncated,
	}, nil
}

func fetchJSON(ctx *tool.Context, a JSONArgs) (JSONOut, error) {
	res, err := tool.Get(a.URL)
	if err != nil {
		return JSONOut{}, explain(err, a.URL)
	}
	if res.Truncated {
		return JSONOut{}, fmt.Errorf("the response was cut at forge's size limit, so it is not valid JSON")
	}
	body := strings.TrimSpace(string(res.Body))
	if !json.Valid([]byte(body)) {
		return JSONOut{}, fmt.Errorf("%s returned %d but the body is not JSON", a.URL, res.Status)
	}
	return JSONOut{Status: res.Status, Data: json.RawMessage(body)}, nil
}

func head(ctx *tool.Context, a HeadArgs) (HeadOut, error) {
	res, err := tool.HTTP(tool.HTTPRequest{Method: "HEAD", URL: a.URL})
	if err != nil {
		return HeadOut{}, explain(err, a.URL)
	}
	return HeadOut{Status: res.Status, Headers: res.Headers}, nil
}

// explain turns a refusal into advice.
//
// It follows the deny code rather than assuming every refusal is a missing
// grant. Telling someone to widen a grant when forge actually refused the
// address -- which no grant will ever permit -- sends them to change a setting
// that was never the problem.
func explain(err error, url string) error {
	d, ok := tool.Denied(err)
	if !ok {
		return err
	}
	switch d.Code {
	case tool.DenyNoGrant, tool.DenyOutOfScope:
		if d.Subject != "" {
			return fmt.Errorf("%s\n\nIf you do want fetch to reach %s:\n"+
				"    forge grant revoke fetch   # then run again and say yes",
				d.Reason, d.Subject)
		}
	case tool.DenyFloor:
		// Nothing to suggest: forge does not allow this at all.
		return fmt.Errorf("%s", d.Reason)
	case tool.DenyBudget:
		return fmt.Errorf("%s\n\nThis is forge's per-request allowance, not a "+
			"permission; split the work across runs.", d.Reason)
	}
	return fmt.Errorf("%s", d.Reason)
}
