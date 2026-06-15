package ghrunner

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const sessionAPIVersion = "5.1-preview.1"

// messageAPIVersion is what the real runner sends on the messages long-poll.
// GitHub uses it (plus the status/runnerVersion/os/architecture query params)
// to decide our agent is eligible for dispatch. Sending 5.1 leaves jobs queued.
const messageAPIVersion = "6.0-preview.1"

// taskAgentSession is the request/response body for creating a message session.
// The server encrypts a fresh AES key with the agent's REGISTERED public key
// (from Phase 1) and returns it in encryptionKey.
type taskAgentSession struct {
	SessionID         string                `json:"sessionId,omitempty"`
	OwnerName         string                `json:"ownerName"`
	Agent             taskAgent             `json:"agent"`
	UseFipsEncryption bool                  `json:"useFipsEncryption"`
	EncryptionKey     *taskAgentSessionKey  `json:"encryptionKey,omitempty"`
}

// taskAgentSessionKey carries the AES key. When Encrypted is true, Value is the
// AES key RSA-OAEP-encrypted with our public key and must be unwrapped with our
// private key before use.
type taskAgentSessionKey struct {
	Encrypted bool   `json:"encrypted"`
	Value     []byte `json:"value"`
}

// taskAgentMessage is one long-poll message. When IV is present, Body is
// AES-CBC encrypted (base64) and must be decrypted with the session AES key.
type taskAgentMessage struct {
	MessageID   int64  `json:"messageId"`
	MessageType string `json:"messageType"`
	IV          []byte `json:"iv"`
	Body        string `json:"body"`
}

// session holds an active message session and its decrypted AES key.
type session struct {
	st        *runnerState
	token     string
	id        string
	aesKey    []byte
	lastMsgID int64
}

// CreateSession opens a message session against the agent's pool and decrypts
// the AES key the server wrapped with our registered public key.
func CreateSession(ctx context.Context, st *runnerState, token string) (*session, error) {
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

	u := apiURL(st.Settings.ServerURL,
		fmt.Sprintf("/_apis/distributedtask/pools/%d/sessions?api-version=%s",
			st.Settings.PoolID, sessionAPIVersion))

	var out taskAgentSession
	if _, err := doJSON(ctx, http.MethodPost, u, "Bearer "+token, sessionAPIVersion, body, &out); err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	if out.SessionID == "" {
		return nil, fmt.Errorf("create session: server returned empty sessionId")
	}

	s := &session{st: st, token: token, id: out.SessionID, lastMsgID: 0}

	if out.EncryptionKey != nil && len(out.EncryptionKey.Value) > 0 {
		if out.EncryptionKey.Encrypted {
			// Unwrap the AES key with our private key. octodemo is non-FIPS, so
			// the server uses RSA-OAEP-SHA1 (FIPS would be SHA256).
			key, err := rsa.DecryptOAEP(sha1.New(), rand.Reader, st.Key, out.EncryptionKey.Value, nil)
			if err != nil {
				// Fall back to SHA256 in case the pool negotiated FIPS.
				key, err = rsa.DecryptOAEP(sha256.New(), rand.Reader, st.Key, out.EncryptionKey.Value, nil)
				if err != nil {
					return nil, fmt.Errorf("unwrap session key: %w", err)
				}
			}
			s.aesKey = key
		} else {
			s.aesKey = out.EncryptionKey.Value
		}
	}

	return s, nil
}

// Next long-polls for the next message, decrypting the body when encrypted.
// It returns the message type and the plaintext body. The caller should ack the
// message with Delete once it has been handled.
func (s *session) Next(ctx context.Context) (msgType string, body string, msgID int64, err error) {
	u := apiURL(s.st.Settings.ServerURL,
		fmt.Sprintf("/_apis/distributedtask/pools/%d/messages?sessionId=%s&lastMessageId=%d&status=Online&runnerVersion=%s&os=Linux&architecture=X64&disableUpdate=true&api-version=%s",
			s.st.Settings.PoolID, s.id, s.lastMsgID, runnerVersion, messageAPIVersion))

	// The long-poll holds the connection open (~50s server-side). pollClient has
	// no client-level deadline, so this per-request context is the only cap.
	reqCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	var msg taskAgentMessage
	resp, err := doJSONWithClient(reqCtx, pollClient, http.MethodGet, u, "Bearer "+s.token, messageAPIVersion, nil, &msg)
	if err != nil {
		return "", "", 0, err
	}
	// A 200 with no message (server timed out the long-poll with no work) decodes
	// into a zero-value struct; treat empty type as "no message".
	if resp.StatusCode == http.StatusNoContent || msg.MessageType == "" {
		return "", "", 0, nil
	}

	plaintext := msg.Body
	if len(msg.IV) > 0 {
		if len(s.aesKey) == 0 {
			return "", "", 0, fmt.Errorf("encrypted message but no session key")
		}
		pt, derr := aesCBCDecrypt(s.aesKey, msg.IV, msg.Body)
		if derr != nil {
			return "", "", 0, fmt.Errorf("decrypt message body: %w", derr)
		}
		plaintext = pt
	}

	s.lastMsgID = msg.MessageID
	return msg.MessageType, plaintext, msg.MessageID, nil
}

