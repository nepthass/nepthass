//go:build integration

// Package integration runs NeptHass's real code (REST client, entity service,
// SQLite storage, audit log) against a REAL Home Assistant Container started by
// compose.dev.yaml. No Discord connection is involved and nothing is mocked.
//
// Run it (see docs/development/smoke-tests.md):
//
//	docker compose --env-file .env.dev -f compose.dev.yaml --profile test run --rm integration-tests
//
// or, with a local Go toolchain:
//
//	HA_URL=http://127.0.0.1:18123 HA_TOKEN=... go test -tags=integration -count=1 -v ./tests/integration/...
//
// Safety: the tests only touch entities named nepthass_dev_* and refuse to run
// unless the target Home Assistant has the development simulation installed,
// so pointing them at a real home does nothing.
package integration

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"
)

const (
	resetScript    = "script.nepthass_dev_scenario_reset"
	readyTimeout   = 180 * time.Second
	readyInterval  = 2 * time.Second
	requestTimeout = 10 * time.Second
)

var target struct {
	url   string
	token string
	admin *haAdmin
}

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	target.url, target.token = os.Getenv("HA_URL"), os.Getenv("HA_TOKEN")
	if target.url == "" || target.token == "" {
		fmt.Fprintln(os.Stderr, "integration tests need HA_URL and HA_TOKEN (a Long-Lived Access Token from the development Home Assistant); see docs/development/smoke-tests.md")
		return 2
	}
	target.admin = newAdmin(target.url, target.token)

	ctx, cancel := context.WithTimeout(context.Background(), readyTimeout)
	defer cancel()
	if err := waitForDevInstance(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "integration tests: ", err)
		return 2
	}
	if err := target.admin.runScript(ctx, "nepthass_dev_scenario_reset"); err != nil {
		fmt.Fprintln(os.Stderr, "integration tests: cannot reset the simulation:", err)
		return 2
	}
	if err := requireMetricUnits(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "integration tests: ", err)
		return 2
	}
	code := m.Run()

	resetCtx, resetCancel := context.WithTimeout(context.Background(), requestTimeout)
	defer resetCancel()
	if err := target.admin.runScript(resetCtx, "nepthass_dev_scenario_reset"); err != nil {
		fmt.Fprintln(os.Stderr, "integration tests: final reset failed:", err)
	}
	return code
}

// waitForDevInstance waits until Home Assistant answers and the development
// simulation is loaded, and refuses any instance without it.
func waitForDevInstance(ctx context.Context) error {
	ticker := time.NewTicker(readyInterval)
	defer ticker.Stop()
	var last error
	for {
		status, err := target.admin.status(ctx, resetScript)
		switch {
		case err == nil && status == http.StatusOK:
			return nil
		case err == nil && (status == http.StatusUnauthorized || status == http.StatusForbidden):
			return fmt.Errorf("Home Assistant rejected HA_TOKEN (HTTP %d)", status)
		case err == nil:
			last = fmt.Errorf("%s not found yet (HTTP %d)", resetScript, status)
		default:
			last = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("this does not look like the NeptHass development Home Assistant (waited %s; last problem: %v); refusing to run", readyTimeout, last)
		case <-ticker.C:
		}
	}
}
