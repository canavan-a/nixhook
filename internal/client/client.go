// Package client talks to a nixhook server and loads the boxes a user has
// configured.
package client

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/canavan-a/nixhook/internal/api"
	"github.com/canavan-a/nixhook/internal/job"
)

const requestTimeout = 2 * time.Minute

// Box is one server in the client config.
type Box struct {
	URL     string `json:"url"`
	KeyFile string `json:"keyFile,omitempty"`
	// Target is the flake tag deployed when none is given on the command
	// line.
	Target string `json:"target,omitempty"`
	// Headers are sent with every request, e.g. Cloudflare Access service
	// token headers.
	Headers map[string]string `json:"headers,omitempty"`
}

// Config is the client config file, ~/.config/nixhook/boxes.json.
type Config struct {
	Boxes map[string]Box `json:"boxes"`
}

// ConfigPath is where the client config is read from. NIXHOOK_CONFIG
// overrides it.
func ConfigPath() string {
	if p := os.Getenv("NIXHOOK_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "nixhook", "boxes.json")
}

// LoadConfig reads the config at path. A missing file is an empty config.
func LoadConfig(path string) (Config, error) {
	var cfg Config
	if path == "" {
		return cfg, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Options selects a box and overrides parts of it. Empty fields fall back to
// the NIXHOOK_BOX, NIXHOOK_URL and NIXHOOK_KEY_FILE environment variables.
type Options struct {
	Box     string
	URL     string
	KeyFile string
}

// Resolve builds a client from the config file, opts and the environment.
// NIXHOOK_KEY, if set, is used as the key directly.
func Resolve(opts Options) (*Client, Box, error) {
	opts.Box = cmp.Or(opts.Box, os.Getenv("NIXHOOK_BOX"))
	opts.URL = cmp.Or(opts.URL, os.Getenv("NIXHOOK_URL"))
	opts.KeyFile = cmp.Or(opts.KeyFile, os.Getenv("NIXHOOK_KEY_FILE"))

	cfg, err := LoadConfig(ConfigPath())
	if err != nil {
		return nil, Box{}, err
	}
	var box Box
	switch {
	case opts.Box != "":
		b, ok := cfg.Boxes[opts.Box]
		if !ok {
			names := slices.Sorted(maps.Keys(cfg.Boxes))
			return nil, Box{}, fmt.Errorf("unknown box %q in %s (have: %s)", opts.Box, ConfigPath(), strings.Join(names, ", "))
		}
		box = b
	case opts.URL == "" && len(cfg.Boxes) == 1:
		for _, b := range cfg.Boxes {
			box = b
		}
	}
	box.URL = cmp.Or(opts.URL, box.URL)
	box.KeyFile = cmp.Or(opts.KeyFile, box.KeyFile)
	if box.URL == "" {
		return nil, Box{}, errors.New("no box selected: pass --box or --url, or set NIXHOOK_URL")
	}

	key := os.Getenv("NIXHOOK_KEY")
	if key == "" {
		if box.KeyFile == "" {
			return nil, Box{}, errors.New("no key: set keyFile for the box, pass --key-file, or set NIXHOOK_KEY")
		}
		b, err := os.ReadFile(expandHome(box.KeyFile))
		if err != nil {
			return nil, Box{}, fmt.Errorf("read key: %w", err)
		}
		key = strings.TrimSpace(string(b))
	}
	return &Client{
		URL:     strings.TrimRight(box.URL, "/"),
		Key:     key,
		Headers: box.Headers,
		HTTP:    &http.Client{},
	}, box, nil
}

// Client calls one nixhook server.
type Client struct {
	URL     string
	Key     string
	Headers map[string]string
	HTTP    *http.Client
}

// APIError is a non-2xx response.
type APIError struct {
	Status int
	Body   api.ErrorResponse
}

func (e *APIError) Error() string {
	if e.Body.Error != "" {
		return fmt.Sprintf("%s (HTTP %d)", e.Body.Error, e.Status)
	}
	return fmt.Sprintf("HTTP %d %s", e.Status, http.StatusText(e.Status))
}

// Deploy starts a deploy.
func (c *Client) Deploy(ctx context.Context, req api.DeployRequest) (*api.DeployResponse, error) {
	var resp api.DeployResponse
	return &resp, c.do(ctx, "POST", "/v1/deploy", req, &resp)
}

// Status describes the box.
func (c *Client) Status(ctx context.Context) (*api.Status, error) {
	var st api.Status
	return &st, c.do(ctx, "GET", "/v1/status", nil, &st)
}

// Targets lists the flake tags the box will deploy.
func (c *Client) Targets(ctx context.Context) ([]string, error) {
	var resp api.TargetsResponse
	return resp.Targets, c.do(ctx, "GET", "/v1/targets", nil, &resp)
}

// Jobs lists recent jobs, newest first.
func (c *Client) Jobs(ctx context.Context, limit int) ([]*job.Job, error) {
	var jobs []*job.Job
	return jobs, c.do(ctx, "GET", "/v1/jobs?limit="+strconv.Itoa(limit), nil, &jobs)
}

// Job fetches one job. id may be "latest".
func (c *Client) Job(ctx context.Context, id string) (*job.Job, error) {
	var j job.Job
	return &j, c.do(ctx, "GET", "/v1/jobs/"+url.PathEscape(id), nil, &j)
}

// Event is one server-sent event from a log stream.
type Event struct {
	Type string
	ID   string
	Data string
}

// Stream reads a job's log stream from offset, calling fn for each event. It
// returns nil when the server ends the stream, which after a "done" event
// means the job finished and otherwise means the connection was cut.
func (c *Client) Stream(ctx context.Context, id string, offset int64, follow bool, fn func(Event) error) error {
	q := url.Values{"offset": {strconv.FormatInt(offset, 10)}}
	if follow {
		q.Set("follow", "1")
	}
	req, err := c.newRequest(ctx, "GET", "/v1/jobs/"+url.PathEscape(id)+"/logs?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readError(resp)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var ev Event
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if ev.Type != "" || ev.Data != "" {
				if err := fn(ev); err != nil {
					return err
				}
			}
			ev = Event{}
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			ev.Type = value
		case "id":
			ev.ID = value
		case "data":
			if ev.Data != "" {
				ev.Data += "\n"
			}
			ev.Data += value
		}
	}
	return sc.Err()
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return readError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

func readError(resp *http.Response) error {
	e := &APIError{Status: resp.StatusCode}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if json.Unmarshal(b, &e.Body) != nil {
		e.Body.Error = strings.TrimSpace(string(b))
	}
	return e
}

func expandHome(path string) string {
	rest, ok := strings.CutPrefix(path, "~/")
	if !ok {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, rest)
}
