package model

import (
	"net"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// planFromBytes builds a small workflow whose names, dependencies, inputs and
// capabilities come from fuzzer bytes. Names use a tiny alphabet so duplicates,
// unknown references and cycles are common.
func planFromBytes(data []byte, maxParallel int) WorkflowPlan {
	next := func() byte {
		if len(data) == 0 {
			return 0
		}
		b := data[0]
		data = data[1:]
		return b
	}
	names := []string{"", "a", "b", "c", "d", "e", "f", "g"}
	plan := WorkflowPlan{MaxParallel: maxParallel}
	for count := int(next()) % 12; len(plan.Steps) < count; {
		header := next()
		step := WorkflowStep{Name: names[header%8], Target: "node", Task: TaskSpec{Capability: "exec.run"}}
		if header&0x80 != 0 && next()%16 == 0 {
			step.Task.Capability = "workflow.run"
		}
		for range int(next() % 4) {
			step.Needs = append(step.Needs, names[next()%8])
		}
		for range int(next() % 3) {
			b := next()
			step.Inputs = append(step.Inputs, struct {
				Step     string `json:"step"`
				Artifact int    `json:"artifact"`
				Path     string `json:"path"`
			}{Step: names[b%8], Artifact: int(int8(next()))})
		}
		plan.Steps = append(plan.Steps, step)
	}
	return plan
}

// orderable decides independently whether a plan is acceptable: bounded,
// uniquely and nonemptily named, without nested workflows, unknown
// dependencies, negative artifact indexes or cycles (by depth-first search).
func orderable(plan WorkflowPlan) bool {
	if len(plan.Steps) == 0 || len(plan.Steps) > 100 || plan.MaxParallel < 0 || plan.MaxParallel > MaxWorkflowParallel {
		return false
	}
	edges := map[string][]string{}
	for _, step := range plan.Steps {
		if _, duplicate := edges[step.Name]; step.Name == "" || duplicate {
			return false
		}
		edges[step.Name] = nil
	}
	for _, step := range plan.Steps {
		if step.Task.Capability == "workflow.run" {
			return false
		}
		edges[step.Name] = append(edges[step.Name], step.Needs...)
		for _, input := range step.Inputs {
			if input.Artifact < 0 {
				return false
			}
			edges[step.Name] = append(edges[step.Name], input.Step)
		}
		for _, dependency := range edges[step.Name] {
			if _, known := edges[dependency]; !known {
				return false
			}
		}
	}
	state := map[string]int{}
	var acyclic func(string) bool
	acyclic = func(name string) bool {
		switch state[name] {
		case 1:
			return false
		case 2:
			return true
		}
		state[name] = 1
		for _, dependency := range edges[name] {
			if !acyclic(dependency) {
				return false
			}
		}
		state[name] = 2
		return true
	}
	for name := range edges {
		if !acyclic(name) {
			return false
		}
	}
	return true
}

// FuzzWorkflowPlanOrdered compares Ordered with an independent validator and
// checks that an accepted order is a permutation of the steps in which every
// dependency and artifact input precedes its consumer.
func FuzzWorkflowPlanOrdered(f *testing.F) {
	for _, seed := range []struct {
		data        []byte
		maxParallel int
	}{
		{[]byte{1, 1, 0, 0}, 0},
		{[]byte{3, 1, 1, 2, 0, 2, 1, 3, 0, 3, 0, 0}, 4},
		{[]byte{2, 1, 1, 2, 0, 2, 1, 1, 0}, 1},
		{[]byte{2, 1, 0, 0, 1, 0}, 1},
		{[]byte{1, 0x81, 0, 0, 0}, 1},
		{[]byte{2, 1, 0, 1, 2, 0xff, 2, 0, 0}, 1},
		{[]byte{1, 1, 0, 0}, -1},
		{[]byte{1, 1, 0, 0}, MaxWorkflowParallel + 1},
		{[]byte{0}, 0},
	} {
		f.Add(seed.data, seed.maxParallel)
	}
	f.Fuzz(func(t *testing.T, data []byte, maxParallel int) {
		plan := planFromBytes(data, maxParallel)
		ordered, err := plan.Ordered()
		if want := orderable(plan); (err == nil) != want {
			t.Fatalf("Ordered error %v, independent validator accepts %v for %+v", err, want, plan)
		}
		if err != nil {
			return
		}
		if len(ordered) != len(plan.Steps) {
			t.Fatalf("ordered %d of %d steps", len(ordered), len(plan.Steps))
		}
		position := map[string]int{}
		for i, step := range ordered {
			if _, repeated := position[step.Name]; repeated {
				t.Fatalf("step %q ordered twice", step.Name)
			}
			position[step.Name] = i
		}
		for _, step := range plan.Steps {
			at, ok := position[step.Name]
			if !ok {
				t.Fatalf("step %q missing from order", step.Name)
			}
			for _, need := range step.Needs {
				if position[need] >= at {
					t.Fatalf("%q runs before its dependency %q", step.Name, need)
				}
			}
			for _, input := range step.Inputs {
				if position[input.Step] >= at {
					t.Fatalf("%q runs before its input source %q", step.Name, input.Step)
				}
			}
		}
	})
}

// FuzzNormalizeProxyURL checks that a normalized proxy URL is a fixed point
// and passes the same validation the gateway and nodes apply to persisted
// and advertised endpoints, so normalizing configuration never produces a
// value its own validators reject.
func FuzzNormalizeProxyURL(f *testing.F) {
	for _, seed := range []string{
		"http://proxy.example:3128", " http://PROXY.Example.:8080/ ", "http://10.0.0.1:3128", "http://[::1]:3128",
		"http://[::ffff:10.0.0.1]:3128", "http://[fe80::1%25en0]:3128", "http://0.0.0.0:3128", "http://224.0.0.1:80",
		"https://proxy:443", "http://user:pass@proxy:80", "http://proxy:0", "http://proxy:65536", "http://proxy:080",
		"http://proxy", "http://proxy:80/path", "http://proxy:80?x", "http://proxy:80#f", "http://proxy:80?",
		"http://a..:80", "http://.:80", "http://0.0.0.0.:1", "http://224.0.0.1.:80", "http://10.0.0.1.:3128", "http://%41:80", "http://a%2Fb:80", "http:proxy:80", "http://:80",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		normalized, err := NormalizeProxyURL(value)
		if err != nil {
			return
		}
		again, err := NormalizeProxyURL(normalized)
		if err != nil || again != normalized {
			t.Fatalf("%q normalized to %q, which renormalizes to %q: %v", value, normalized, again, err)
		}
		if err := ValidateGatewayProxy(&GatewayProxy{URL: normalized}); err != nil {
			t.Fatalf("normalized %q fails gateway proxy validation: %v", normalized, err)
		}
		if err := ValidateProxyEndpoints([]ProxyEndpoint{{URL: normalized}}); err != nil {
			t.Fatalf("normalized %q fails endpoint validation: %v", normalized, err)
		}
		u, err := url.Parse(normalized)
		if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			t.Fatalf("normalized %q has extra URL parts: %v", normalized, err)
		}
		host, port, err := net.SplitHostPort(u.Host)
		number, portErr := strconv.Atoi(port)
		if err != nil || host == "" || strings.HasSuffix(host, ".") || portErr != nil || number < 1 || number > 65535 {
			t.Fatalf("normalized %q has an invalid host or port", normalized)
		}
	})
}
