package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

// placementAPI is a local node API that answers discovery and records what
// reaches the coordinator.
type placementAPI struct {
	mu        sync.Mutex
	methods   []string
	selectAll int
	submitted []json.RawMessage
	checked   []json.RawMessage
	machines  []model.NodeSelection
	one       model.NodeSelection
	existing  *model.Task
	legacyAll bool
	broken    bool
}

func (f *placementAPI) client(t *testing.T) Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call model.APICall
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			t.Error(err)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.methods = append(f.methods, call.Method)
		response := model.Response{Result: model.JSON(map[string]any{})}
		switch call.Method {
		case "nodes.select":
			var mode struct{ All bool }
			_ = json.Unmarshal(call.Params, &mode)
			switch {
			case !mode.All || f.legacyAll:
				response.Result = model.JSON(f.one)
			case len(f.machines) == 0:
				response = model.Response{Error: "no online enabled node matches the selector", Code: "operation_failed", Outcome: "unknown"}
			default:
				f.selectAll++
				response.Result = model.JSON(f.machines)
			}
		case "tasks.get":
			if f.broken {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			response = model.Response{Error: "task not found", Code: "operation_failed", Outcome: "unknown"}
			if f.existing != nil {
				response = model.Response{Result: model.JSON(f.existing)}
			}
		case "capabilities.list":
			response.Result = model.JSON([]model.Capability{{Name: "workflow.run", InputSchema: model.JSON(map[string]any{"properties": map[string]any{"steps": map[string]any{"items": map[string]any{"properties": map[string]any{"when": model.WorkflowWhenSchema()}}}}})}})
		case model.TaskCheckMethod:
			f.checked = append(f.checked, call.Params)
			response.Result = model.JSON(model.TaskCheck{Advisory: true, OK: true})
		case "tasks.start":
			f.submitted = append(f.submitted, call.Params)
			var spec model.TaskSpec
			_ = json.Unmarshal(call.Params, &spec)
			response.Result = model.JSON(model.Task{ID: spec.ID, State: "queued", Spec: spec})
		case "workflow.run":
			f.submitted = append(f.submitted, call.Params)
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(server.Close)
	return Client{URL: server.URL}
}

func (f *placementAPI) calls() ([]string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.methods), len(f.submitted)
}

func machines(names ...string) []model.NodeSelection {
	var out []model.NodeSelection
	for _, name := range names {
		out = append(out, model.NodeSelection{Node: model.Node{ID: "id-" + name, Name: name}})
	}
	return out
}

func decodePlan(t *testing.T, raw json.RawMessage) []model.WorkflowStep {
	t.Helper()
	var plan model.WorkflowPlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	return plan.Steps
}

func workflowSpec(id, plan string) model.TaskSpec {
	return model.TaskSpec{ID: id, Capability: "workflow.run", Args: json.RawMessage(plan)}
}

func TestWorkflowSelectResolvesOneMachine(t *testing.T) {
	api := &placementAPI{one: model.NodeSelection{Node: model.Node{ID: "id-builder", Name: "builder"}}}
	c := api.client(t)
	spec := workflowSpec("parent", `{"steps":[{"name":"build","target":{"select":{"capability":"exec.run"}},"task":{"capability":"exec.run"}}]}`)
	if _, err := c.StartTask(t.Context(), "coordinator", spec, nil); err != nil {
		t.Fatal(err)
	}
	methods, _ := api.calls()
	if !slices.Equal(methods, []string{"tasks.get", "nodes.select", "tasks.start"}) {
		t.Fatal("methods", methods)
	}
	var sent model.TaskSpec
	_ = json.Unmarshal(api.submitted[0], &sent)
	steps := decodePlan(t, sent.Args)
	if len(steps) != 1 || steps[0].Target != "id-builder" || steps[0].Placement != nil || steps[0].Task.ID != identity.ID([]byte("parent\x00build")) {
		t.Fatal("resolved step", steps)
	}
	if strings.Contains(string(sent.Args), "select") {
		t.Fatal("selector reached the coordinator", string(sent.Args))
	}
}

