package ghrunner

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// accessToken is the OAuth token used to authenticate against the Actions
// service for sessions, messages, and job results.
type accessToken struct {
	Token   string
	Expires time.Time
}

// tokenResponse mirrors the OAuth token endpoint reply.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

// FetchAccessToken loads the persisted runner identity from dir and exchanges
// it for an OAuth access token. Exported for CLI/debug use; pass "" for the
// default .tabrunner directory.
func FetchAccessToken(ctx context.Context, dir string) (token string, expires time.Time, err error) {
	st, err := loadState(dir)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("load runner state: %w", err)
	}
	at, err := getAccessToken(ctx, st)
	if err != nil {
		return "", time.Time{}, err
	}
	return at.Token, at.Expires, nil
}

// getAccessToken exchanges a client-assertion JWT (RS256, signed with the
// runner's private key) for an OAuth access token. This is the classic
// VssOAuthCredential flow: the agent proves possession of the registered public
// key by signing a JWT, and the token service returns a bearer token.
func getAccessToken(ctx context.Context, st *runnerState) (*accessToken, error) {
	authURL := st.Settings.AuthorizationURL
	clientID := st.Settings.ClientID
	if authURL == "" || clientID == "" {
		return nil, fmt.Errorf("missing authorizationUrl/clientId; re-register the runner")
	}

	assertion, err := buildClientAssertion(st.Key, clientID, authURL)
	if err != nil {
		return nil, fmt.Errorf("build client assertion: %w", err)
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
	form.Set("client_assertion", assertion)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "tabrunner/0.1 (+https://github.com/austenstone/tabrunner)")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return nil, fmt.Errorf("token request -> %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}

	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("token response had empty access_token")
	}
	ttl := tr.ExpiresIn
	if ttl <= 0 {
		ttl = 3600
	}
	return &accessToken{
		Token:   tr.AccessToken,
		Expires: time.Now().Add(time.Duration(ttl) * time.Second),
	}, nil
}

// buildClientAssertion constructs and signs the RS256 JWT used as the OAuth
// client assertion. Claims mirror the real runner's VssOAuthCredential: subject
// and issuer are the agent's clientId, audience is the token endpoint.
func buildClientAssertion(key *rsa.PrivateKey, clientID, audience string) (string, error) {
	now := time.Now()
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{
		"sub": clientID,
		"iss": clientID,
		"aud": audience,
		"nbf": now.Add(-1 * time.Minute).Unix(),
		"exp": now.Add(3 * time.Minute).Unix(),
		"jti": newJTI(),
	}

	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}

	signingInput := b64url(hb) + "." + b64url(cb)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func b64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func newJTI() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
