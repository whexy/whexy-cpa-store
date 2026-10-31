package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	loginLifetime      = 10 * time.Minute
	loginStateLength   = 24
	finishedLoginGrace = time.Minute
)

type rpcAuthLoginStartRequest struct {
	pluginapi.AuthLoginStartRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcAuthLoginPollRequest struct {
	pluginapi.AuthLoginPollRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// signInResult is the decrypted native-app callback.
type signInResult struct {
	UserID      string
	AccessToken string
}

// loginFlow is one pending browser sign-in.
type loginFlow struct {
	mu        sync.Mutex
	key       *rsa.PrivateKey
	server    *http.Server
	closeOnce sync.Once
	systemID  string
	expiresAt time.Time
	result    *signInResult
	// signedIn is the finished sign-in, kept for finishedLoginGrace so a
	// repeated poll, or a retry after a failed save, gets the same account.
	signedIn *pluginapi.AuthLoginPollResponse
}

// close stops the callback listener and any connection it already accepted.
// sync.Once makes callback, poll, paste and expiry safe to race.
func (f *loginFlow) close() {
	f.closeOnce.Do(func() {
		if f.server != nil {
			_ = f.server.Close()
		}
	})
}

type loginFlows struct {
	mu    sync.Mutex
	flows map[string]*loginFlow
	now   func() time.Time
}

var logins = &loginFlows{flows: make(map[string]*loginFlow), now: time.Now}

func handleLoginStart(raw []byte) ([]byte, error) {
	var req rpcAuthLoginStartRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode login start request: %w", errUnmarshal)
	}
	resp, errStart := logins.start(currentSettings())
	if errStart != nil {
		return errorEnvelope("login_start_failed", "start Zed login: "+errStart.Error(), http.StatusBadGateway), nil
	}
	return okEnvelope(resp)
}

func handleLoginPoll(raw []byte) ([]byte, error) {
	var req rpcAuthLoginPollRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode login poll request: %w", errUnmarshal)
	}
	h := newHost(req.HostCallbackID)
	resp := logins.poll(context.Background(), h, currentSettings(), strings.TrimSpace(req.State))
	return okEnvelope(resp)
}

// start opens a loopback listener for Zed's native-app callback and returns
// the sign-in URL.
func (f *loginFlows) start(cfg settings) (pluginapi.AuthLoginStartResponse, error) {
	key, errKey := rsa.GenerateKey(rand.Reader, 2048)
	if errKey != nil {
		return pluginapi.AuthLoginStartResponse{}, errKey
	}
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("open the sign-in callback port: %w", errListen)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	publicKey := base64.RawURLEncoding.EncodeToString(x509.MarshalPKCS1PublicKey(&key.PublicKey))
	systemID := randomUUID()
	query := url.Values{}
	query.Set("native_app_port", fmt.Sprintf("%d", port))
	query.Set("native_app_public_key", publicKey)
	query.Set("system_id", systemID)
	signInURL := cfg.SignInURL + "?" + query.Encode()

	state, errState := randomState()
	if errState != nil {
		_ = listener.Close()
		return pluginapi.AuthLoginStartResponse{}, errState
	}
	flow := &loginFlow{key: key, systemID: systemID, expiresAt: f.now().Add(loginLifetime)}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if strings.TrimSpace(query.Get("user_id")) == "" || strings.TrimSpace(query.Get("access_token")) == "" {
			// A probe or a stray request must not end the pending sign-in.
			http.NotFound(w, r)
			return
		}
		result, errCallback := decryptCallback(flow.key, query)
		if errCallback != nil {
			// Keep waiting: a bad callback must not poison the legitimate one.
			http.Error(w, "invalid Zed callback", http.StatusBadRequest)
			return
		}
		// The redirect must reach the browser before anything closes this
		// connection, including a poll that sees the result.
		w.Header().Set("Location", "https://zed.dev/native_app_signin_succeeded")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusFound)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		flow.mu.Lock()
		if flow.result == nil {
			flow.result = &result
		}
		flow.mu.Unlock()
		flow.close()
	})}
	flow.server = server
	f.mu.Lock()
	f.pruneLocked(f.now())
	f.flows[state] = flow
	f.mu.Unlock()
	time.AfterFunc(loginLifetime+finishedLoginGrace, func() { f.expire(state, flow) })
	go func() { _ = server.Serve(listener) }()

	return pluginapi.AuthLoginStartResponse{
		Provider:  providerKey,
		URL:       signInURL,
		State:     state,
		ExpiresAt: flow.expiresAt,
		Metadata: map[string]any{
			"callback_port": port,
			"system_id":     systemID,
		},
	}, nil
}