// TestWorkflowCheckResolvesPlacement verifies that tasks.check reports on the
// concrete plan its submission would send, and still submits nothing.
func TestWorkflowCheckResolvesPlacement(t *testing.T) {
	api := &placementAPI{one: model.NodeSelection{Node: model.Node{ID: "id-builder", Name: "builder"}}}
	c := api.client(t)
	spec := workflowSpec("parent", `{"steps":[{"name":"build","target":{"select":{"capability":"exec.run"}},"task":{"capability":"exec.run"}}]}`)
	var report model.TaskCheck
	if err := c.Call(t.Context(), "coordinator", model.TaskCheckMethod, spec, &report); err != nil || !report.OK {
		t.Fatal(report, err)
	}
	methods, submitted := api.calls()
	if submitted != 0 || !slices.Equal(methods, []string{"tasks.get", "nodes.select", model.TaskCheckMethod}) {
		t.Fatal("methods", methods, "submitted", submitted)
	}
	var sent model.TaskSpec
	_ = json.Unmarshal(api.checked[0], &sent)
	steps := decodePlan(t, sent.Args)
	if len(steps) != 1 || steps[0].Target != "id-builder" || steps[0].Placement != nil {
		t.Fatal("checked step", steps)
	}
}

const fanOutPlan = `{"maxParallel":4,"steps":[
	{"name":"prepare","target":"coordinator","task":{"id":"prep","capability":"exec.run"}},
	{"name":"patch","target":{"each":{"labels":{"env":"prod"}}},"needs":["prepare"],"task":{"id":"patch","capability":"exec.run"}},
	{"name":"verify","target":{"select":{"labels":{"env":"prod"}}},"needs":["patch"],"inputs":[{"step":"prepare","artifact":0,"path":"in"}],"task":{"capability":"exec.run"}},
	{"name":"cleanup","target":{"each":{"labels":{"env":"prod"}}},"when":"always","needs":["patch"],"task":{"capability":"exec.run"}},
	{"name":"report","target":"coordinator","needs":["cleanup","verify"],"task":{"capability":"exec.run"}}]}`

func TestWorkflowEachExpansion(t *testing.T) {
	api := &placementAPI{machines: machines("beta", "alpha"), one: machines("beta")[0]}
	c := api.client(t)
	r, err := c.ResolveTask(t.Context(), "coordinator", workflowSpec("parent", fanOutPlan))
	if err != nil {
		t.Fatal(err)
	}
	spec, plan := r.Spec, r.Steps
	steps := decodePlan(t, spec.Args)
	var names []string
	byName := map[string]model.WorkflowStep{}
	for _, step := range steps {
		names = append(names, step.Name)
		byName[step.Name] = step
		if step.Placement != nil || step.Target == "" || step.Task.ID == "" {
			t.Fatal("unresolved step", step)
		}
	}
	if !slices.Equal(names, []string{"prepare", "patch@beta", "patch@alpha", "verify", "cleanup@beta", "cleanup@alpha", "report"}) {
		t.Fatal("expanded names", names)
	}
	patches := []string{"patch@beta", "patch@alpha"}
	for _, machine := range []string{"alpha", "beta"} {
		patch, cleanup := byName["patch@"+machine], byName["cleanup@"+machine]
		if patch.Target != "id-"+machine || patch.Task.ID != identity.ID([]byte("patch\x00each\x00id-"+machine)) || !slices.Equal(patch.Needs, []string{"prepare"}) {
			t.Fatal("explicit-ID instance", patch)
		}
		if cleanup.Target != "id-"+machine || cleanup.When != model.WorkflowWhenAlways || !slices.Equal(cleanup.Needs, patches) || cleanup.Task.ID != identity.ID([]byte("parent\x00cleanup@"+machine)) {
			t.Fatal("cleanup instance", cleanup)
		}
	}
	if verify := byName["verify"]; verify.Target != "id-beta" || !slices.Equal(verify.Needs, patches) || len(verify.Inputs) != 1 || verify.Inputs[0].Step != "prepare" {
		t.Fatal("select step depending on a fan-out", verify)
	}
	if report := byName["report"]; !slices.Equal(report.Needs, []string{"cleanup@beta", "cleanup@alpha", "verify"}) || report.Target != "coordinator" || byName["prepare"].Task.ID != "prep" {
		t.Fatal("dependents of a fan-out", report)
	}
	if plan[1].From != "patch" || plan[1].Node != "beta" || plan[1].TaskID != byName["patch@beta"].Task.ID || plan[0].From != "" || plan[0].Target != "coordinator" {
		t.Fatal("resolved plan description", plan)
	}
	if api.selectAll != 1 {
		t.Fatal("identical each selectors were discovered separately", api.selectAll)
	}
	// Names and IDs depend only on the plan, parent ID and matched machines.
	again, err := c.ResolveTask(t.Context(), "coordinator", workflowSpec("parent", fanOutPlan))
	if err != nil || string(again.Spec.Args) != string(spec.Args) {
		t.Fatal("resolution is not deterministic", err)
	}
	// The cleanup steps compose with the coordinator capability check and the
	// coordinator receives exactly the printed plan.
	before, _ := api.calls()
	if _, err = c.StartTask(t.Context(), "coordinator", spec, nil); err != nil {
		t.Fatal(err)
	}
	methods, _ := api.calls()
	if !slices.Equal(methods[len(before):], []string{"capabilities.list", "tasks.start"}) {
		t.Fatal("resolved plan was resolved again", methods[len(before):])
	}
	var sent model.TaskSpec
	_ = json.Unmarshal(api.submitted[0], &sent)
	if string(sent.Args) != string(spec.Args) {
		t.Fatal("submitted plan differs from the resolved plan")
	}
}

