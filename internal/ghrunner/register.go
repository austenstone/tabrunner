package ghrunner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// runnerVersion is reported to the service. It influences whether GitHub thinks
// the agent needs a self-update; a recent value avoids forced-update messages.
const runnerVersion = "2.335.1"

const dotcomAPIBase = "https://api.github.com"

// httpClient is shared for short request/response calls.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// pollClient has no client-level deadline so the long-poll is governed purely by
// the per-request context. http.Client.Timeout is an absolute cap that a longer
// request context cannot extend, so reusing httpClient would kill the ~50s
// long-poll at 30s.
var pollClient = &http.Client{}

// GitHubAuthResult is the response from the RemoteAuth handshake
// (POST /actions/runner-registration). It tells us the Actions service URL, a
// short-lived bearer token, and whether this tenant uses the v2/broker flow.
type GitHubAuthResult struct {
	URL         string `json:"url"`
	TokenSchema string `json:"token_schema"`
	Token       string `json:"token"`
	UseV2Flow   bool   `json:"use_v2_flow"`
}

type taskAgentPool struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	IsInternal bool   `json:"isInternal"`
}

type poolList struct {
	Count int             `json:"count"`
	Value []taskAgentPool `json:"value"`
}

type publicKey struct {
	Exponent string `json:"exponent"`
	Modulus  string `json:"modulus"`
}

type agentLabel struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type agentAuthorization struct {
	ClientID         string     `json:"clientId,omitempty"`
	AuthorizationURL string     `json:"authorizationUrl,omitempty"`
	PublicKey        *publicKey `json:"publicKey,omitempty"`
}

type taskAgent struct {
	ID             int                `json:"id,omitempty"`
	Name           string             `json:"name"`
	Version        string             `json:"version"`
	OSDescription  string             `json:"osDescription,omitempty"`
	MaxParallelism int                `json:"maxParallelism"`
	Ephemeral      bool               `json:"ephemeral"`
	DisableUpdate  bool               `json:"disableUpdate"`
	ProvisioningSt string             `json:"provisioningState,omitempty"`
	Labels         []agentLabel       `json:"labels,omitempty"`
	Authorization  agentAuthorization `json:"authorization,omitempty"`
	Properties     map[string]any     `json:"properties,omitempty"`
}

// Settings is tabrunner's persisted runner identity (a merge of the real
// runner's .runner + .credentials). It is everything needed to authenticate and
// start listening on subsequent runs.
type Settings struct {
	AgentID          int    `json:"agentId"`
	AgentName        string `json:"agentName"`
	PoolID           int    `json:"poolId"`
	ServerURL        string `json:"serverUrl"`
	ServerURLV2      string `json:"serverUrlV2,omitempty"`
	GitHubURL        string `json:"gitHubUrl"`
	UseV2Flow        bool   `json:"useV2Flow"`
	Ephemeral        bool   `json:"ephemeral"`
	ClientID         string `json:"clientId"`
	AuthorizationURL string `json:"authorizationUrl"`
}

// RegisterOptions controls a registration attempt.
type RegisterOptions struct {
	GitHubURL string   // e.g. https://github.com/octodemo
	RegToken  string   // org/repo registration token (short-lived)
	Name      string   // optional; random tabrunner-xxxx if empty
	Labels    []string // extra labels beyond the system "self-hosted"
	Ephemeral bool
	GroupID   int // v2 runner group; defaults to 1 (Default) when zero
}

func randomName() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "tabrunner-" + hex.EncodeToString(b)
}

func doJSON(ctx context.Context, method, url, authHeader, apiVersion string, body any, out any) (*http.Response, error) {
	return doJSONWithClient(ctx, httpClient, method, url, authHeader, apiVersion, body, out)
}

func doJSONWithClient(ctx context.Context, client *http.Client, method, url, authHeader, apiVersion string, body any, out any) (*http.Response, error) {
	// The Actions/Azure DevOps service negotiates the request/response contract
	// version through the media type, NOT the query string. Without an
	// "api-version" parameter on Content-Type/Accept the agents controller binds
	// the body against the wrong contract and yields a null model, producing the
	// infamous "Value cannot be null. (Parameter 'agent')" 400.
	mediaType := "application/json"
	if apiVersion != "" {
		mediaType = "application/json; api-version=" + apiVersion
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	if body != nil {
		req.Header.Set("Content-Type", mediaType)
	}
	req.Header.Set("Accept", mediaType)
	req.Header.Set("User-Agent", "tabrunner/0.1 (+https://github.com/austenstone/tabrunner)")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		resp.Body.Close()
		return resp, fmt.Errorf("%s %s -> %s: %s", method, trimURL(url), resp.Status, strings.TrimSpace(string(data)))
	}
	defer resp.Body.Close()
	if out != nil {
		// Long-poll endpoints return 200/204 with an EMPTY body when the
		// server-side poll times out with no work. An empty body is not a
		// decode error -- treat it as "no content" and leave out at zero value.
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return resp, fmt.Errorf("read response: %w", err)
		}
		if len(bytes.TrimSpace(data)) == 0 {
			return resp, nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return resp, fmt.Errorf("decode response: %w", err)
		}
	}
	return resp, nil
}