func randomState() (string, error) {
	var b [loginStateLength]byte
	if _, errRead := rand.Read(b[:]); errRead != nil {
		return "", errRead
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// decryptCallback turns the browser's encrypted access_token into the
// credential Zed would store.
func decryptCallback(key *rsa.PrivateKey, query url.Values) (signInResult, error) {
	userID := strings.TrimSpace(query.Get("user_id"))
	encrypted := strings.TrimSpace(query.Get("access_token"))
	if userID == "" || encrypted == "" {
		return signInResult{}, errors.New("the Zed callback carried no user_id or access_token")
	}
	ciphertext, errDecode := base64.RawURLEncoding.DecodeString(encrypted)
	if errDecode != nil {
		var errPadded error
		ciphertext, errPadded = base64.URLEncoding.DecodeString(encrypted)
		if errPadded != nil {
			return signInResult{}, fmt.Errorf("decode the Zed access token: %w", errDecode)
		}
	}
	plain, errDecrypt := rsa.DecryptOAEP(sha256.New(), rand.Reader, key, ciphertext, nil)
	if errDecrypt != nil {
		plain, errDecrypt = rsa.DecryptPKCS1v15(rand.Reader, key, ciphertext)
		if errDecrypt != nil {
			return signInResult{}, fmt.Errorf("decrypt the Zed access token: %w", errDecrypt)
		}
	}
	return signInResult{UserID: userID, AccessToken: string(plain)}, nil
}

// completeFromCallback lets a remote user paste the callback URL their
// browser landed on, which the loopback listener on this host cannot receive.
func (f *loginFlows) completeFromCallback(state, callbackURL string) error {
	parsed, errParse := url.Parse(strings.TrimSpace(callbackURL))
	if errParse != nil {
		return fmt.Errorf("parse the callback URL: %w", errParse)
	}
	flow := f.find(state)
	if flow == nil {
		return errors.New("the sign-in has expired; start it again")
	}
	result, errCallback := decryptCallback(flow.key, parsed.Query())
	if errCallback != nil {
		return errCallback
	}
	flow.mu.Lock()
	if flow.result == nil {
		flow.result = &result
	}
	flow.mu.Unlock()
	flow.close()
	return nil
}

func (f *loginFlows) find(state string) *loginFlow {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.flows[state]
}

func (f *loginFlows) poll(ctx context.Context, h host, cfg settings, state string) pluginapi.AuthLoginPollResponse {
	flow := f.find(state)
	if flow == nil {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "the Zed sign-in expired or was never started"}
	}
	flow.mu.Lock()
	result, signedIn := flow.result, flow.signedIn
	flow.mu.Unlock()
	if signedIn != nil {
		return *signedIn
	}
	if result == nil {
		if f.now().After(flow.expiresAt.Add(finishedLoginGrace)) {
			flow.close()
			return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "the Zed sign-in expired; start it again"}
		}
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusPending, Message: "waiting for the browser sign-in"}
	}
	cred := newCredential(result.UserID, result.AccessToken, flow.systemID, "", "", "")
	info, errInfo := fetchAccountInfo(ctx, h, cfg, cred)
	if errInfo != nil {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "read the signed-in Zed account: " + errInfo.Error()}
	}
	cred.Username = info.Username
	cred.Plan = info.Plan
	cred.OrganizationID = info.OrganizationID
	file := authFile{
		UserID:         cred.UserID,
		AccessToken:    cred.AccessToken,
		SystemID:       cred.SystemID,
		OrganizationID: cred.OrganizationID,
		Username:       cred.Username,
		Plan:           cred.Plan,
		AddedAt:        time.Now().UTC().Format(time.RFC3339),
		extra:          make(map[string]json.RawMessage),
	}
	storage, errMarshal := file.marshal()
	if errMarshal != nil {
		return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "store the Zed login: " + errMarshal.Error()}
	}
	flow.close()
	resp := pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusSuccess, Auth: file.authData(fileNameFor("", file), storage)}
	f.finish(state, flow, resp)
	return resp
}

// finish keeps the signed-in account for finishedLoginGrace, then drops the
// flow.
func (f *loginFlows) finish(state string, flow *loginFlow, resp pluginapi.AuthLoginPollResponse) {
	flow.mu.Lock()
	flow.signedIn = &resp
	flow.mu.Unlock()
	time.AfterFunc(finishedLoginGrace, func() { f.expire(state, flow) })
}

// expire drops one flow and stops its listener.
func (f *loginFlows) expire(state string, flow *loginFlow) {
	flow.close()
	f.mu.Lock()
	if f.flows[state] == flow {
		delete(f.flows, state)
	}
	f.mu.Unlock()
}

// closeAll stops every pending listener; plugin shutdown calls it so no
// login server outlives the library.
func (f *loginFlows) closeAll() {
	f.mu.Lock()
	flows := f.flows
	f.flows = make(map[string]*loginFlow)
	f.mu.Unlock()
	for _, flow := range flows {
		flow.close()
	}
}

// pruneLocked removes flows past their grace period.
func (f *loginFlows) pruneLocked(now time.Time) {
	for state, flow := range f.flows {
		flow.mu.Lock()
		expired := now.After(flow.expiresAt.Add(finishedLoginGrace))
		flow.mu.Unlock()
		if expired {
			flow.close()
			delete(f.flows, state)
		}
	}
}
