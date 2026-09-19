// Package api is the server as the client sees it: version 1 of the telemetry
// API, exactly as the protocol module defines it. One method per route, the
// error envelope turned into a Go error a caller can branch on, and the door
// a companion's requests go through to its own plugin.
//
// It sends and never decides: whether to queue a failed write, whether to
// forget a token, whether to back off are the runner's calls, made on the
// error this package returns.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/pacenote-sim/protocol/wire"
)

// The routes, from the protocol.
const (
	DiscoveryPath = "/.well-known/sim-telemetry.json"
	Prefix        = "/api/v1"
	PluginPrefix  = "/plugin/"
)

// MaxAnswerBytes is the most of an answer this client reads: the reference lap
// is the largest, and it is a few hundred kilobytes at most.
const MaxAnswerBytes = 8 << 20

// Error is a non-2xx answer: the envelope's code and message, and the status
// for an answer that carried no envelope. Message is written for the driver
// and shown as it is.
type Error struct {
	Status     int
	Code       wire.Code
	Message    string
	Detail     map[string]any
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("the server answered %d", e.Status)
}

// Retryable reports whether the same request may be sent again later: rate
// limited, a server error, or no answer at all. Everything else is final.
func (e *Error) Retryable() bool {
	if e.Code.Known() {
		return e.Code.Retryable()
	}
	return e.Status >= 500 || e.Status == http.StatusTooManyRequests
}

// The errors a caller branches on with errors.Is.
var (
	// ErrUnauthorized: the token is not valid any more. Forget it and pair again.
	ErrUnauthorized = errors.New("api: this client is no longer paired")
	// ErrTooOld: the server wants a newer client. Stop uploading, say so.
	ErrTooOld = errors.New("api: this client is too old for the server")
	// ErrUnreachable: no answer came back at all.
	ErrUnreachable = errors.New("api: the server could not be reached")
	// ErrPairingDenied and ErrPairingExpired end a pairing.
	ErrPairingDenied  = errors.New("api: the pairing was refused")
	ErrPairingExpired = errors.New("api: the pairing code expired before it was approved")
)

// Is lets errors.Is match the sentinels above against an *Error.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrUnauthorized:
		return e.Code == wire.CodeUnauthorized || e.Status == http.StatusUnauthorized
	case ErrTooOld:
		return e.Code == wire.CodeClientTooOld || e.Status == http.StatusUpgradeRequired
	}
	return false
}

// Client talks to one server.
type Client struct {
	base    *url.URL
	http    *http.Client
	version string
	agent   string
	log     *slog.Logger

	mu    sync.RWMutex
	token string
	// Sleep waits between pairing polls and Now is the clock; a test replaces them.
	Sleep func(context.Context, time.Duration) error
	Now   func() time.Time
}

// New is a client for the server at address, identifying itself as this
// version. The address is a scheme and a host, as the stamp gives it.
func New(address, version string, log *slog.Logger) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(address, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("api: %q is not a server address: it needs a scheme and a host, like https://team.example", address)
	}
	u.Path, u.RawQuery, u.Fragment = "", "", ""
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Client{
		base:    u,
		http:    &http.Client{Timeout: 30 * time.Second},
		version: version,
		agent:   "Pacenote/" + version + " (" + runtime.GOOS + ")",
		log:     log,
		Sleep:   sleep,
		Now:     time.Now,
	}, nil
}

// SetToken sets the device token every authenticated call carries.
func (c *Client) SetToken(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = token
}

// Token is the device token, or empty.
func (c *Client) Token() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.token
}

// Address is the server's address.
func (c *Client) Address() string { return c.base.String() }

// Discover reads the server's public description: its name, features and limits.
func (c *Client) Discover(ctx context.Context) (wire.Discovery, error) {
	var d wire.Discovery
	err := c.call(ctx, http.MethodGet, DiscoveryPath, nil, "", &d)
	return d, err
}

// PairStart opens a pairing and returns the code the operator approves.
func (c *Client) PairStart(ctx context.Context) (wire.PairStart, error) {
	var p wire.PairStart
	err := c.call(ctx, http.MethodPost, Prefix+"/pair/start", nil, "", &p)
	return p, err
}

// PairPoll asks whether the operator has decided.
func (c *Client) PairPoll(ctx context.Context, deviceCode string) (wire.PairPoll, error) {
	var p wire.PairPoll
	err := c.call(ctx, http.MethodPost, Prefix+"/pair/poll", wire.PairPollRequest{DeviceCode: deviceCode}, "", &p)
	return p, err
}

// Pair runs the whole pairing: it opens one, shows the code, polls at the
// server's interval until the operator decides or the code expires, and on
// approval keeps the token. show is called once with the code to display.
func (c *Client) Pair(ctx context.Context, show func(wire.PairStart)) (wire.PairPoll, error) {
	start, err := c.PairStart(ctx)
	if err != nil {
		return wire.PairPoll{}, err
	}
	if show != nil {
		show(start)
	}
	interval := time.Duration(max(start.IntervalS, 1)) * time.Second
	deadline := c.Now().Add(time.Duration(max(start.ExpiresInS, 1)) * time.Second)
	for {
		if err := c.Sleep(ctx, interval); err != nil {
			return wire.PairPoll{}, err
		}
		poll, err := c.PairPoll(ctx, start.DeviceCode)
		if err != nil {
			var e *Error
			if errors.As(err, &e) && e.Retryable() {
				continue
			}
			return wire.PairPoll{}, err
		}
		switch poll.Status {
		case wire.StatusApproved:
			c.SetToken(poll.Token)
			return poll, nil
		case wire.StatusDenied:
			return poll, ErrPairingDenied
		case wire.StatusExpired:
			return poll, ErrPairingExpired
		case wire.StatusPending:
		}
		if c.Now().After(deadline) {
			return poll, ErrPairingExpired
		}
	}
}

