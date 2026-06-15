package ghrunner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/austenstone/tabrunner/internal/runner"
	"github.com/austenstone/tabrunner/internal/workflow"
)

// completeJobRequest is the minimal body we POST to the Run Service completejob
// endpoint. The run's conclusion derives from this top-level conclusion alone;
// stepResults are optional and omitted for now.
type completeJobRequest struct {
	PlanID     string `json:"planId"`
	JobID      string `json:"jobId"`
	Conclusion string `json:"conclusion"`
}

// litValue extracts the `lit` string from an expression-tree leaf node such as
// {"type":0,"lit":"..."}. Casing of `lit` is stable (lowercase) in the wire
// format, but we go case-insensitive for safety.
func litValue(raw json.RawMessage) (string, bool) {
	var node map[string]json.RawMessage
	if json.Unmarshal(raw, &node) != nil {
		return "", false
	}
	if v, ok := ciField(node, "lit"); ok {
		var s string
		if json.Unmarshal(v, &s) == nil {
			return s, true
		}
	}
	return "", false
}

// stepScript pulls the shell command out of an acquired-job step. The command
// lives at steps[i].inputs.map[j].Value.lit where the sibling Key.lit == "script".
// Note inputs.map[].Key/Value are PascalCase while the rest is camelCase.
func stepScript(step map[string]json.RawMessage) string {
	inputs, ok := ciField(step, "inputs")
	if !ok {
		return ""
	}
	var inObj map[string]json.RawMessage
	if json.Unmarshal(inputs, &inObj) != nil {
		return ""
	}
	mapRaw, ok := ciField(inObj, "map")
	if !ok {
		return ""
	}
	var pairs []map[string]json.RawMessage
	if json.Unmarshal(mapRaw, &pairs) != nil {
		return ""
	}
	for _, pair := range pairs {
		keyRaw, ok := ciField(pair, "Key")
		if !ok {
			continue
		}
		key, _ := litValue(keyRaw)
		if key != "script" {
			continue
		}
		valRaw, ok := ciField(pair, "Value")
		if !ok {
			continue
		}
		if cmd, ok := litValue(valRaw); ok {
			return cmd
		}
	}
	return ""
}

// githubContextValue walks contextData.github.d[] (a list of {k,v} entries) and
// returns the string value for the requested key.
func githubContextValue(github map[string]json.RawMessage, key string) string {
	dRaw, ok := ciField(github, "d")
	if !ok {
		return ""
	}
	var entries []map[string]json.RawMessage
	if json.Unmarshal(dRaw, &entries) != nil {
		return ""
	}
	for _, e := range entries {
		kRaw, ok := ciField(e, "k")
		if !ok {
			continue
		}
		var k string
		if json.Unmarshal(kRaw, &k) != nil || k != key {
			continue
		}
		vRaw, ok := ciField(e, "v")
		if !ok {
			return ""
		}
		var s string
		if json.Unmarshal(vRaw, &s) == nil {
			return s
		}
		return ""
	}
	return ""
}

