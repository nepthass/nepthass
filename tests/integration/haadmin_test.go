//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

// haAdmin is a test-only, unrestricted Home Assistant client. The production
// client (internal/homeassistant/rest) deliberately cannot call arbitrary
// services; the tests need to drive the simulation scripts and read state
// independently of the code under test.
type haAdmin struct {
	base   string
	token  string
	client *http.Client
}

func newAdmin(base, token string) *haAdmin {
	return &haAdmin{base: base, token: token, client: &http.Client{Timeout: requestTimeout}}
}

func (a *haAdmin) request(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

func (a *haAdmin) status(ctx context.Context, entityID string) (int, error) {
	code, _, err := a.request(ctx, http.MethodGet, "/api/states/"+entityID, nil)
	return code, err
}

// runScript runs a script and returns once Home Assistant finished it (the
// REST service endpoint is blocking).
func (a *haAdmin) runScript(ctx context.Context, name string) error {
	code, data, err := a.request(ctx, http.MethodPost, "/api/services/script/"+name, map[string]any{})
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("script %s: HTTP %d: %s", name, code, data)
	}
	return nil
}

func (a *haAdmin) call(ctx context.Context, domain, service string, data map[string]any) error {
	code, body, err := a.request(ctx, http.MethodPost, "/api/services/"+domain+"/"+service, data)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("%s.%s: HTTP %d: %s", domain, service, code, body)
	}
	return nil
}

type rawState struct {
	State      string         `json:"state"`
	Attributes map[string]any `json:"attributes"`
}

func (a *haAdmin) state(ctx context.Context, entityID string) (rawState, error) {
	code, data, err := a.request(ctx, http.MethodGet, "/api/states/"+entityID, nil)
	if err != nil {
		return rawState{}, err
	}
	if code != http.StatusOK {
		return rawState{}, fmt.Errorf("state %s: HTTP %d", entityID, code)
	}
	var st rawState
	return st, json.Unmarshal(data, &st)
}

const (
	stateTimeout  = 10 * time.Second
	statePollStep = 100 * time.Millisecond
)

// waitForState polls Home Assistant until the entity reports want. Home
// Assistant exposes no push channel over REST and template entities update a
// moment after a service call returns, so a bounded poll is the honest tool here.
func waitForState(t *testing.T, entityID, want string) {
	t.Helper()
	deadline := time.Now().Add(stateTimeout)
	var last string
	for {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		st, err := target.admin.state(ctx, entityID)
		cancel()
		if err == nil {
			last = st.State
			if last == want {
				return
			}
		} else {
			last = "error: " + err.Error()
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: state = %q after %s, want %q", entityID, last, stateTimeout, want)
		}
		time.Sleep(statePollStep)
	}
}

// resetSimulation returns every virtual device to its baseline.
func resetSimulation(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if err := target.admin.runScript(ctx, "nepthass_dev_scenario_reset"); err != nil {
		t.Fatalf("reset simulation: %v", err)
	}
}
