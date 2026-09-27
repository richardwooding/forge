// Command watch answers "what has appeared at this URL since I last looked".
//
// It fetches a JSON endpoint, pulls an identifier out of each item, asks the
// `since` tool which of those it has not seen, and returns the full items that
// are new. One call replaces fetch, extract, compare and remember.
//
// It does not do the remembering itself. `since` owns that, and calling it
// through tool.invoke rather than reimplementing it means one memory shared by
// everything that wants one -- and it means this tool can be granted the
// network without also being trusted to keep state.
//
// A consequence of forge's attenuation worth knowing: a callee runs with the
// intersection of its own grants and its caller's, so this tool has to hold
// every capability `since` needs, for `since` to keep working when called from
// here. Holding kv(since) grants nothing of `since`'s data: a kv namespace
// belongs to the tool doing the asking, so this tool's "since" namespace is
// its own and stays empty.
//
// clock.wall is on that list for a reason worth dwelling on. A missing grant
// usually fails loudly, but an ungranted clock does not: forge hands the tool
// a clock frozen at the start of 2022 and everything carries on. Without it
// here, `since` worked perfectly and stamped every record with 2022-01-01,
// which is the kind of fault that is only ever found by reading the output.
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/richardwooding/forge/sdk/tool"
)

type Args struct {
	URL    string `json:"url" jsonschema:"a URL returning JSON"`
	Stream string `json:"stream" jsonschema:"a name to remember this source under; reuse it to see only what is new"`
	ID     string `json:"id" jsonschema:"the field in each item that identifies it, e.g. 'id', 'tag_name', 'sha'"`
	Path   string `json:"path,omitempty" jsonschema:"dotted path to the array of items; omit when the response is itself an array"`
	Fields []string `json:"fields,omitempty" jsonschema:"return only these fields of each item, to keep the answer small"`
	Peek   bool   `json:"peek,omitempty" jsonschema:"report what is new without remembering it"`
}

type Out struct {
	New   []map[string]any `json:"new" jsonschema:"the items not seen before"`
	Count int              `json:"count"`
	// Checked is how many items the response held, so a caller can tell "the
	// feed is quiet" from "the feed is empty" -- which look identical if you
	// only report what is new.
	Checked  int  `json:"checked"`
	Recorded bool `json:"recorded"`
	First    bool `json:"first,omitempty" jsonschema:"true when this stream had never been seen, so everything was new"`
}

var _ = tool.Register(
	tool.Spec{
		Name:    "watch",
		Version: "0.1.0",
		Summary: "Fetch a JSON endpoint and report only what has appeared since last time",
		Description: "Fetches a URL, identifies each item by one of its fields, and returns " +
			"only the items it has not seen before. Remembering is delegated to the `since` " +
			"tool, so several watchers share one memory.",
		Labels: []string{"net", "http", "watch", "state"},
		Needs: []tool.Need{
			{
				Kind:  tool.NetHTTP,
				Scope: []string{"*"},
				Reason: "fetch the endpoints you point it at. Narrow this to the hosts you " +
					"actually poll if you would rather not grant all of them",
			},
			{
				Kind:   tool.Invoke,
				Scope:  []string{"since"},
				Reason: "ask `since` which items are new, rather than keeping a second copy of that memory",
			},
			{
				Kind:  tool.ClockWall,
				Scope: []string{"*"},
				Reason: "let `since` record a real date against what it remembers. Without " +
					"it forge hands a tool a clock frozen in 2022 -- and unlike a missing " +
					"grant, that does not fail, it just writes the wrong date",
			},
			{
				Kind:  tool.KV,
				Scope: []string{"since"},
				Reason: "forge gives a called tool only what its caller also holds, so this is " +
					"needed for `since` to reach its own store. It grants no access to what " +
					"`since` has stored: a namespace belongs to the tool that opens it",
			},
		},
	},
	tool.Op("json", watch,
		tool.Summary("What is new at this JSON endpoint?"),
		tool.ReadOnly(), tool.OpenWorld()),
)

func main() {}

// sinceOut mirrors the shape `since` returns. Mirrored rather than shared
// because a tool is a separate module; the pairing is a contract between two
// tools, and a change to it should be caught by the test that runs them
// together rather than by the compiler.
type sinceOut struct {
	New   []string `json:"new"`
	First bool     `json:"first"`
}

