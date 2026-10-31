package mailmantasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrClosed = errors.New("mailman tasks client is closed")

type Option func(*Client)

func WithGroup(group string) Option { return func(c *Client) { c.group = group } }
func WithBaseURL(base string) Option {
	return func(c *Client) { c.base = strings.TrimRight(base, "/") }
}
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}
func WithUserAgent(ua string) Option { return func(c *Client) { c.ua = ua } }

type timing struct{ poll, full, auth, backoff time.Duration }
type taskInfo struct {
	group             string
	deadline, expires time.Time
}
type cached struct {
	result  TaskResult
	expires time.Time
}
type delivery struct {
	result TaskResult
	err    error
}
type poller struct {
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	waiters  map[string]map[chan delivery]struct{}
	stopping bool
}
type pollGate struct {
	token chan struct{}
	next  time.Time
	refs  int
}
type Client struct {
	lifetime                context.Context
	cancel                  context.CancelFunc
	gates                   map[string]*pollGate
	trackingLimit, creating int
	key, group, base, ua    string
	http                    *http.Client
	mu                      sync.Mutex
	closed                  bool
	groups                  map[string]*poller
	tasks                   map[string]taskInfo
	cache                   map[string]map[string]cached
	timing                  timing
}

// New constructs a concurrent client. Call Close when finished.
func New(apiKey string, opts ...Option) *Client {
	c := &Client{key: apiKey, group: "default", base: "https://tasks.getmailman.app", ua: "mailman-tasks-go/" + Version,
		http: &http.Client{Timeout: 15 * time.Second}, groups: make(map[string]*poller), tasks: make(map[string]taskInfo),
		cache: make(map[string]map[string]cached), timing: timing{3 * time.Second, time.Second, 10 * time.Second, time.Second}}
	c.lifetime, c.cancel = context.WithCancel(context.Background())
	c.gates = make(map[string]*pollGate)
	c.trackingLimit = 10000
	for _, o := range opts {
		o(c)
	}
	return c
}
func (c *Client) redact(s string) string {
	if c.key != "" {
		return strings.ReplaceAll(s, c.key, "[redacted]")
	}
	return s
}
func (c *Client) request(ctx context.Context, method, path string, body []byte, out any) error {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrClosed
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	stop := context.AfterFunc(c.lifetime, cancel)
	defer stop()
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid Tasks API URL")
	}
	if method == http.MethodGet && strings.HasPrefix(path, "/api/v1/tasks/completed?") {
		// An opaque empty body with no replay function prevents net/http.Transport
		// from silently replaying a destructive GET on a reused connection.
		// Do not use http.NoBody: Transport considers that replayable.
		req.Body = io.NopCloser(strings.NewReader(""))
		req.GetBody = nil
		req.ContentLength = -1
	}
	req.Header.Set("Authorization", "Token "+c.key)
	req.Header.Set("User-Agent", c.ua)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Never follow redirects: credentials must remain on the configured host.
	h := *c.http
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := h.Do(req)
	if err != nil {
		return &NetworkError{Cause: err, MayHaveBeenAccepted: method == http.MethodPost}
	}
	defer res.Body.Close()
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		if out == nil {
			_, err = io.Copy(io.Discard, res.Body)
		} else {
			err = json.NewDecoder(res.Body).Decode(out)
		}
		if err != nil {
			var syntax *json.SyntaxError
			var shape *json.UnmarshalTypeError
			if errors.As(err, &syntax) || errors.As(err, &shape) {
				return errors.New("invalid Tasks API response")
			}
			return &NetworkError{Cause: err, MayHaveBeenAccepted: method == http.MethodPost}
		}
		return nil
	}
	var payload struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(res.Body, 65536)).Decode(&payload)
	retry := time.Duration(0)
	if n, e := strconv.ParseInt(res.Header.Get("Retry-After"), 10, 32); e == nil && n > 0 {
		retry = time.Duration(n) * time.Second
	} else if d, e := http.ParseTime(res.Header.Get("Retry-After")); e == nil {
		retry = time.Until(d)
		if retry < 0 {
			retry = 0
		}
	}
	e := &MailmanError{res.StatusCode, c.redact(payload.Error), retry}
	switch res.StatusCode {
	case 400:
		return &InvalidRequestError{e}
	case 401:
		e.Message = "The key was rejected, or Mailman isn't open on your computer."
		return &AuthError{e}
	case 408:
		return &RequestTimeoutError{e}
	case 413:
		return &PayloadTooLargeError{e}
	case 429:
		if payload.Error == "pending task limit exceeded" {
			return &PendingLimitError{e}
		}
		return &RateLimitError{e}
	case 503:
		return &ServerBusyError{e}
	}
	return e
}
func (c *Client) Verify(ctx context.Context) error {
	return c.request(ctx, "GET", "/api/v1/user/verify", nil, nil)
}
func pause(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (c *Client) backoff(attempt int) time.Duration {
	if attempt > 5 {
		attempt = 5
	}
	d := c.timing.backoff * time.Duration(1<<attempt)
	cap := 30 * c.timing.backoff
	if d > cap {
		d = cap
	}
	// Positive jitter never shortens the required delay.
	d += time.Duration(rand.Float64() * float64(d) / 5)
	if d > cap {
		d = cap
	}
	return d
}
func (c *Client) normalize(t MailTask) MailTask {
	if t.ReceivedAt == 0 {
		t.ReceivedAt = time.Now().Unix()
	}
	if t.Timeout == 0 {
		t.Timeout = 300
	}
	if t.Timeout > 3600 {
		t.Timeout = 3600
	}
	if t.Group == "" {
		t.Group = c.group
	}
	return t
}
func (c *Client) CreateTask(ctx context.Context, t MailTask) (string, error) {
	t = c.normalize(t)
	if err := validate(t); err != nil {
		var invalid *InvalidRequestError
		if errors.As(err, &invalid) {
			invalid.Message = c.redact(invalid.Message)
		}
		return "", err
	}
	body, err := json.Marshal(t)
	if err != nil {
		return "", invalid("invalid task JSON")
	}
	if len(body) > 65536 {
		return "", &PayloadTooLargeError{&MailmanError{Status: 413, Message: "request body exceeds 64 KiB"}}
	}
	// Reserve before sending, including concurrent creates and explicit retries.
	c.mu.Lock()
	c.pruneLocked()
	if c.closed {
		c.mu.Unlock()
		return "", ErrClosed
	}
	if len(c.tasks)+c.creating >= c.trackingLimit {
		c.mu.Unlock()
		return "", &TooManyTasksError{Limit: c.trackingLimit}
	}
	c.creating++
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.creating--; c.mu.Unlock() }()
	for attempt := 0; ; attempt++ {
		var result TaskResult
		started := time.Now()
		err = c.request(ctx, "POST", "/api/v1/tasks/mail/create", body, &result)
		if err == nil {
			c.mu.Lock()
			c.pruneLocked()
			c.tasks[result.ID] = taskInfo{t.Group, started.Add(time.Duration(t.Timeout+15) * time.Second), started.Add(time.Hour + 15*time.Second)}
			c.pruneLocked()
			c.mu.Unlock()
			return result.ID, nil
		}
		var api *MailmanError
		var pending *PendingLimitError
		if attempt >= 2 || errors.As(err, &pending) || !errors.As(err, &api) || (api.Status != 429 && api.Status != 503) {
			return "", err
		}
		// M9: wait the longer of the exponential backoff and Retry-After.
		delay := c.backoff(attempt)
		if api.RetryAfter > delay {
			delay = api.RetryAfter
		}
		if err = pause(ctx, delay); err != nil {
			return "", err
		}
	}
}

