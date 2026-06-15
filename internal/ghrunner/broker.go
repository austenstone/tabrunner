package ghrunner

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// brokerMigrationType is the messageType the classic pipelines long-poll returns
// to tell us this tenant has moved to the Actions broker. Modern GitHub no longer
// dispatches jobs on the classic endpoint -- every poll returns this until we
// create a session against the broker and poll there instead.
const brokerMigrationType = "BrokerMigration"

// brokerMigration is the body of a BrokerMigration message.
type brokerMigration struct {
	BrokerBaseURL string `json:"brokerBaseUrl"`
}

// parseBrokerBase pulls the absolute broker base URL out of a BrokerMigration
// message body.
func parseBrokerBase(body string) (string, error) {
	var bm brokerMigration
	if err := json.Unmarshal([]byte(body), &bm); err != nil {
		return "", err
	}
	if bm.BrokerBaseURL == "" {
		return "", fmt.Errorf("empty brokerBaseUrl")
	}
	return bm.BrokerBaseURL, nil
}

// brokerSession is an active session against the Actions broker. Unlike the
// classic pool session, the broker exposes flat /session, /message and
// /acknowledge routes off the absolute brokerBaseUrl with NO api-version query.
type brokerSession struct {
	token  string
	base   string // absolute broker base, no trailing slash
	id     string
	aesKey []byte
}

// brokerQuery builds the query params the broker expects on /message and
// /acknowledge (matching the real runner's BrokerHttpClient).
func (b *brokerSession) brokerQuery() url.Values {
	q := url.Values{}
	q.Set("sessionId", b.id)
	q.Set("status", "Online")
	q.Set("runnerVersion", runnerVersion)
	q.Set("os", "Linux")
	q.Set("architecture", "X64")
	q.Set("disableUpdate", "true")
	return q
}

// createBrokerSession opens a session directly against the broker. The body is
// the same TaskAgentSession shape as the classic session create, but the URL is
// the absolute {brokerBase}/session and there is no api-version negotiation. If
// the broker wraps its own AES key we unwrap it with our private key; otherwise
// we fall back to the classic session's key.
// BrokerURLRewrite, when set, rewrites the broker base URL before the session is
// created. Used by the wasm/browser build to route the no-CORS broker leg through
// a small CORS proxy. nil for native runs (direct broker access).
var BrokerURLRewrite func(string) string

func createBrokerSession(ctx context.Context, st *runnerState, token, brokerBase string, classicKey []byte) (*brokerSession, error) {
	base := strings.TrimRight(brokerBase, "/")
	if BrokerURLRewrite != nil {
		base = strings.TrimRight(BrokerURLRewrite(base), "/")
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "tabrunner"
	}
	body := taskAgentSession{
		OwnerName: fmt.Sprintf("%s (PID: %d)", host, os.Getpid()),
		Agent: taskAgent{
			ID:            st.Settings.AgentID,
			Name:          st.Settings.AgentName,
			Version:       runnerVersion,
			OSDescription: "wasm (tabrunner)",
		},
		UseFipsEncryption: false,
	}

	var out taskAgentSession
	if _, err := doJSON(ctx, http.MethodPost, base+"/session", "Bearer "+token, "", body, &out); err != nil {
		return nil, fmt.Errorf("create broker session: %w", err)
	}
	if out.SessionID == "" {
		return nil, fmt.Errorf("create broker session: server returned empty sessionId")
	}

	bs := &brokerSession{token: token, base: base, id: out.SessionID, aesKey: classicKey}
	if out.EncryptionKey != nil && len(out.EncryptionKey.Value) > 0 {
		if out.EncryptionKey.Encrypted {
			key, err := rsa.DecryptOAEP(sha1.New(), rand.Reader, st.Key, out.EncryptionKey.Value, nil)
			if err != nil {
				key, err = rsa.DecryptOAEP(sha256.New(), rand.Reader, st.Key, out.EncryptionKey.Value, nil)
				if err != nil {
					return nil, fmt.Errorf("unwrap broker session key: %w", err)
				}
			}
			bs.aesKey = key
		} else {
			bs.aesKey = out.EncryptionKey.Value
		}
	}
	return bs, nil
}