// Me is who this token is, and which plugins are running.
func (c *Client) Me(ctx context.Context) (wire.Me, error) {
	var m wire.Me
	err := c.call(ctx, http.MethodGet, Prefix+"/me", nil, "", &m)
	return m, err
}

// PutStint creates or updates a stint. key is the idempotency key; a retry
// carries the same one.
func (c *Client) PutStint(ctx context.Context, stintID string, s wire.Stint, key string) (wire.StintResult, error) {
	var res wire.StintResult
	err := c.call(ctx, http.MethodPut, Prefix+"/stints/"+url.PathEscape(stintID), s, key, &res)
	return res, err
}

// PostLaps uploads laps to a stint.
func (c *Client) PostLaps(ctx context.Context, stintID string, batch wire.LapBatch, key string) (wire.LapBatchResult, error) {
	var res wire.LapBatchResult
	err := c.call(ctx, http.MethodPost, Prefix+"/stints/"+url.PathEscape(stintID)+"/laps", batch, key, &res)
	return res, err
}

// PutSummary replaces a stint's summary.
func (c *Client) PutSummary(ctx context.Context, stintID string, s wire.Summary, key string) error {
	var ok wire.OK
	return c.call(ctx, http.MethodPut, Prefix+"/stints/"+url.PathEscape(stintID)+"/summary", s, key, &ok)
}

// PostLive sends one live sample. It is never retried by anyone.
func (c *Client) PostLive(ctx context.Context, s wire.LiveSample, key string) error {
	return c.call(ctx, http.MethodPost, Prefix+"/live", s, key, nil)
}

// PostField relays the session field.
func (c *Client) PostField(ctx context.Context, f wire.FieldReport, key string) (wire.FieldResult, error) {
	var res wire.FieldResult
	err := c.call(ctx, http.MethodPost, Prefix+"/field", f, key, &res)
	return res, err
}

// Reference asks for the lap to compare against.
func (c *Client) Reference(ctx context.Context, sim, trackID, car, class string, scope wire.Scope) (wire.ReferenceLap, error) {
	q := url.Values{"sim": {sim}, "track_id": {trackID}}
	if car != "" {
		q.Set("car", car)
	}
	if class != "" {
		q.Set("class", class)
	}
	if scope != "" {
		q.Set("scope", string(scope))
	}
	var ref wire.ReferenceLap
	err := c.call(ctx, http.MethodGet, Prefix+"/reference?"+q.Encode(), nil, "", &ref)
	return ref, err
}

// ErrOutsideMount is what Plugin returns for a path that would leave a
// plugin's mount.
var ErrOutsideMount = errors.New("api: a companion may only reach its own plugin's routes")

// Plugin sends a request to a server plugin's own route, under
// /plugin/<name>/, with the token. It is the door a companion's Host.Do goes
// through; path is relative to the mount and may carry a query. The answer is
// returned as it came, body unread, for the companion to interpret.
func (c *Client) Plugin(ctx context.Context, name, method, path string, body io.Reader, header http.Header) (*http.Response, error) {
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "..") || strings.Contains(path, "://") || strings.HasPrefix(path, "//") {
		return nil, ErrOutsideMount
	}
	u := c.base.String() + PluginPrefix + url.PathEscape(name) + path
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, fmt.Errorf("api: %w", err)
	}
	for k, v := range header {
		req.Header[k] = append([]string(nil), v...)
	}
	c.decorate(req, "")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	return res, nil
}

// call sends one request and decodes the answer into out, or returns the
// error the answer was. A nil out accepts an empty answer.
func (c *Client) call(ctx context.Context, method, path string, body any, key string, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("api: encoding the request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, reader)
	if err != nil {
		return fmt.Errorf("api: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.decorate(req, key)
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	defer res.Body.Close() //nolint:errcheck // a read answer; nothing to do about a close error.
	raw, err := io.ReadAll(io.LimitReader(res.Body, MaxAnswerBytes))
	if err != nil {
		return fmt.Errorf("%w: reading the answer: %w", ErrUnreachable, err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return c.failure(res, raw)
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("api: the answer to %s %s was not the JSON it should be: %w", method, path, err)
	}
	return nil
}

func (c *Client) decorate(req *http.Request, key string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.agent)
	req.Header.Set("X-Client-Version", c.version)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if t := c.Token(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
}

// failure turns a non-2xx answer into an *Error, from the envelope when there
// is one and from the status alone when there is not.
func (c *Client) failure(res *http.Response, raw []byte) error {
	e := &Error{Status: res.StatusCode}
	var env wire.ErrorEnvelope
	if json.Unmarshal(raw, &env) == nil && env.Error != nil {
		e.Code, e.Message, e.Detail = env.Error.Code, env.Error.Message, env.Error.Detail
		e.RetryAfter = time.Duration(env.Error.RetryAfterS) * time.Second
	}
	if e.RetryAfter == 0 && res.Header.Get("Retry-After") != "" {
		if secs, err := time.ParseDuration(res.Header.Get("Retry-After") + "s"); err == nil {
			e.RetryAfter = secs
		}
	}
	c.log.LogAttrs(context.Background(), slog.LevelDebug, "the server refused",
		slog.String("path", res.Request.URL.Path), slog.Int("status", e.Status), slog.String("code", string(e.Code)))
	return e
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck // the context's own reason is the reason.
	case <-t.C:
		return nil
	}
}