// Poll performs exactly one destructive request. Store every returned result.
// The caller context bounds admission only. Once admitted, the HTTP request
// finishes under its own 15-second timeout, unless Close cancels the client.
// Do not combine Poll with another consumer of the same group.
func (c *Client) Poll(ctx context.Context, group string) ([]TaskResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(c.lifetime, cancel)
	defer stop()
	// A gate covers waiting and the entire request, so even direct Poll calls
	// cannot race the dispatcher or bypass the per-group minimum.
	c.mu.Lock()
	for g, gate := range c.gates {
		if gate.refs == 0 && !time.Now().Before(gate.next) {
			delete(c.gates, g)
		}
	}
	gate := c.gates[group]
	if gate == nil {
		gate = &pollGate{token: make(chan struct{}, 1)}
		gate.token <- struct{}{}
		c.gates[group] = gate
	}
	gate.refs++
	c.mu.Unlock()
	defer func() { c.mu.Lock(); gate.refs--; c.mu.Unlock() }()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-gate.token:
	}
	defer func() { gate.token <- struct{}{} }()
	if err := pause(ctx, time.Until(gate.next)); err != nil {
		return nil, err
	}
	var page struct {
		Tasks []TaskResult `json:"tasks"`
	}
	// This is the admission boundary: caller/poller cancellation prevents
	// starting a request, but must never abort a destructive response in flight.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	err := c.request(c.lifetime, "GET", "/api/v1/tasks/completed?group="+url.QueryEscape(group), nil, &page)
	for i := range page.Tasks {
		if page.Tasks[i].Status == StatusSuccess && page.Tasks[i].Results == nil {
			page.Tasks[i].Results = make(map[string]string)
		}
	}
	delay := c.timing.poll
	if err == nil && len(page.Tasks) == 100 {
		delay = c.timing.full
	}
	var api *MailmanError
	if errors.As(err, &api) && api.RetryAfter > delay {
		delay = api.RetryAfter
	}
	c.mu.Lock()
	gate.next = time.Now().Add(delay)
	if err == nil && !c.closed {
		for _, r := range page.Tasks {
			if c.cache[group] == nil {
				c.cache[group] = make(map[string]cached)
			}
			c.cache[group][r.ID] = cached{cloneResult(r), time.Now().Add(time.Hour)}
			if p := c.groups[group]; p != nil {
				for ch := range p.waiters[r.ID] {
					select {
					case ch <- delivery{result: cloneResult(r)}:
					default:
					}
				}
			}
		}
		c.pruneLocked()
	}
	c.mu.Unlock()
	return page.Tasks, err
}

