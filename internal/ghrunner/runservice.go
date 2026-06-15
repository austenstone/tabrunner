package ghrunner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// runnerJobRequestType is the broker message type that carries a dispatched job.
// Unlike the classic encrypted PipelineAgentJobRequest, the broker delivers a
// small plaintext pointer telling us where to ACQUIRE the real job.
const runnerJobRequestType = "RunnerJobRequest"

// runnerJobRequest is the plaintext body of a RunnerJobRequest broker message.
// It does not contain the job itself, only the coordinates to acquire it from
// the Run Service.
type runnerJobRequest struct {
	RunnerRequestID  string `json:"runner_request_id"`
	RunServiceURL    string `json:"run_service_url"`
	BillingOwnerID   string `json:"billing_owner_id"`
	ShouldAcknowledge bool  `json:"should_acknowledge"`
}

func parseRunnerJobRequest(body string) (*runnerJobRequest, error) {
	var jr runnerJobRequest
	if err := json.Unmarshal([]byte(body), &jr); err != nil {
		return nil, fmt.Errorf("parse RunnerJobRequest: %w", err)
	}
	if jr.RunnerRequestID == "" {
		return nil, fmt.Errorf("RunnerJobRequest missing runner_request_id")
	}
	return &jr, nil
}

// acquireJobRequest is the body we POST to the Run Service acquirejob endpoint
// to claim a job that the broker told us about.
type acquireJobRequest struct {
	JobMessageID   string `json:"jobMessageId"`
	RunnerOS       string `json:"runnerOS"`
	BillingOwnerID string `json:"billingOwnerId"`
}

// acquiredJob is the parsed result of acquiring a job. We keep the raw bytes
// because the AgentJobRequestMessage schema is large and casing is inconsistent
// across fields; raw lets us ground-truth the real shape and translate later.
type acquiredJob struct {
	raw    json.RawMessage
	jobID  string
	planID string
	// runServiceToken is the job-scoped OAuth token embedded in the acquired job
	// message. Per-job operations (completejob, logs, timeline) must authenticate
	// with THIS token, not the runner's session OAuth token.
	runServiceToken string
}

// ciField does a case-insensitive lookup over a decoded JSON object, returning
// the first matching key's raw value. GitHub's AgentJobRequestMessage mixes
// camelCase and PascalCase, so we cannot rely on exact key names.
func ciField(m map[string]json.RawMessage, names ...string) (json.RawMessage, bool) {
	for _, name := range names {
		want := strings.ToLower(name)
		for k, v := range m {
			if strings.ToLower(k) == want {
				return v, true
			}
		}
	}
	return nil, false
}

// acquireJob claims the job described by a RunnerJobRequest. On success it dumps
// the raw AgentJobRequestMessage to .tabrunner/acquired_job.json and extracts the
// job and plan IDs. 404/409/422 mean the job is gone or already taken; we treat
// those as a skip (nil, nil) rather than an error to retry.
func acquireJob(ctx context.Context, jr *runnerJobRequest, token string) (*acquiredJob, error) {
	if jr.RunServiceURL == "" {
		return nil, fmt.Errorf("RunnerJobRequest missing run_service_url")
	}
	u := strings.TrimRight(jr.RunServiceURL, "/") + "/acquirejob"
	reqBody := acquireJobRequest{
		JobMessageID:   jr.RunnerRequestID,
		RunnerOS:       "Linux",
		BillingOwnerID: jr.BillingOwnerID,
	}

	var raw json.RawMessage
	resp, err := doJSON(ctx, http.MethodPost, u, "Bearer "+token, "", reqBody, &raw)
	if err != nil {
		if resp != nil {
			switch resp.StatusCode {
			case http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity:
				fmt.Printf("acquire skipped (%s): job unavailable or already taken\n", resp.Status)
				return nil, nil
			}
		}
		return nil, fmt.Errorf("acquire job: %w", err)
	}

	dumpPath := filepath.Join(settingsDir, "acquired_job.json")
	if werr := os.WriteFile(dumpPath, raw, 0o600); werr != nil {
		fmt.Printf("dump acquired job: %v\n", werr)
	} else {
		fmt.Printf("acquired job dumped to %s (%d bytes)\n", dumpPath, len(raw))
	}

	job := &acquiredJob{raw: raw}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("parse acquired job: %w", err)
	}
	if v, ok := ciField(top, "jobId"); ok {
		_ = json.Unmarshal(v, &job.jobID)
	}
	if v, ok := ciField(top, "plan"); ok {
		var plan map[string]json.RawMessage
		if json.Unmarshal(v, &plan) == nil {
			if pv, ok := ciField(plan, "planId"); ok {
				_ = json.Unmarshal(pv, &job.planID)
			}
		}
	}
	job.runServiceToken = extractRunServiceToken(top)
	return job, nil
}

// extractRunServiceToken pulls the job-scoped OAuth token out of the acquired job
// message at resources.endpoints[0].authorization.parameters.AccessToken. Keys are
// PascalCase under authorization, so we use ciField throughout.
func extractRunServiceToken(top map[string]json.RawMessage) string {
	resV, ok := ciField(top, "resources")
	if !ok {
		return ""
	}
	var res map[string]json.RawMessage
	if json.Unmarshal(resV, &res) != nil {
		return ""
	}
	epV, ok := ciField(res, "endpoints")
	if !ok {
		return ""
	}
	var eps []map[string]json.RawMessage
	if json.Unmarshal(epV, &eps) != nil || len(eps) == 0 {
		return ""
	}
	authV, ok := ciField(eps[0], "authorization")
	if !ok {
		return ""
	}
	var auth map[string]json.RawMessage
	if json.Unmarshal(authV, &auth) != nil {
		return ""
	}
	paramsV, ok := ciField(auth, "parameters")
	if !ok {
		return ""
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(paramsV, &params) != nil {
		return ""
	}
	tokV, ok := ciField(params, "AccessToken")
	if !ok {
		return ""
	}
	var tok string
	_ = json.Unmarshal(tokV, &tok)
	return tok
}