// Delete acks a message so the server stops redelivering it.
func (s *session) Delete(ctx context.Context, msgID int64) error {
	u := apiURL(s.st.Settings.ServerURL,
		fmt.Sprintf("/_apis/distributedtask/pools/%d/messages/%d?sessionId=%s&api-version=%s",
			s.st.Settings.PoolID, msgID, s.id, sessionAPIVersion))
	if _, err := doJSON(ctx, http.MethodDelete, u, "Bearer "+s.token, sessionAPIVersion, nil, nil); err != nil {
		return fmt.Errorf("delete message: %w", err)
	}
	return nil
}

// Close tears down the session so the agent stops showing as connected.
func (s *session) Close(ctx context.Context) error {
	u := apiURL(s.st.Settings.ServerURL,
		fmt.Sprintf("/_apis/distributedtask/pools/%d/sessions/%s?api-version=%s",
			s.st.Settings.PoolID, s.id, sessionAPIVersion))
	if _, err := doJSON(ctx, http.MethodDelete, u, "Bearer "+s.token, sessionAPIVersion, nil, nil); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// ID returns the active session id.
func (s *session) ID() string { return s.id }

// Listen runs the full long-poll message loop for an already-registered runner:
// it mints an access token, creates a message session, then dispatches each
// decrypted message to handler until ctx is cancelled. The session is torn down
// cleanly on exit so the agent stops showing as connected. Pass "" for the
// default .tabrunner directory. Tokens are refreshed automatically (~50m life).
func Listen(ctx context.Context, dir string, handler func(msgType, body string, msgID int64) error) error {
	st, err := loadState(dir)
	if err != nil {
		return fmt.Errorf("load runner state: %w", err)
	}

	token, expires, err := FetchAccessToken(ctx, dir)
	if err != nil {
		return fmt.Errorf("fetch access token: %w", err)
	}

	sess, err := CreateSession(ctx, st, token)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	fmt.Printf("session created (id %s); listening for jobs...\n", sess.ID())

	// Tear down with a fresh context so Close still runs after ctx is cancelled.
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if cerr := sess.Close(closeCtx); cerr != nil {
			fmt.Printf("session close: %v\n", cerr)
		} else {
			fmt.Println("session closed")
		}
	}()

	for {
		if ctx.Err() != nil {
			return nil
		}

		// Refresh the token before it expires (tokens last ~50m).
		if time.Until(expires) < 5*time.Minute {
			t, exp, rerr := FetchAccessToken(ctx, dir)
			if rerr != nil {
				return fmt.Errorf("refresh access token: %w", rerr)
			}
			token, expires = t, exp
			sess.token = token
		}

		msgType, body, msgID, err := sess.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Transient long-poll failure: back off briefly and retry.
			fmt.Printf("poll error: %v\n", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if msgType == "" {
			// Server returned an empty long-poll result; pause briefly so a
			// fast-returning server can't spin this loop.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(1500 * time.Millisecond):
			}
			continue
		}

		// github.com dispatches via the broker: the classic messages endpoint
		// only ever returns BrokerMigration. Ack it, drop the classic session,
		// and hand off to the broker listen loop, which polls for real jobs.
		if msgType == brokerMigrationType {
			brokerBase, perr := parseBrokerBase(body)
			if perr != nil {
				return fmt.Errorf("parse broker migration: %w", perr)
			}
			if derr := sess.Delete(ctx, msgID); derr != nil {
				fmt.Printf("ack broker migration %d: %v\n", msgID, derr)
			}
			fmt.Printf("broker migration -> %s\n", brokerBase)
			return listenBroker(ctx, dir, st, token, expires, brokerBase, sess.aesKey, handler)
		}

		if herr := handler(msgType, body, msgID); herr != nil {
			fmt.Printf("handler error: %v\n", herr)
		}
		if derr := sess.Delete(ctx, msgID); derr != nil {
			fmt.Printf("ack message %d: %v\n", msgID, derr)
		}
	}
}

// aesCBCDecrypt decrypts a base64 ciphertext with AES-CBC and strips PKCS7
// padding, matching the runner's DecryptMessage (default Aes.Create mode).
func aesCBCDecrypt(key, iv []byte, b64Body string) (string, error) {
	ct, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64Body))
	if err != nil {
		return "", fmt.Errorf("base64 body: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("aes cipher: %w", err)
	}
	if len(iv) != block.BlockSize() {
		return "", fmt.Errorf("iv length %d != block size %d", len(iv), block.BlockSize())
	}
	if len(ct) == 0 || len(ct)%block.BlockSize() != 0 {
		return "", fmt.Errorf("ciphertext length %d not a multiple of block size", len(ct))
	}
	mode := cipher.NewCBCDecrypter(block, iv)
	pt := make([]byte, len(ct))
	mode.CryptBlocks(pt, ct)

	unpadded, err := pkcs7Unpad(pt, block.BlockSize())
	if err != nil {
		return "", err
	}
	return string(unpadded), nil
}

func pkcs7Unpad(b []byte, blockSize int) ([]byte, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("empty plaintext")
	}
	pad := int(b[len(b)-1])
	if pad == 0 || pad > blockSize || pad > len(b) {
		return nil, fmt.Errorf("invalid padding")
	}
	for _, c := range b[len(b)-pad:] {
		if int(c) != pad {
			return nil, fmt.Errorf("invalid padding bytes")
		}
	}
	return b[:len(b)-pad], nil
}