type WaitOption func(*waitOptions)
type waitOptions struct {
	group    string
	deadline time.Time
}

func WithWaitGroup(group string) WaitOption { return func(o *waitOptions) { o.group = group } }
func WithDeadline(deadline time.Time) WaitOption {
	return func(o *waitOptions) { o.deadline = deadline }
}

// WithTimeout bounds a wait from the moment the option is constructed.
func WithTimeout(timeout time.Duration) WaitOption { return WithDeadline(time.Now().Add(timeout)) }

func (c *Client) Wait(ctx context.Context, id string, opts ...WaitOption) (TaskResult, error) {
	c.mu.Lock()
	o := waitOptions{c.group, time.Now().Add(315 * time.Second)}
	if info, ok := c.tasks[id]; ok {
		o.group = info.group
		o.deadline = info.deadline
	}
	c.mu.Unlock()
	for _, opt := range opts {
		opt(&o)
	}
	timer := time.NewTimer(time.Until(o.deadline))
	defer timer.Stop()
	ch := make(chan delivery, 1)
	for {
		if err := ctx.Err(); err != nil {
			return TaskResult{}, err
		}
		if !time.Now().Before(o.deadline) {
			return TaskResult{}, &TaskTimeoutError{ID: id, Reason: "deadline"}
		}
		c.mu.Lock()
		c.pruneLocked()
		if c.closed {
			c.mu.Unlock()
			return TaskResult{}, ErrClosed
		}
		if entry, ok := c.cache[o.group][id]; ok {
			c.mu.Unlock()
			return cloneResult(entry.result), nil
		}
		p := c.groups[o.group]
		if p != nil && p.stopping {
			done := p.done
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return TaskResult{}, ctx.Err()
			case <-timer.C:
				return TaskResult{}, &TaskTimeoutError{ID: id, Reason: "deadline"}
			case <-done:
				continue
			}
		}
		if p == nil {
			p = &poller{done: make(chan struct{}), waiters: make(map[string]map[chan delivery]struct{})}
			p.ctx, p.cancel = context.WithCancel(context.Background())
			c.groups[o.group] = p
			go c.pollLoop(o.group, p)
		}
		if p.waiters[id] == nil {
			p.waiters[id] = make(map[chan delivery]struct{})
		}
		p.waiters[id][ch] = struct{}{}
		c.mu.Unlock()
		defer func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			delete(p.waiters[id], ch)
			if len(p.waiters[id]) == 0 {
				delete(p.waiters, id)
			}
			if len(p.waiters) == 0 {
				p.stopping = true
				p.cancel()
			}
		}()
		select {
		case d := <-ch:
			return d.result, d.err
		case <-ctx.Done():
			return TaskResult{}, ctx.Err()
		case <-timer.C:
			return TaskResult{}, &TaskTimeoutError{ID: id, Reason: "deadline"}
		}
	}
}
func (c *Client) pollLoop(group string, p *poller) {
	defer func() { c.mu.Lock(); delete(c.groups, group); close(p.done); c.mu.Unlock() }()
	attempt := 0
	for {
		results, err := c.Poll(p.ctx, group)
		if p.ctx.Err() != nil {
			return
		}
		delay := c.timing.poll
		if err == nil {
			attempt = 0
			if len(results) == 100 {
				delay = c.timing.full
			}
		} else {
			var api *MailmanError
			if errors.As(err, &api) {
				switch api.Status {
				case 401:
					delay = c.timing.auth
				case 429, 408, 503:
					delay = c.backoff(attempt)
					attempt++
					if api.RetryAfter > delay {
						delay = api.RetryAfter
					}
				default:
					c.mu.Lock()
					for _, ws := range p.waiters {
						for ch := range ws {
							select {
							case ch <- delivery{err: err}:
							default:
							}
						}
					}
					p.stopping = true
					p.cancel()
					c.mu.Unlock()
					return
				}
			} else {
				delay = c.backoff(attempt)
				attempt++
			}
		}
		if pause(p.ctx, delay) != nil {
			return
		}
	}
}