func watch(ctx *tool.Context, a Args) (Out, error) {
	if strings.TrimSpace(a.URL) == "" {
		return Out{}, fmt.Errorf("a url is required")
	}
	if strings.TrimSpace(a.Stream) == "" {
		return Out{}, fmt.Errorf("a stream name is required; it is how `since` tells one source from another")
	}
	if strings.TrimSpace(a.ID) == "" {
		return Out{}, fmt.Errorf("an id field is required; without one there is no way to tell two items apart")
	}

	res, err := tool.Get(a.URL)
	if err != nil {
		return Out{}, explain(err)
	}
	if !res.OK() {
		return Out{}, fmt.Errorf("%s returned %d", a.URL, res.Status)
	}
	if res.Truncated {
		// Parsing half a document would silently drop items, and dropped items
		// are indistinguishable from "nothing new" -- the one answer this tool
		// must never give wrongly.
		return Out{}, fmt.Errorf("the response was cut at forge's size limit, so it cannot be read as JSON")
	}

	var body any
	if err := json.Unmarshal(res.Body, &body); err != nil {
		return Out{}, fmt.Errorf("%s did not return usable JSON: %w", a.URL, err)
	}

	items, err := itemsAt(body, a.Path)
	if err != nil {
		return Out{}, err
	}

	ids := make([]string, 0, len(items))
	byID := make(map[string]map[string]any, len(items))
	for i, raw := range items {
		obj, ok := raw.(map[string]any)
		if !ok {
			return Out{}, fmt.Errorf("item %d is not an object, so it has no %q to identify it by", i, a.ID)
		}
		id, err := identify(obj, a.ID, i)
		if err != nil {
			return Out{}, err
		}
		if _, seen := byID[id]; seen {
			continue
		}
		ids = append(ids, id)
		byID[id] = obj
	}

	var ask sinceOut
	err = tool.Call("since", "new", map[string]any{
		"stream": a.Stream,
		"items":  ids,
		"peek":   a.Peek,
	}, &ask)
	if err != nil {
		return Out{}, explain(err)
	}

	out := Out{
		New:      make([]map[string]any, 0, len(ask.New)),
		Checked:  len(items),
		Recorded: !a.Peek && len(ask.New) > 0,
		First:    ask.First,
	}
	for _, id := range ask.New {
		if obj, ok := byID[id]; ok {
			out.New = append(out.New, project(obj, a.Fields))
		}
	}
	out.Count = len(out.New)
	return out, nil
}

// itemsAt walks a dotted path to the array of items.
func itemsAt(body any, path string) ([]any, error) {
	node := body
	if p := strings.TrimSpace(path); p != "" {
		for _, step := range strings.Split(p, ".") {
			obj, ok := node.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("the path %q runs through %q, which is not an object", path, step)
			}
			next, ok := obj[step]
			if !ok {
				return nil, fmt.Errorf("the response has no %q at path %q", step, path)
			}
			node = next
		}
	}
	items, ok := node.([]any)
	if !ok {
		if path == "" {
			return nil, fmt.Errorf("the response is not an array; give a path to the array of items")
		}
		return nil, fmt.Errorf("%q is not an array", path)
	}
	return items, nil
}

// identify reads the id field, accepting a number as well as a string because
// plenty of APIs number their items.
func identify(obj map[string]any, field string, index int) (string, error) {
	v, ok := obj[field]
	if !ok {
		return "", fmt.Errorf("item %d has no %q field", index, field)
	}
	switch t := v.(type) {
	case string:
		if t == "" {
			return "", fmt.Errorf("item %d has an empty %q", index, field)
		}
		return t, nil
	case float64:
		// %v on a float64 would render 101 as 101 but 1e+06 as 1e+06, and an
		// id whose spelling depends on its magnitude would be remembered twice.
		return fmt.Sprintf("%.0f", t), nil
	case bool:
		return fmt.Sprintf("%t", t), nil
	default:
		return "", fmt.Errorf("item %d has a %q that is not a string or a number", index, field)
	}
}

// project narrows an item to the requested fields. An agent asking what is new
// rarely wants all fifty fields of a release object, and the ones it does not
// want cost it context.
func project(obj map[string]any, fields []string) map[string]any {
	if len(fields) == 0 {
		return obj
	}
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		if v, ok := obj[f]; ok {
			out[f] = v
		}
	}
	return out
}

// explain turns a refusal into something actionable, following the deny code
// rather than assuming every refusal is a missing grant.
func explain(err error) error {
	d, ok := tool.Denied(err)
	if !ok {
		return err
	}
	switch d.Code {
	case tool.DenyNoGrant, tool.DenyOutOfScope:
		if d.Capability == "tool.invoke" || d.Capability == "kv" {
			return fmt.Errorf("%s\n\nwatch delegates its memory to `since`, which needs both "+
				"tool.invoke(since) and kv(since) here. Check `forge grant ls`, or run "+
				"`forge grant revoke watch` and answer yes when asked", d.Reason)
		}
		return fmt.Errorf("%s\n\nIf you do want watch to reach %s:\n"+
			"    forge grant revoke watch   # then run again and say yes", d.Reason, d.Subject)
	case tool.DenyFloor:
		return fmt.Errorf("%s", d.Reason)
	}
	return fmt.Errorf("%s", d.Reason)
}