// translateAcquiredJob converts a raw AgentJobRequestMessage into tabrunner's
// internal workflow model plus a runner.Context and an env map (carrying the
// job's GITHUB_TOKEN). Only `run:` (script) steps are translated; `uses:` steps
// are skipped at M0.
func translateAcquiredJob(raw json.RawMessage) (*workflow.Workflow, runner.Context, map[string]string, error) {
	rc := runner.DefaultContext()
	env := map[string]string{}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, rc, env, fmt.Errorf("parse acquired job: %w", err)
	}

	// Job name/display name.
	jobName := "build"
	if v, ok := ciField(top, "jobName"); ok {
		_ = json.Unmarshal(v, &jobName)
	}
	jobDisplay := jobName
	if v, ok := ciField(top, "jobDisplayName"); ok {
		_ = json.Unmarshal(v, &jobDisplay)
	}

	// Steps.
	var steps []*workflow.Step
	if v, ok := ciField(top, "steps"); ok {
		var rawSteps []map[string]json.RawMessage
		if json.Unmarshal(v, &rawSteps) == nil {
			dbg("translate: rawSteps=%d", len(rawSteps))
			for si, s := range rawSteps {
				cmd := stepScript(s)
				dbg("translate step %d: scriptLen=%d script=%q", si, len(cmd), truncate(cmd, 120))
				if cmd == "" {
					continue // uses-step or non-script; skip for now
				}
				step := &workflow.Step{Run: cmd}
				if idv, ok := ciField(s, "id"); ok {
					_ = json.Unmarshal(idv, &step.ID)
				}
				if nv, ok := ciField(s, "name"); ok {
					_ = json.Unmarshal(nv, &step.Name)
				}
				if step.Name == "" {
					step.Name = "run"
				}
				steps = append(steps, step)
			}
		}
	}

	// Context from contextData.github.
	if cd, ok := ciField(top, "contextData"); ok {
		var cdObj map[string]json.RawMessage
		if json.Unmarshal(cd, &cdObj) == nil {
			if gh, ok := ciField(cdObj, "github"); ok {
				var ghObj map[string]json.RawMessage
				if json.Unmarshal(gh, &ghObj) == nil {
					if v := githubContextValue(ghObj, "repository"); v != "" {
						rc.Repository = v
					}
					if v := githubContextValue(ghObj, "sha"); v != "" {
						rc.SHA = v
					}
					if v := githubContextValue(ghObj, "ref"); v != "" {
						rc.Ref = v
					}
					if v := githubContextValue(ghObj, "run_id"); v != "" {
						rc.RunID = v
					}
				}
			}
		}
	}
	rc.RunnerOS = "Wasm"
	rc.RunnerArch = "wasm32"

	// GITHUB_TOKEN from variables.github_token.value.
	if vars, ok := ciField(top, "variables"); ok {
		var varsObj map[string]json.RawMessage
		if json.Unmarshal(vars, &varsObj) == nil {
			if tok, ok := ciField(varsObj, "github_token", "system.github.token"); ok {
				var tokObj map[string]json.RawMessage
				if json.Unmarshal(tok, &tokObj) == nil {
					if val, ok := ciField(tokObj, "value"); ok {
						var s string
						if json.Unmarshal(val, &s) == nil && s != "" {
							env["GITHUB_TOKEN"] = s
						}
					}
				}
			}
		}
	}

	job := &workflow.Job{
		ID:    jobName,
		Name:  jobDisplay,
		Steps: steps,
	}
	wf := &workflow.Workflow{
		Name: jobDisplay,
		Jobs: map[string]*workflow.Job{jobName: job},
	}
	return wf, rc, env, nil
}

// executeAndComplete translates an acquired job, runs its steps in Wasm, derives
// the overall conclusion, and reports it back to GitHub via completejob so the
// run goes green (or red).
func executeAndComplete(ctx context.Context, job *acquiredJob, jr *runnerJobRequest, token string) error {
	wf, rc, env, err := translateAcquiredJob(job.raw)
	if err != nil {
		return fmt.Errorf("translate job: %w", err)
	}
	// Inject the job env (GITHUB_TOKEN) at the workflow level.
	if len(env) > 0 {
		if wf.Env == nil {
			wf.Env = map[string]string{}
		}
		for k, v := range env {
			wf.Env[k] = v
		}
	}

	r, err := runner.New(ctx, rc)
	if err != nil {
		return fmt.Errorf("new runner: %w", err)
	}
	r.SetOutput(logWriter{})
	defer r.Close(ctx)

	outcomes, runErr := r.RunWorkflowWithResults(ctx, wf, "")

	conclusion := "succeeded"
	if runErr != nil {
		conclusion = "failed"
	}
	for _, o := range outcomes {
		if o.Conclusion == "failed" {
			conclusion = "failed"
			break
		}
	}
	dbg("execute: conclusion=%s runErr=%v steps=%d", conclusion, runErr, len(outcomes))
	for i, o := range outcomes {
		dbg("execute step %d: name=%q exit=%d conclusion=%s", i, o.Name, o.ExitCode, o.Conclusion)
	}

	completeToken := job.runServiceToken
	if completeToken == "" {
		completeToken = token
	}
	if cerr := completeJob(ctx, jr.RunServiceURL, completeToken, job.planID, job.jobID, conclusion); cerr != nil {
		return fmt.Errorf("complete job: %w", cerr)
	}
	return nil
}

// completeJob reports the final job conclusion to the Run Service. The body is
// minimal (planId, jobId, conclusion); the run's conclusion derives from this
// alone. 401/404 are terminal; other failures are logged by the caller.
func completeJob(ctx context.Context, runServiceURL, token, planID, jobID, conclusion string) error {
	if runServiceURL == "" {
		return fmt.Errorf("missing run_service_url")
	}
	u := strings.TrimRight(runServiceURL, "/") + "/completejob"
	body := completeJobRequest{PlanID: planID, JobID: jobID, Conclusion: conclusion}

	resp, err := doJSON(ctx, http.MethodPost, u, "Bearer "+token, "", body, nil)
	if err != nil {
		if resp != nil {
			switch resp.StatusCode {
			case http.StatusUnauthorized, http.StatusNotFound:
				return fmt.Errorf("completejob terminal (%s): %w", resp.Status, err)
			}
		}
		return fmt.Errorf("completejob: %w", err)
	}
	fmt.Printf("completejob ok: planId=%s jobId=%s conclusion=%s\n", planID, jobID, conclusion)
	return nil
}
