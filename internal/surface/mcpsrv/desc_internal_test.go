package mcpsrv

import (
	"strings"
	"testing"

	"github.com/richardwooding/forge/internal/core"
	"github.com/richardwooding/forge/internal/store"
)

func rec5() store.Record {
	ops := make([]core.OpSpec, 5)
	for i := range ops {
		ops[i] = core.OpSpec{Name: string(rune('a' + i)), Summary: "does thing " + string(rune('a'+i))}
	}
	return store.Record{Spec: core.Spec{
		Name: "weather", Summary: "Weather research",
		Description: strings.Repeat("long description. ", 30),
		UseWhen:     "you need a forecast rather than a guess",
		Labels:      []string{"geo", "weather"},
		Ops:         ops,
	}}
}

func TestDescriptionShape(t *testing.T) {
	r := rec5()
	d := description(r, r.Spec.Ops[0])

	if !strings.Contains(d, "does thing a") {
		t.Error("missing the op summary")
	}
	if !strings.Contains(d, "Weather research") {
		t.Error("the tool summary is dropped for a multi-op tool, as it was before")
	}
	if !strings.Contains(d, "Use when: you need a forecast") {
		t.Error("UseWhen is not reaching the model")
	}
	if !strings.Contains(d, "Labels: geo, weather") {
		t.Error("labels are not reaching the model")
	}
	if strings.Contains(d, "long description") {
		t.Error("the long description is still duplicated per op")
	}

	total := 0
	for _, op := range r.Spec.Ops {
		total += len(description(r, op))
	}
	t.Logf("five ops cost %d bytes of description", total)
	if total > 1500 {
		t.Errorf("five ops cost %d bytes; the duplication is back", total)
	}
}