// Bounded metadata and result retention, with lazy expiry and oldest eviction.
func (c *Client) pruneLocked() {
	now := time.Now()
	count := 0
	for group, entries := range c.cache {
		for id, e := range entries {
			if !now.Before(e.expires) {
				delete(entries, id)
			}
		}
		if len(entries) == 0 {
			delete(c.cache, group)
		}
		count += len(entries)
	}
	for ; count > 10000; count-- {
		var g, key string
		var at time.Time
		for group, entries := range c.cache {
			for id, e := range entries {
				if at.IsZero() || e.expires.Before(at) {
					g = group
					key = id
					at = e.expires
				}
			}
		}
		delete(c.cache[g], key)
		if len(c.cache[g]) == 0 {
			delete(c.cache, g)
		}
	}
	// Metadata must outlive both its live task and any retained result.
	// Capacity pressure never evicts it: CreateTask reserves a slot instead.
	for id, t := range c.tasks {
		_, retained := c.cache[t.group][id]
		active := false
		if p := c.groups[t.group]; p != nil {
			active = len(p.waiters[id]) > 0
		}
		if !retained && !active && !now.Before(t.deadline) && !now.Before(t.expires) {
			delete(c.tasks, id)
		}
	}

}
func (c *Client) Run(ctx context.Context, t MailTask) (TaskResult, error) {
	id, err := c.CreateTask(ctx, t)
	if err != nil {
		return TaskResult{}, err
	}
	r, err := c.Wait(ctx, id)
	if err != nil {
		return TaskResult{}, err
	}
	switch r.Status {
	case StatusSuccess:
		return r, nil
	case StatusTimeout:
		return TaskResult{}, &TaskTimeoutError{ID: id, Reason: "timeout"}
	case StatusError:
		return TaskResult{}, &TaskFailedError{id, c.redact(r.Results["errorStatus"]), c.redact(r.Results["errorMessage"])}
	default:
		return TaskResult{}, errors.New("invalid completed task status")
	}
}

// Close cancels all requests, joins pollers, and wakes waiters. Results in
// flight may be discarded. It is safe to call more than once.
func (c *Client) Close() {
	c.mu.Lock()
	c.closed = true
	c.cancel()
	var done []chan struct{}
	for _, p := range c.groups {
		for _, ws := range p.waiters {
			for ch := range ws {
				select {
				case ch <- delivery{err: ErrClosed}:
				default:
				}
			}
		}
		p.stopping = true
		p.cancel()
		done = append(done, p.done)
	}
	c.mu.Unlock()
	for _, d := range done {
		<-d
	}
}

// Keep callers' mutable maps independent from retained results and other waiters.
func cloneResult(r TaskResult) TaskResult {
	if r.Results != nil {
		values := make(map[string]string, len(r.Results))
		for k, v := range r.Results {
			values[k] = v
		}
		r.Results = values
	}
	return r
}