func TestWorkflowPlacementFailuresSubmitNothing(t *testing.T) {
	plan := func(steps ...string) string { return `{"steps":[` + strings.Join(steps, ",") + `]}` }
	each := func(name, labels string) string {
		return fmt.Sprintf(`{"name":%q,"target":{"each":{"labels":{"group":%q}}},"task":{"capability":"exec.run"}}`, name, labels)
	}
	for _, tc := range []struct {
		name      string
		api       *placementAPI
		plan      string
		want      string
		discovery bool
	}{
		{"empty match", &placementAPI{}, plan(each("patch", "none")), "each: no online enabled node", true},
		{"older selector", &placementAPI{legacyAll: true, one: machines("a")[0]}, plan(each("patch", "x")), "does not support nodes.select with all", true},
		{"step limit after expansion", &placementAPI{machines: machines(seq(60)...)}, plan(each("a", "x"), each("b", "y")), "workflow expands to 120 steps (a on 60 machines, b on 60 machines); at most 100", true},
		{"ambiguous artifact input", &placementAPI{machines: machines("a")}, plan(each("build", "x"), `{"name":"collect","target":"c","inputs":[{"step":"build","artifact":0,"path":"in"}],"task":{"capability":"exec.run"}}`), "ambiguous", false},
		{"misspelled selector", &placementAPI{machines: machines("a")}, plan(`{"name":"a","target":{"each":{"label":{}}},"task":{}}`), "unknown field", false},
		{"unknown dependency", &placementAPI{machines: machines("a")}, plan(`{"name":"a","target":{"each":{}},"needs":["missing"],"task":{}}`), "unknown dependency missing", true},
		{"name collision", &placementAPI{machines: machines("x")}, plan(each("a", "x"), `{"name":"a@x","target":"c","task":{}}`), "unique", true},
		{"unconfirmed existing task", &placementAPI{broken: true, machines: machines("a")}, plan(each("a", "x")), "already exists", false},
		{"pruned task ID", &placementAPI{existing: &model.Task{ID: "parent", Spec: model.TaskSpec{ID: "parent", Capability: "workflow.run"}, Pruned: true}, machines: machines("a")}, plan(each("a", "x")), "not retained", false},
		{"reply for another task", &placementAPI{existing: &model.Task{ID: "other", Spec: model.TaskSpec{ID: "other", Capability: "workflow.run", Args: json.RawMessage(`{"steps":[]}`)}}, machines: machines("a")}, plan(each("a", "x")), `answered with task "other"`, false},
		{"reply with another specification ID", &placementAPI{existing: &model.Task{ID: "parent", Spec: model.TaskSpec{ID: "other", Capability: "workflow.run", Args: json.RawMessage(`{"steps":[]}`)}}, machines: machines("a")}, plan(each("a", "x")), `specification "other"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := tc.api
			c := api.client(t)
			_, err := c.StartTask(t.Context(), "coordinator", workflowSpec("parent", tc.plan), nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
			var rpc *model.RPCError
			if !strings.Contains(err.Error(), "nothing was submitted") && (!errors.As(err, &rpc) || rpc.Outcome != "not_sent") {
				t.Fatal("resolution failure does not report not_sent", err)
			}
			methods, submitted := api.calls()
			if submitted != 0 || slices.Contains(methods, "tasks.start") {
				t.Fatal("resolution failure submitted work", methods)
			}
			if !tc.discovery && slices.Contains(methods, "nodes.select") {
				t.Fatal("invalid plan was discovered before validation", methods)
			}
		})
	}
	// Exactly 100 steps after expansion is allowed.
	api := &placementAPI{machines: machines(seq(50)...)}
	if _, err := api.client(t).StartTask(t.Context(), "coordinator", workflowSpec("parent", plan(each("a", "x"), each("b", "y"))), nil); err != nil {
		t.Fatal("100 expanded steps refused", err)
	}
}

// A resubmitted task ID returns the coordinator's retained task read-only,
// even when discovery would now choose other machines: nothing is resolved,
// checked for grants or submitted again, on every client route.
func TestWorkflowPlacementResubmissionReturnsRetainedTask(t *testing.T) {
	first := &placementAPI{machines: machines("alpha", "beta"), one: machines("beta")[0]}
	resolution, err := first.client(t).ResolveTask(t.Context(), "coordinator", workflowSpec("parent", fanOutPlan))
	if err != nil {
		t.Fatal(err)
	}
	resolved, plan := resolution.Spec, resolution.Steps
	retained := model.Task{ID: "parent", State: "running", Spec: resolved}
	api := &placementAPI{machines: machines("gamma"), one: machines("gamma")[0], existing: &retained}
	c := api.client(t)
	again, err := c.ResolveTask(t.Context(), "coordinator", workflowSpec("parent", fanOutPlan))
	if err != nil || again.Existing == nil || again.Existing.State != "running" || string(again.Spec.Args) != string(resolved.Args) {
		t.Fatal("resubmission resolved again", err)
	}
	for i := range again.Steps {
		// The retained plan names targets by ID; names are not re-read.
		if again.Steps[i].Name != plan[i].Name || again.Steps[i].Target != plan[i].Target || again.Steps[i].TaskID != plan[i].TaskID {
			t.Fatal("reported plan differs", again.Steps[i], plan[i])
		}
	}
	// A differently encoded but identical request matches too.
	var reordered map[string]any
	if err = json.Unmarshal([]byte(fanOutPlan), &reordered); err != nil {
		t.Fatal(err)
	}
	for name, start := range map[string]func(any) error{
		"StartTask": func(result any) error {
			_, err := c.StartTask(t.Context(), "coordinator", workflowSpec("parent", fanOutPlan), result)
			return err
		},
		"StartResolvedTask": func(result any) error {
			_, err := c.StartResolvedTask(t.Context(), "coordinator", again, result)
			return err
		},
		"Call": func(result any) error {
			return c.Call(t.Context(), "coordinator", "tasks.start", workflowSpec("parent", string(model.JSON(reordered))), result)
		},
	} {
		var task model.Task
		if err = start(&task); err != nil || task.ID != "parent" || task.State != "running" {
			t.Fatal(name, task, err)
		}
	}
	// A preflight of the same request reports the existing task without
	// sending the retained plan anywhere.
	var report model.TaskCheck
	if err = c.Call(t.Context(), "coordinator", model.TaskCheckMethod, workflowSpec("parent", fanOutPlan), &report); err != nil || !report.OK || !report.Advisory || report.TaskID != "parent" || len(report.Checks) != 1 || !strings.Contains(report.Checks[0].Message, "already exists") {
		t.Fatal("preflight of an existing task", report, err)
	}
	methods, submitted := api.calls()
	for _, method := range methods {
		if method != "tasks.get" {
			t.Fatal("resubmission sent more than reads", methods)
		}
	}
	if submitted != 0 || len(api.checked) != 0 {
		t.Fatal("resubmission submitted or checked", submitted, len(api.checked))
	}
}

// A request that differs from, or cannot be verified against, the task that
// already holds its ID is refused before discovery, grants or submission, for
// starts and preflights alike. The retained plan is never sent back.
func TestWorkflowPlacementRetainedRequestMismatch(t *testing.T) {
	first := &placementAPI{machines: machines("alpha", "beta"), one: machines("beta")[0]}
	resolution, err := first.client(t).ResolveTask(t.Context(), "coordinator", workflowSpec("parent", fanOutPlan))
	if err != nil {
		t.Fatal(err)
	}
	var withDigest model.WorkflowPlan
	if err = json.Unmarshal(resolution.Spec.Args, &withDigest); err != nil || !strings.HasPrefix(withDigest.RequestDigest, "sha256:") {
		t.Fatal("resolved plan records no request digest", withDigest.RequestDigest, err)
	}
	legacy := withDigest
	legacy.RequestDigest = ""
	concrete := `{"steps":[{"name":"s","target":"id-alpha","task":{"id":"s","capability":"exec.run"}}]}`
	changed := workflowSpec("parent", strings.Replace(fanOutPlan, `"maxParallel":4`, `"maxParallel":2`, 1))
	retimed := workflowSpec("parent", fanOutPlan)
	retimed.TimeoutSeconds = 60
	for _, tc := range []struct {
		name     string
		retained model.TaskSpec
		request  model.TaskSpec
		want     string
	}{
		{"changed plan", resolution.Spec, changed, "different workflow request"},
		{"changed timeout", resolution.Spec, retimed, "different workflow request"},
		{"digestless resolved plan", model.TaskSpec{ID: "parent", Capability: "workflow.run", Args: model.JSON(legacy)}, workflowSpec("parent", fanOutPlan), "cannot verify original workflow request"},
		{"earlier concrete workflow", model.TaskSpec{ID: "parent", Capability: "workflow.run", Args: json.RawMessage(concrete)}, workflowSpec("parent", fanOutPlan), "cannot verify original workflow request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &placementAPI{machines: machines("gamma"), one: machines("gamma")[0], existing: &model.Task{ID: "parent", State: "succeeded", Spec: tc.retained}}
			c := api.client(t)
			attempts := map[string]func() error{
				"StartTask": func() error {
					_, err := c.StartTask(t.Context(), "coordinator", tc.request, nil)
					return err
				},
				"Call": func() error { return c.Call(t.Context(), "coordinator", "tasks.start", tc.request, &model.Task{}) },
				"check": func() error {
					return c.Call(t.Context(), "coordinator", model.TaskCheckMethod, tc.request, &model.TaskCheck{})
				},
				"ResolveTask": func() error {
					r, err := c.ResolveTask(t.Context(), "coordinator", tc.request)
					if r.Existing != nil {
						t.Error("refused resolution returned the retained task")
					}
					return err
				},
			}
			for name, attempt := range attempts {
				err := attempt()
				var rpc *model.RPCError
				if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), `control task get "coordinator" parent`) || !errors.As(err, &rpc) || rpc.Outcome != "not_sent" {
					t.Fatalf("%s: error %v, want not_sent %q", name, err, tc.want)
				}
			}
			methods, submitted := api.calls()
			for _, method := range methods {
				if method != "tasks.get" {
					t.Fatal("refused request sent", methods)
				}
			}
			if submitted != 0 || len(api.checked) != 0 {
				t.Fatal("refused request submitted or checked", submitted, len(api.checked))
			}
		})
	}
}

// The request digest covers the whole original specification and ignores
// only its encoding, tag order and any caller-supplied digest.
func TestWorkflowRequestDigest(t *testing.T) {
	digest := func(spec model.TaskSpec) string {
		t.Helper()
		var plan model.WorkflowPlan
		if err := json.Unmarshal(spec.Args, &plan); err != nil {
			t.Fatal(err)
		}
		d, err := workflowRequestDigest(spec, plan)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	const plan = `{"steps":[{"name":"s","target":{"each":{"labels":{"env":"prod"}}},"task":{"capability":"exec.run","args":{"command":"a","n":1.50}}}]}`
	base := workflowSpec("parent", plan)
	base.Tags = []string{"b", "a"}
	// This digest outlives the client that submitted the task. A later
	// release must still recognize the same request.
	const recorded = "sha256:27c6f958576026bf23ae17ad3be73d71f3f1718eabd43f4010cf921b36cc478c"
	if got := digest(base); got != recorded {
		t.Fatalf("digest of an existing workflow request changed: %s", got)
	}
	same := workflowSpec("parent", `{ "requestDigest":"sha256:forged", "steps":[{"task":{"args":{"n":1.50,"command":"a"},"capability":"exec.run"},"target":{"each":{"labels":{"env":"prod"}}},"name":"s"}]}`)
	same.Tags = []string{"a", "b"}
	if digest(base) != digest(same) {
		t.Fatal("equivalent encodings differ")
	}
	childTags := func(tags string) string {
		tagged := base
		tagged.Args = json.RawMessage(strings.Replace(plan, `"task":{`, `"task":{"tags":`+tags+`,`, 1))
		return digest(tagged)
	}
	if childTags(`["b","a","b"]`) != childTags(`["a","b"]`) || childTags(`["a"]`) == childTags(`["b"]`) {
		t.Fatal("request digest must distinguish child tag sets, not tag order")
	}
	for name, mutate := range map[string]func(*model.TaskSpec){
		"ID":       func(s *model.TaskSpec) { s.ID = "other" },
		"child":    func(s *model.TaskSpec) { s.Args = json.RawMessage(strings.Replace(plan, `"a"`, `"b"`, 1)) },
		"number":   func(s *model.TaskSpec) { s.Args = json.RawMessage(strings.Replace(plan, "1.50", "1.5", 1)) },
		"selector": func(s *model.TaskSpec) { s.Args = json.RawMessage(strings.Replace(plan, "prod", "dev", 1)) },
		"lease":    func(s *model.TaskSpec) { s.LeaseID = "lease" },
		"tags":     func(s *model.TaskSpec) { s.Tags = []string{"a"} },
		"outputs":  func(s *model.TaskSpec) { s.Outputs = []string{"out"} },
	} {
		changed := base
		mutate(&changed)
		if digest(changed) == digest(base) {
			t.Fatal("digest ignores a change to", name)
		}
	}
}

// A direct synchronous workflow.run is resolved too, with fresh child IDs.
func TestWorkflowPlacementSynchronousCall(t *testing.T) {
	api := &placementAPI{machines: machines("a", "b")}
	c := api.client(t)
	if err := c.Call(t.Context(), "coordinator", "workflow.run", json.RawMessage(`{"steps":[{"name":"s","target":{"each":{}},"task":{"capability":"exec.run"}}]}`), nil); err != nil {
		t.Fatal(err)
	}
	methods, _ := api.calls()
	if !slices.Equal(methods, []string{"nodes.select", "workflow.run"}) {
		t.Fatal(methods)
	}
	steps := decodePlan(t, api.submitted[0])
	if len(steps) != 2 || steps[0].Name != "s@a" || steps[1].Target != "id-b" || steps[0].Task.ID == "" || steps[0].Task.ID == steps[1].Task.ID {
		t.Fatal(steps)
	}
}

func TestWorkflowFanOutExample(t *testing.T) {
	b, err := os.ReadFile("../../examples/workflow-fanout-task.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec model.TaskSpec
	if err = json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	api := &placementAPI{machines: machines("db", "web"), one: machines("web")[0]}
	r, err := api.client(t).ResolveTask(t.Context(), "coordinator", spec)
	if steps := r.Steps; err != nil || len(steps) != 5 || steps[0].Name != "rotate@db" || steps[2].Name != "summarize" || steps[4].When != model.WorkflowWhenAlways {
		t.Fatal(steps, err)
	}
}

func seq(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("m%03d", i)
	}
	return out
}