// next long-polls the broker for the next runner message, decrypting the body
// when encrypted. An empty type means the server timed out the poll with no work.
func (b *brokerSession) next(ctx context.Context) (msgType, body string, runnerRequestID int64, err error) {
	u := b.base + "/message?" + b.brokerQuery().Encode()

	reqCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	var msg taskAgentMessage
	resp, err := doJSONWithClient(reqCtx, pollClient, http.MethodGet, u, "Bearer "+b.token, "", nil, &msg)
	if err != nil {
		return "", "", 0, err
	}
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusAccepted || msg.MessageType == "" {
		return "", "", 0, nil
	}

	plaintext := msg.Body
	if len(msg.IV) > 0 {
		if len(b.aesKey) == 0 {
			return "", "", 0, fmt.Errorf("encrypted broker message but no session key")
		}
		pt, derr := aesCBCDecrypt(b.aesKey, msg.IV, msg.Body)
		if derr != nil {
			return "", "", 0, fmt.Errorf("decrypt broker message body: %w", derr)
		}
		plaintext = pt
	}
	return msg.MessageType, plaintext, msg.MessageID, nil
}

// acknowledge tells the broker we accepted a runner request so it stops
// redelivering it.
func (b *brokerSession) acknowledge(ctx context.Context, runnerRequestID string) error {
	u := b.base + "/acknowledge?" + b.brokerQuery().Encode()
	reqBody := map[string]string{"runnerRequestId": runnerRequestID}
	if _, err := doJSON(ctx, http.MethodPost, u, "Bearer "+b.token, "", reqBody, nil); err != nil {
		return fmt.Errorf("acknowledge broker request: %w", err)
	}
	return nil
}

// close deletes the broker session so the agent stops showing as connected.
func (b *brokerSession) close(ctx context.Context) error {
	q := url.Values{}
	q.Set("sessionId", b.id)
	u := b.base + "/session?" + q.Encode()
	if _, err := doJSON(ctx, http.MethodDelete, u, "Bearer "+b.token, "", nil, nil); err != nil {
		return fmt.Errorf("delete broker session: %w", err)
	}
	return nil
}

// listenBroker runs the broker message loop after a BrokerMigration redirect. It
// creates a broker session, then polls until ctx is cancelled, refreshing the
// access token as needed and dispatching each decrypted message to handler.
func listenBroker(ctx context.Context, st *runnerState, token string, expires time.Time, brokerBaseURL string, classicKey []byte, handler func(msgType, body string, msgID int64) error) error {
	bs, err := createBrokerSession(ctx, st, token, brokerBaseURL, classicKey)
	if err != nil {
		return err
	}
	fmt.Printf("broker session created (id %s); listening for jobs at %s ...\n", bs.id, bs.base)

	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if cerr := bs.close(closeCtx); cerr != nil {
			fmt.Printf("broker session close: %v\n", cerr)
		} else {
			fmt.Println("broker session closed")
		}
	}()

	for {
		if ctx.Err() != nil {
			return nil
		}

		if time.Until(expires) < 5*time.Minute {
			at, rerr := getAccessToken(ctx, st)
			if rerr != nil {
				return fmt.Errorf("refresh access token: %w", rerr)
			}
			token, expires = at.Token, at.Expires
			bs.token = token
		}

		msgType, body, reqID, err := bs.next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			fmt.Printf("broker poll error: %v\n", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if msgType == "" {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(1500 * time.Millisecond):
			}
			continue
		}
		// A second BrokerMigration would just point back here; skip it.
		if msgType == brokerMigrationType {
			continue
		}

		fmt.Printf("broker message: type=%s id=%d bytes=%d\n", msgType, reqID, len(body))
		if herr := handler(msgType, body, reqID); herr != nil {
			fmt.Printf("handler error: %v\n", herr)
		}
		if msgType == runnerJobRequestType {
			jr, perr := parseRunnerJobRequest(body)
			if perr != nil {
				fmt.Printf("parse runner job request: %v\n", perr)
				continue
			}
			if jr.ShouldAcknowledge {
				if aerr := bs.acknowledge(ctx, jr.RunnerRequestID); aerr != nil {
					fmt.Printf("acknowledge request %s: %v\n", jr.RunnerRequestID, aerr)
				} else {
					fmt.Printf("acknowledged request %s\n", jr.RunnerRequestID)
				}
			}
			job, aerr := acquireJob(ctx, jr, bs.token)
			if aerr != nil {
				fmt.Printf("acquire job: %v\n", aerr)
			} else if job != nil {
				fmt.Printf("acquired job: jobID=%s planID=%s\n", job.jobID, job.planID)
				if eerr := executeAndComplete(ctx, job, jr, bs.token); eerr != nil {
					fmt.Printf("execute/complete job: %v\n", eerr)
				}
			} else {
				fmt.Printf("acquire skipped (already taken or expired)\n")
			}
		}
	}
}