func trimURL(u string) string {
	if i := strings.Index(u, "?"); i >= 0 {
		return u[:i]
	}
	return u
}

// Handshake performs the RemoteAuth registration handshake and returns the
// Actions service URL + bearer token. This is Phase 1a and also the cheapest way
// to learn whether the tenant is classic or v2/broker.
func Handshake(ctx context.Context, githubURL, regToken string) (*GitHubAuthResult, error) {
	var res GitHubAuthResult
	body := map[string]string{"url": githubURL, "runner_event": "register"}
	_, err := doJSON(ctx, http.MethodPost, dotcomAPIBase+"/actions/runner-registration",
		"RemoteAuth "+regToken, "", body, &res)
	if err != nil {
		return nil, fmt.Errorf("runner-registration handshake: %w", err)
	}
	return &res, nil
}

// apiURL joins the Actions service base (which carries a trailing slash) with an
// _apis path, avoiding a double slash.
func apiURL(base, path string) string {
	return strings.TrimRight(base, "/") + path
}

// listPools enumerates the agent pools visible to the handshake token. For org-
// level runners GitHub exposes a single internal pool.
func listPools(ctx context.Context, tenantURL, token string) ([]taskAgentPool, error) {
	var pools poolList
	u := apiURL(tenantURL, "/_apis/distributedtask/pools?api-version=5.1-preview.1")
	if _, err := doJSON(ctx, http.MethodGet, u, "Bearer "+token, "5.1-preview.1", nil, &pools); err != nil {
		return nil, fmt.Errorf("list pools: %w", err)
	}
	return pools.Value, nil
}

// pickPool selects the internal pool GitHub created for this org/repo, falling
// back to the first available pool.
func pickPool(pools []taskAgentPool) (taskAgentPool, error) {
	for _, p := range pools {
		if p.IsInternal {
			return p, nil
		}
	}
	if len(pools) > 0 {
		return pools[0], nil
	}
	return taskAgentPool{}, fmt.Errorf("no agent pools available")
}

// registerAgent creates the TaskAgent in the given pool and returns the created
// agent (including its OAuth client credentials).
func registerAgent(ctx context.Context, tenantURL, token string, poolID int, agent taskAgent) (*taskAgent, error) {
	var out taskAgent
	u := apiURL(tenantURL, fmt.Sprintf("/_apis/distributedtask/pools/%d/agents?api-version=6.0-preview.2", poolID))
	if _, err := doJSON(ctx, http.MethodPost, u, "Bearer "+token, "6.0-preview.2", agent, &out); err != nil {
		return nil, fmt.Errorf("register agent: %w", err)
	}
	return &out, nil
}

// Register performs the full classic self-hosted runner registration: handshake,
// RSA keygen, pool discovery, agent registration, and persistence to ./.tabrunner.
// On success the runner identity can be reused on subsequent runs without a new
// registration token.
func Register(ctx context.Context, opts RegisterOptions) (*Settings, error) {
	auth, err := Handshake(ctx, opts.GitHubURL, opts.RegToken)
	if err != nil {
		return nil, err
	}
	if auth.UseV2Flow {
		return nil, fmt.Errorf("tenant uses the v2/broker flow; classic registration is not applicable")
	}

	key, err := generateKey()
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}

	pools, err := listPools(ctx, auth.URL, auth.Token)
	if err != nil {
		return nil, err
	}
	pool, err := pickPool(pools)
	if err != nil {
		return nil, err
	}

	name := opts.Name
	if name == "" {
		name = randomName()
	}

	modulus, exponent := publicParams(&key.PublicKey)
	labels := []agentLabel{
		{Name: "self-hosted", Type: "system"},
		{Name: "Linux", Type: "system"},
		{Name: "X64", Type: "system"},
	}
	for _, l := range opts.Labels {
		labels = append(labels, agentLabel{Name: l, Type: "user"})
	}

	agent := taskAgent{
		Name:           name,
		Version:        runnerVersion,
		OSDescription:  "tabrunner (wasm)",
		MaxParallelism: 1,
		Ephemeral:      opts.Ephemeral,
		DisableUpdate:  true,
		Labels:         labels,
		Authorization: agentAuthorization{
			PublicKey: &publicKey{Exponent: exponent, Modulus: modulus},
		},
	}

	created, err := registerAgent(ctx, auth.URL, auth.Token, pool.ID, agent)
	if err != nil {
		return nil, err
	}

	s := Settings{
		AgentID:          created.ID,
		AgentName:        created.Name,
		PoolID:           pool.ID,
		ServerURL:        auth.URL,
		GitHubURL:        opts.GitHubURL,
		UseV2Flow:        false,
		Ephemeral:        opts.Ephemeral,
		ClientID:         created.Authorization.ClientID,
		AuthorizationURL: created.Authorization.AuthorizationURL,
	}
	if err := saveState(settingsDir, s, key); err != nil {
		return nil, fmt.Errorf("persist runner state: %w", err)
	}
	return &s, nil
}
