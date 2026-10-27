// Package rest is a minimal Home Assistant REST API client.
//
// It authenticates with a long-lived access token, applies request timeouts,
// never follows redirects (so the token cannot be forwarded elsewhere), and
// can only invoke the explicitly allowlisted services.
package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/nepthass/nepthass/internal/config"
	"github.com/nepthass/nepthass/internal/entities"
)

const (
	maxResponseBytes = 16 << 20
	dialTimeout      = 5 * time.Second
)

// allowedServices is the complete list of Home Assistant services NeptHass
// may call. Anything else is refused before a request is made.
var allowedServices = map[string]struct{}{
	"light.turn_on":   {},
	"light.turn_off":  {},
	"switch.turn_on":  {},
	"switch.turn_off": {},
}

// Client talks to one Home Assistant instance.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New builds a client from validated configuration.
func New(cfg config.HomeAssistant) *Client {
	transport := &http.Transport{
		// Home Assistant is local: never route the token through an
		// environment-configured proxy.
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: dialTimeout}).DialContext,
		ResponseHeaderTimeout: cfg.Timeout,
		MaxIdleConns:          4,
		IdleConnTimeout:       time.Minute,
	}
	return &Client{
		base:  cfg.URL,
		token: cfg.Token.Reveal(),
		http: &http.Client{
			Transport:     transport,
			Timeout:       cfg.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Info is the subset of /api/config that NeptHass reports.
type Info struct {
	Version string `json:"version"`
}

// Info validates connectivity and the access token and returns server details.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var info Info
	err := c.do(ctx, "get config", http.MethodGet, "/api/config", nil, &info)
	return info, err
}

// State fetches one entity.
func (c *Client) State(ctx context.Context, id entities.ID) (entities.State, error) {
	var dto stateDTO
	if err := c.do(ctx, "get state", http.MethodGet, "/api/states/"+url.PathEscape(string(id)), nil, &dto); err != nil {
		return entities.State{}, err
	}
	return dto.toState()
}

// States fetches every entity. Entries with malformed IDs are skipped.
func (c *Client) States(ctx context.Context) ([]entities.State, error) {
	var dtos []stateDTO
	if err := c.do(ctx, "list states", http.MethodGet, "/api/states", nil, &dtos); err != nil {
		return nil, err
	}
	states := make([]entities.State, 0, len(dtos))
	for _, dto := range dtos {
		if state, err := dto.toState(); err == nil {
			states = append(states, state)
		}
	}
	return states, nil
}

// Control turns an entity on or off. Only allowlisted domain and service
// pairs are sent; everything else returns ErrServiceNotAllowed without any
// network traffic.
func (c *Client) Control(ctx context.Context, id entities.ID, action entities.Action) error {
	domain := string(id.Domain())
	service := action.Service()
	if _, ok := allowedServices[domain+"."+service]; !ok {
		return fmt.Errorf("%w: %s.%s", ErrServiceNotAllowed, domain, service)
	}
	body := map[string]string{"entity_id": string(id)}
	return c.do(ctx, "call service", http.MethodPost, "/api/services/"+domain+"/"+service, body, nil)
}

func (c *Client) do(ctx context.Context, op, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		payload, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("%s: encode request: %w", op, err)
		}
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return fmt.Errorf("%s: build request: %w", op, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w: %w", op, ErrUnavailable, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%s: %w", op, ErrUnauthorized)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%s: %w", op, ErrNotFound)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return &APIError{Op: op, Status: resp.StatusCode}
	case out == nil:
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); err != nil {
		return fmt.Errorf("%s: decode response: %w", op, err)
	}
	return nil
}
