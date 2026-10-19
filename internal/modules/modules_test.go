package modules

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	t.Parallel()
	has := func(id string) bool { return id != "unconfigured" }
	core := []string{"bridge", "metadata"}
	if err := Validate(nil, has, core); err != nil {
		t.Fatalf("an empty registry: %v", err)
	}
	if err := Validate([]Module{{ID: "contributions", RequiresBridge: "contribute"}, {ID: "a_2"}}, has, core); err != nil {
		t.Fatalf("a good registry: %v", err)
	}
	long := "a" + strings.Repeat("b", 32) // 33 characters
	cases := []struct {
		name string
		ms   []Module
		want string
	}{
		{"empty id", []Module{{ID: ""}}, `module ""`},
		{"upper case", []Module{{ID: "Contrib"}}, `module "Contrib"`},
		{"leading digit", []Module{{ID: "2fa"}}, `module "2fa"`},
		{"a dot", []Module{{ID: "a.b"}}, `module "a.b"`},
		{"a dash", []Module{{ID: "a-b"}}, `module "a-b"`},
		{"too long", []Module{{ID: long}}, long},
		{"twice", []Module{{ID: "x"}, {ID: "x"}}, "registered twice"},
		{"a core capability", []Module{{ID: "bridge"}}, "core capability"},
		{"no config row", []Module{{ID: "unconfigured"}}, "no config row"},
	}
	for _, c := range cases {
		err := Validate(c.ms, has, core)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Validate = %v, want an error naming %q", c.name, err, c.want)
		}
	}
	if err := Validate([]Module{{ID: "x"}}, nil, core); err == nil {
		t.Error("no config check: Validate passed")
	}
	// Every problem is reported, not only the first.
	err := Validate([]Module{{ID: "Bad"}, {ID: "unconfigured"}}, has, core)
	if err == nil || !strings.Contains(err.Error(), "Bad") || !strings.Contains(err.Error(), "unconfigured") {
		t.Errorf("two problems: %v", err)
	}
}

// The capability rule: on, and the Bridge requirement (if any) met.
func TestBridgeStateAndActive(t *testing.T) {
	t.Parallel()
	none := Module{ID: "x"}
	needs := Module{ID: "y", RequiresBridge: "contribute"}
	cases := []struct {
		m        Module
		online   bool
		features []string
		state    string
	}{
		{none, false, nil, ""},
		{needs, false, nil, BridgeOffline},
		{needs, true, []string{"bridge"}, BridgeNoFeature},
		{needs, true, []string{"bridge", "contribute"}, BridgeReady},
	}
	for _, c := range cases {
		st := c.m.BridgeState(c.online, c.features)
		if st != c.state {
			t.Errorf("%s online=%t %v: state %q, want %q", c.m.ID, c.online, c.features, st, c.state)
		}
		want := st == "" || st == BridgeReady
		if Active(true, st) != want || Active(false, st) {
			t.Errorf("%s state %q: Active wrong", c.m.ID, st)
		}
	}
}
