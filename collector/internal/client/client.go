// Package client reads a remote collector's HTTP API.
//
// It implements query.Source, which is what lets the display commands run
// either on the collector box (reading SQLite directly) or from a laptop
// pointed at `-remote`, with no difference in what they render.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"vpsm-collector/internal/protocol"
	"vpsm-collector/internal/query"
)

// Client talks to one collector.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

var _ query.Source = (*Client)(nil)

// New returns a client for the collector at base (e.g.
// "https://vpsm.example.com"). token may be empty for a collector serving
// anonymously.
func New(base, token string) (*Client, error) {
	if !strings.Contains(base, "://") {
		// A bare host:port is the likeliest thing to be typed; assume the
		// same default the server uses rather than failing on it.
		base = "http://" + base
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("parse collector url %q: %w", base, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("collector url %q has no host", base)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")

	return &Client{
		base:  u,
		token: token,
		http:  &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// Overview returns every host the remote collector knows about.
func (c *Client) Overview(ctx context.Context, history int) ([]query.HostStatus, error) {
	var body struct {
		Hosts []query.HostStatus `json:"hosts"`
	}
	if err := c.get(ctx, c.url("/hosts", history), &body); err != nil {
		return nil, err
	}
	return body.Hosts, nil
}

// Host returns one host from the remote collector.
func (c *Client) Host(ctx context.Context, id string, history int) (*query.HostStatus, error) {
	var host query.HostStatus
	if err := c.get(ctx, c.url("/hosts/"+url.PathEscape(id), history), &host); err != nil {
		return nil, err
	}
	return &host, nil
}

func (c *Client) url(path string, history int) string {
	u := *c.base
	u.Path = c.base.Path + "/" + protocol.Version + path
	u.RawQuery = url.Values{"history": {strconv.Itoa(history)}}.Encode()
	return u.String()
}

func (c *Client) get(ctx context.Context, endpoint string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("contact collector: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return c.statusError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("decode collector response: %w", err)
	}
	return nil
}

// statusError turns a non-200 into something an operator can act on, using
// the server's own error message when it sent one.
func (c *Client) statusError(resp *http.Response) error {
	if resp.StatusCode == http.StatusNotFound {
		return query.ErrNoHost
	}

	// The collector sends protocol.Error on every non-2xx; decoding it
	// rather than the bare status gives the operator the actual reason.
	var body protocol.Error
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	_ = json.Unmarshal(raw, &body)

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return errors.New("collector rejected the token: set -token or VPSM_TOKEN")
	case body.Error != "":
		return fmt.Errorf("collector returned %s (%s): %s", resp.Status, body.Code, body.Error)
	default:
		return fmt.Errorf("collector returned %s", resp.Status)
	}
}
