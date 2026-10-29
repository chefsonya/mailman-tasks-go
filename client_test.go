package mailmantasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	c := New("secret-key", WithBaseURL(s.URL))
	c.timing = timing{time.Millisecond * 8, time.Millisecond * 2, time.Millisecond * 12, time.Millisecond * 2}
	t.Cleanup(c.Close)
	return c
}
func task() MailTask                              { return CodeTask("a@example.com", "sender", "verify", 6) }
func jsonOut(w http.ResponseWriter, v any)        { _ = json.NewEncoder(w).Encode(v) }
func page(w http.ResponseWriter, r ...TaskResult) { jsonOut(w, map[string]any{"tasks": r}) }
func success(id string) TaskResult {
	return TaskResult{ID: id, Status: StatusSuccess, Results: map[string]string{"code": "123456"}}
}

func TestDefaultsHelpersAndEncoding(t *testing.T) {
	var got MailTask
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Token secret-key" || r.Header.Get("User-Agent") != "mailman-tasks-go/0.1.0" {
			t.Error("headers")
		}
		switch r.URL.Path {
		case "/api/v1/tasks/mail/create":
			_ = json.NewDecoder(r.Body).Decode(&got)
			jsonOut(w, TaskResult{ID: "id", Status: StatusPending})
		case "/api/v1/tasks/completed":
			if r.URL.Query().Get("group") != "a &?#/+" {
				t.Error("group encoding")
			}
			page(w)
		default:
			jsonOut(w, map[string]any{})
		}
	})
	c.group = "a &?#/+"
	if err := c.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateTask(context.Background(), task()); err != nil {
		t.Fatal(err)
	}
	if got.Timeout != 300 || got.ReceivedAt < time.Now().Unix()-2 || got.Group != c.group || got.MailElements[0].Regex != `\b\d{6}\b` {
		t.Fatalf("%+v", got)
	}
	if _, err := c.Poll(context.Background(), c.group); err != nil {
		t.Fatal(err)
	}
	link := LinkTask("a", "b", "c")
	if link.MailElements[0].Selector != "a[href*='/login']" || link.MailElements[0].Formatter != FormatterLink {
		t.Fatal(link)
	}
}

func TestValidation(t *testing.T) {
	cases := []struct {
		name   string
		change func(*MailTask)
	}{
		{"blank email", func(x *MailTask) { x.Email = " " }},
		{"unicode length", func(x *MailTask) { x.Email = strings.Repeat("?", 1025) }},
		{"group", func(x *MailTask) { x.Group = strings.Repeat("x", 1025) }},
		{"template", func(x *MailTask) { x.TemplateID = strings.Repeat("x", 1025) }},
		{"received", func(x *MailTask) { x.ReceivedAt = -1 }},
		{"no filters", func(x *MailTask) { x.MailFilters = nil }},
		{"many filters", func(x *MailTask) { x.MailFilters = make([]MailFilter, 11) }},
		{"no elements", func(x *MailTask) { x.MailElements = nil }},
		{"many elements", func(x *MailTask) { x.MailElements = make([]MailElement, 11) }},
		{"target", func(x *MailTask) { x.MailFilters[0].Target = TargetBody }},
		{"comparator", func(x *MailTask) { x.MailFilters[0].Comparator = "contains" }},
		{"empty value", func(x *MailTask) { x.MailFilters[0].Value = "" }},
		{"or count", func(x *MailTask) { x.MailFilters[0].OrValues = make([]string, 11) }},
		{"or length", func(x *MailTask) { x.MailFilters[0].OrValues = []string{strings.Repeat("x", 1025)} }},
		{"replace length", func(x *MailTask) { x.MailFilters[0].RegexReplaceAll = strings.Repeat("x", 1025) }},
		{"name", func(x *MailTask) { x.MailElements[0].Name = "" }},
		{"element target", func(x *MailTask) { x.MailElements[0].Target = "invalid" }},
		{"body", func(x *MailTask) { x.MailElements[0].Regex = "" }},
		{"source", func(x *MailTask) { x.MailElements[0].RegexSource = "invalid" }},
		{"formatter", func(x *MailTask) { x.MailElements[0].Formatter = "invalid" }},
		{"regex length", func(x *MailTask) { x.MailElements[0].Regex = strings.Repeat("x", 1025) }},
	}
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid request sent") })
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x := task()
			tc.change(&x)
			_, err := c.CreateTask(context.Background(), x)
			var e *InvalidRequestError
			if !errors.As(err, &e) {
				t.Fatalf("%T %v", err, err)
			}
		})
	}
	x := task()
	x.Email = strings.Repeat("?", 1024)
	x.Timeout = 99999
	x = c.normalize(x)
	if err := validate(x); err != nil || x.Timeout != 3600 {
		t.Fatalf("%v %+v", err, x)
	}
}

func TestHTTPErrorTypesAndCreateRetry(t *testing.T) {
	for _, code := range []int{400, 401, 408, 413, 429, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var calls atomic.Int32
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(code)
				jsonOut(w, map[string]string{"error": "message"})
			})
			_, err := c.CreateTask(context.Background(), task())
			var base *MailmanError
			if !errors.As(err, &base) || base.Status != code {
				t.Fatalf("%T %v", err, err)
			}
			ok := false
			switch code {
			case 400:
				var e *InvalidRequestError
				ok = errors.As(err, &e)
			case 401:
				var e *AuthError
				ok = errors.As(err, &e) && strings.Contains(e.Message, "Mailman isn't open")
			case 408:
				var e *RequestTimeoutError
				ok = errors.As(err, &e)
			case 413:
				var e *PayloadTooLargeError
				ok = errors.As(err, &e)
			case 429:
				var e *RateLimitError
				ok = errors.As(err, &e)
			case 503:
				var e *ServerBusyError
				ok = errors.As(err, &e)
			}
			expected := int32(1)
			if code == 429 || code == 503 {
				expected = 3
			}
			if !ok || calls.Load() != expected {
				t.Fatalf("type %T calls %d", err, calls.Load())
			}
		})
	}
	t.Run("pending", func(t *testing.T) {
		var calls atomic.Int32
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(429)
			jsonOut(w, map[string]string{"error": "pending task limit exceeded"})
		})
		_, err := c.CreateTask(context.Background(), task())
		var e *PendingLimitError
		if !errors.As(err, &e) || calls.Load() != 1 {
			t.Fatal(err, calls.Load())
		}
	})
}
func TestRetryAfterCreate(t *testing.T) {
	for _, status := range []int{429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			var first time.Time
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					first = time.Now()
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(status)
					jsonOut(w, map[string]string{"error": "busy"})
					return
				}
				if time.Since(first) < time.Second {
					t.Error("Retry-After violated")
				}
				jsonOut(w, TaskResult{ID: "ok"})
			})
			if _, err := c.CreateTask(context.Background(), task()); err != nil || calls.Load() != 2 {
				t.Fatal(err, calls.Load())
			}
		})
	}
}
func TestCreateBackoffAtLeastRetryAfter(t *testing.T) {
	var mu sync.Mutex
	var times []time.Time
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		times = append(times, time.Now())
		n := len(times)
		mu.Unlock()
		if n <= 2 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(503)
			jsonOut(w, map[string]string{"error": "server busy"})
			return
		}
		jsonOut(w, TaskResult{ID: "ok"})
	})
	c.timing.backoff = 600 * time.Millisecond
	if _, err := c.CreateTask(context.Background(), task()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(times) != 3 {
		t.Fatalf("%d attempts", len(times))
	}
	if gap := times[1].Sub(times[0]); gap < time.Second {
		t.Fatalf("first retry after %v; Retry-After is 1s", gap)
	}
	// The second backoff (2 x 600ms) is longer than Retry-After, so it wins.
	if gap := times[2].Sub(times[1]); gap < 1200*time.Millisecond {
		t.Fatalf("second retry after %v; backoff is 1.2s", gap)
	}
}
func TestDroppedConnection(t *testing.T) {
	for _, create := range []bool{true, false} {
		t.Run(fmt.Sprint(create), func(t *testing.T) {
			var calls atomic.Int32
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				page(w, success("a"))
			})
			if create {
				_, err := c.CreateTask(context.Background(), task())
				if err == nil || calls.Load() != 1 {
					t.Fatal(err, calls.Load())
				}
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				r, err := c.Wait(ctx, "a")
				if err != nil || r.ID != "a" || calls.Load() < 2 {
					t.Fatal(r, err, calls.Load())
				}
			}
		})
	}
}
func TestSharedPollerPagesAndEarlyResult(t *testing.T) {
	var calls atomic.Int32
	var active atomic.Int32
	var overlap atomic.Bool
	var mu sync.Mutex
	var times []time.Time
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if active.Add(1) != 1 {
			overlap.Store(true)
		}
		defer active.Add(-1)
		n := calls.Add(1)
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
		switch n {
		case 1:
			results := make([]TaskResult, 100)
			for i := range results {
				results[i] = success(fmt.Sprint(i))
			}
			results[0] = success("early")
			page(w, results...)
		case 2:
			page(w, success("a"), success("b"))
		default:
			page(w)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, id := range []string{"a", "b"} {
		go func(id string) {
			<-start
			r, err := c.Wait(ctx, id)
			if err == nil && r.ID != id {
				err = fmt.Errorf("wrong result %s", r.ID)
			}
			errs <- err
		}(id)
	}
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	before := calls.Load()
	r, err := c.Wait(ctx, "early")
	if err != nil || r.ID != "early" || calls.Load() != before || overlap.Load() {
		t.Fatal(r, err, calls.Load(), overlap.Load())
	}
	mu.Lock()
	if len(times) < 2 || times[1].Sub(times[0]) < c.timing.full {
		t.Fatal("full page pace")
	}
	mu.Unlock()
	// The API is destructive; no result may be fetched twice.
	remaining, err := c.Poll(ctx, "default")
	if err != nil || len(remaining) != 0 {
		t.Fatal(remaining, err)
	}
}

func TestWaitRecoveryAndPacing(t *testing.T) {
	for _, status := range []int{401, 408, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			var first time.Time
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					first = time.Now()
					if status == 429 || status == 503 {
						w.Header().Set("Retry-After", "1")
					}
					w.WriteHeader(status)
					jsonOut(w, map[string]string{"error": "temporary"})
					return
				}
				min := time.Duration(0)
				if status == 429 || status == 503 {
					min = time.Second
				}
				if status == 401 {
					min = 12 * time.Millisecond
				}
				if time.Since(first) < min {
					t.Error("poll pace")
				}
				page(w, success("a"))
			})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if _, err := c.Wait(ctx, "a"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestWaitDeadlineCancellationAndClose(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { page(w) })
	_, err := c.Wait(context.Background(), "a", WithDeadline(time.Now().Add(-time.Second)))
	var timeout *TaskTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatal(err)
	}
	_, err = c.Wait(context.Background(), "a", WithTimeout(time.Millisecond*5))
	if !errors.As(err, &timeout) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Wait(ctx, "a")
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Millisecond*5)
	defer cancel()
	_, err = c.Wait(ctx, "a")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := c.Wait(context.Background(), "close"); done <- err }()
	time.Sleep(5 * time.Millisecond)
	c.Close()
	if err := <-done; !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestRunStatusesAndCreatedDeadline(t *testing.T) {
	for _, status := range []string{StatusSuccess, StatusError, StatusTimeout} {
		t.Run(status, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					jsonOut(w, TaskResult{ID: "a", Status: StatusPending})
					return
				}
				page(w, TaskResult{ID: "a", Status: status, Results: map[string]string{"errorStatus": "invalid mail task", "errorMessage": "bad regex"}})
			})
			r, err := c.Run(context.Background(), task())
			switch status {
			case StatusSuccess:
				if err != nil || r.Status != status {
					t.Fatal(r, err)
				}
			case StatusError:
				var e *TaskFailedError
				if !errors.As(err, &e) || e.ErrorStatus != "invalid mail task" {
					t.Fatal(err)
				}
			case StatusTimeout:
				var e *TaskTimeoutError
				if !errors.As(err, &e) {
					t.Fatal(err)
				}
			}
		})
	}
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { page(w) })
	c.tasks["lost"] = taskInfo{"other", time.Now().Add(-time.Second), time.Now().Add(time.Hour)}
	_, err := c.Wait(context.Background(), "lost")
	var e *TaskTimeoutError
	if !errors.As(err, &e) {
		t.Fatal(err)
	}
}
func TestCacheExpiryAndBound(t *testing.T) {
	c := New("key")
	defer c.Close()
	c.cache["g"] = map[string]cached{"expired": {success("expired"), time.Now().Add(-time.Second)}}
	for i := 0; i < 10002; i++ {
		id := fmt.Sprint(i)
		c.cache["g"][id] = cached{success(id), time.Now().Add(time.Hour)}
	}
	c.pruneLocked()
	if len(c.cache["g"]) != 10000 {
		t.Fatal(len(c.cache["g"]))
	}
	if _, ok := c.cache["g"]["expired"]; ok {
		t.Fatal("expired retained")
	}
}
func TestKeyRedactionAndRedirect(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		jsonOut(w, map[string]string{"error": "oops secret-key"})
	})
	if err := c.Verify(context.Background()); err == nil || strings.Contains(err.Error(), "secret-key") {
		t.Fatal(err)
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("redirect followed") }))
	defer target.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer s.Close()
	redirected := New("key", WithBaseURL(s.URL))
	defer redirected.Close()
	var e *MailmanError
	if err := redirected.Verify(context.Background()); !errors.As(err, &e) || e.Status != 302 {
		t.Fatal(err)
	}
}

func TestPollMinimumIncludesErrorsAndDirectCalls(t *testing.T) {
	for _, status := range []int{401, 408, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			var mu sync.Mutex
			var attempts []time.Time
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				attempts = append(attempts, time.Now())
				mu.Unlock()
				if calls.Add(1) == 1 {
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(status)
					jsonOut(w, map[string]string{"error": "temporary"})
					return
				}
				page(w, success("a"))
			})
			// Simulate 3s minimum > Retry-After 1s without slowing every test.
			c.timing.poll = 1100 * time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if _, err := c.Wait(ctx, "a"); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Poll(ctx, "default"); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(attempts) != 3 {
				t.Fatal(attempts)
			}
			for i := 1; i < len(attempts); i++ {
				if attempts[i].Sub(attempts[i-1]) < c.timing.poll {
					t.Fatalf("poll attempts only %v apart", attempts[i].Sub(attempts[i-1]))
				}
			}
		})
	}
}

func TestResultsSurviveLaterPollFailure(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			page(w, success("early"))
			return
		}
		w.WriteHeader(400)
		jsonOut(w, map[string]string{"error": "invalid group query"})
	})
	_, err := c.Wait(context.Background(), "missing", WithTimeout(time.Second))
	var invalid *InvalidRequestError
	if !errors.As(err, &invalid) {
		t.Fatal(err)
	}
	r, err := c.Wait(context.Background(), "early")
	if err != nil || r.ID != "early" || calls.Load() != 2 {
		t.Fatal(r, err, calls.Load())
	}
	// Direct Poll also immediately stores results for subsequent Wait.
	c2 := testClient(t, func(w http.ResponseWriter, r *http.Request) { page(w, success("direct")) })
	if _, err := c2.Poll(context.Background(), "default"); err != nil {
		t.Fatal(err)
	}
	if r, err := c2.Wait(context.Background(), "direct"); err != nil || r.ID != "direct" {
		t.Fatal(r, err)
	}
}
func TestTaskOwnGroupAndDeadline(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			var task MailTask
			_ = json.NewDecoder(r.Body).Decode(&task)
			if task.Group != "own & group" || task.Timeout != 7 {
				t.Errorf("create %+v", task)
			}
			jsonOut(w, TaskResult{ID: "a", Status: StatusPending})
			return
		}
		if r.URL.Query().Get("group") != "own & group" {
			t.Error(r.URL)
		}
		page(w, success("a"))
	})
	x := task()
	x.Group = "own & group"
	x.Timeout = 7
	r, err := c.Run(context.Background(), x)
	if err != nil || r.ID != "a" {
		t.Fatal(r, err)
	}
	c.mu.Lock()
	deadline := c.tasks["a"].deadline
	c.mu.Unlock()
	if d := time.Until(deadline); d < 21*time.Second || d > 22*time.Second {
		t.Fatal(d)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEveryHTTPRequestHasTimeoutAndCreateNeverRetriesAmbiguity(t *testing.T) {
	for _, method := range []string{"verify", "create", "poll"} {
		t.Run(method, func(t *testing.T) {
			var calls atomic.Int32
			client := New("key", WithHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				deadline, ok := r.Context().Deadline()
				if !ok || time.Until(deadline) > 15*time.Second {
					t.Error("request missing bounded deadline")
				}
				// Simulates a client-side request timeout after bytes may have been sent.
				return nil, context.DeadlineExceeded
			})}))
			defer client.Close()
			ctx := context.Background()
			var err error
			switch method {
			case "verify":
				err = client.Verify(ctx)
			case "create":
				_, err = client.CreateTask(ctx, task())
			case "poll":
				_, err = client.Poll(ctx, "default")
			}
			if err == nil || calls.Load() != 1 {
				t.Fatal(err, calls.Load())
			}
		})
	}
}

func TestCreateClientTimeoutDoesNotRetry(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(30 * time.Millisecond)
	})
	c.http = &http.Client{Timeout: 10 * time.Millisecond}
	_, err := c.CreateTask(context.Background(), task())
	if err == nil || calls.Load() != 1 {
		t.Fatal(err, calls.Load())
	}
}
func TestResultMapsAreIndependent(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { page(w, success("a")) })
	results, err := c.Poll(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	results[0].Results["code"] = "changed"
	first, err := c.Wait(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if first.Results["code"] != "123456" {
		t.Fatal(first)
	}
	first.Results["code"] = "changed again"
	second, err := c.Wait(context.Background(), "a")
	if err != nil || second.Results["code"] != "123456" {
		t.Fatal(second, err)
	}
}

func TestPollReusedConnectionDoesNotReplay(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(fmt.Sprintf("custom-client=%v", custom), func(t *testing.T) {
			var calls atomic.Int32
			var mu sync.Mutex
			var verifiedConn string
			var attempts []time.Time
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/user/verify" {
					mu.Lock()
					verifiedConn = r.RemoteAddr
					mu.Unlock()
					jsonOut(w, map[string]any{})
					return
				}
				n := calls.Add(1)
				mu.Lock()
				attempts = append(attempts, time.Now())
				if n == 1 && r.RemoteAddr != verifiedConn {
					t.Error("poll did not reuse verified connection")
				}
				mu.Unlock()
				if n == 1 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				page(w, success("a"))
			})
			if custom {
				transport := http.DefaultTransport.(*http.Transport).Clone()
				defer transport.CloseIdleConnections()
				c.http = &http.Client{Transport: transport, Timeout: time.Second}
			}
			c.timing.poll = 30 * time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := c.Verify(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Poll(ctx, "default"); err == nil {
				t.Fatal("dropped poll was replayed inside http.Client.Do")
			}
			if calls.Load() != 1 {
				t.Fatalf("one Poll made %d HTTP attempts", calls.Load())
			}
			// Dispatcher retries must use the same pace gate as the failed direct poll.
			r, err := c.Wait(ctx, "a")
			if err != nil || r.ID != "a" {
				t.Fatal(r, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(attempts) != 2 {
				t.Fatal(attempts)
			}
			if elapsed := attempts[1].Sub(attempts[0]); elapsed < c.timing.poll {
				t.Fatalf("retry after %v; minimum %v", elapsed, c.timing.poll)
			}
		})
	}
}

func TestTrackingLimitPreservesMetadata(t *testing.T) {
	var creates atomic.Int32
	var polls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			n := creates.Add(1)
			jsonOut(w, TaskResult{ID: fmt.Sprint(n), Status: StatusPending})
			return
		}
		polls.Add(1)
		if r.URL.Query().Get("group") != "own" {
			t.Error("wrong group", r.URL)
		}
		page(w, success("1"))
	})
	c.trackingLimit = 2
	first := task()
	first.Group = "own"
	id, err := c.CreateTask(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Poll(context.Background(), "own"); err != nil {
		t.Fatal(err)
	}
	second := task()
	second.Group = "pending"
	pending, err := c.CreateTask(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	original := c.tasks[id]
	outstanding := c.tasks[pending]
	c.mu.Unlock()
	for i := 0; i < 3; i++ {
		if _, err := c.CreateTask(context.Background(), task()); !isTooManyTasks(err) {
			t.Fatal(err)
		}
	}
	if creates.Load() != 2 {
		t.Fatalf("capacity rejection sent create: %d", creates.Load())
	}
	c.mu.Lock()
	if c.tasks[id] != original || c.tasks[pending] != outstanding {
		t.Error("metadata changed under capacity pressure")
	}
	c.mu.Unlock()
	result, err := c.Wait(context.Background(), id)
	if err != nil || result.ID != id || polls.Load() != 1 {
		t.Fatal(result, err, polls.Load())
	}

	// Expiry of metadata alone must not orphan a still-retained result,
	// or forget a live task's original group and deadline.
	c.mu.Lock()
	info := c.tasks[id]
	info.expires = time.Now().Add(-time.Hour)
	info.deadline = time.Now().Add(-time.Second)
	c.tasks[id] = info
	live := c.tasks[pending]
	live.expires = time.Now().Add(-time.Hour)
	c.tasks[pending] = live
	c.pruneLocked()
	_, keptResult := c.tasks[id]
	_, keptLive := c.tasks[pending]
	c.mu.Unlock()
	if !keptResult || !keptLive {
		t.Fatal("pruning forgot retained result or live task")
	}
	if result, err := c.Wait(context.Background(), id, WithDeadline(time.Now().Add(time.Second))); err != nil || result.ID != id {
		t.Fatal(result, err)
	}
	_, err = c.Wait(context.Background(), id)
	var timeout *TaskTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatal("original deadline forgotten", err)
	}

	// Expired, inactive tasks without a retained result can release capacity.
	c.mu.Lock()
	entry := c.cache["own"][id]
	entry.expires = time.Now().Add(-time.Second)
	c.cache["own"][id] = entry
	live.deadline = time.Now().Add(-time.Second)
	c.tasks[pending] = live
	c.mu.Unlock()
	if _, err := c.CreateTask(context.Background(), task()); err != nil {
		t.Fatal("expired tracking slots not released", err)
	}
}

func TestTrackingReservationConcurrentCreate(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		jsonOut(w, TaskResult{ID: "reserved", Status: StatusPending})
	})
	c.trackingLimit = 1
	done := make(chan error, 1)
	go func() { _, err := c.CreateTask(context.Background(), task()); done <- err }()
	<-entered
	_, err := c.CreateTask(context.Background(), task())
	close(release)
	if !isTooManyTasks(err) {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("concurrent create bypassed reservation", calls.Load())
	}
	c.mu.Lock()
	tracked := len(c.tasks)
	reserved := c.creating
	c.mu.Unlock()
	if tracked != 1 || reserved != 0 {
		t.Fatal(tracked, reserved)
	}
}
func TestFailedCreateReleasesTrackingReservation(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(400)
			jsonOut(w, map[string]string{"error": "invalid task"})
			return
		}
		jsonOut(w, TaskResult{ID: "a", Status: StatusPending})
	})
	c.trackingLimit = 1
	if _, err := c.CreateTask(context.Background(), task()); err == nil {
		t.Fatal("expected failure")
	}
	if _, err := c.CreateTask(context.Background(), task()); err != nil {
		t.Fatal(err)
	}
}
func TestLocalValidationRedactsKey(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	for _, field := range []string{"comparator", "filter target", "element target", "source", "formatter"} {
		t.Run(field, func(t *testing.T) {
			x := task()
			switch field {
			case "comparator":
				x.MailFilters[0].Comparator = c.key
			case "filter target":
				x.MailFilters[0].Target = c.key
			case "element target":
				x.MailElements[0].Target = c.key
			case "source":
				x.MailElements[0].RegexSource = c.key
			case "formatter":
				x.MailElements[0].Formatter = c.key
			}
			_, err := c.CreateTask(context.Background(), x)
			var invalid *InvalidRequestError
			var base *MailmanError
			if !errors.As(err, &invalid) || !errors.As(err, &base) || base.Status != 400 {
				t.Fatalf("%T %v", err, err)
			}
			if strings.Contains(err.Error(), c.key) || strings.Contains(base.Message, c.key) || !strings.Contains(base.Message, "[redacted]") {
				t.Fatal("validation key redaction failed")
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("invalid tasks were sent")
	}
}

func TestInFlightPollSurvivesLastWaiterDeparture(t *testing.T) {
	for _, mode := range []string{"context cancellation", "task deadline"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			// Registered before the server/client cleanups, and called explicitly on
			// failures, so the fixture cannot leave a server handler blocked.
			defer unblock()
			var calls atomic.Int32
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					close(started)
				}
				select {
				case <-release:
					page(w, success("late"))
				case <-r.Context().Done():
					return
				}
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			opts := []WaitOption{}
			if mode == "task deadline" {
				opts = append(opts, WithDeadline(time.Now().Add(time.Second)))
			}
			returned := make(chan error, 1)
			go func() { _, err := c.Wait(ctx, "departing", opts...); returned <- err }()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				unblock()
				t.Fatal("poll did not start")
			}
			c.mu.Lock()
			p := c.groups["default"]
			c.mu.Unlock()
			if p == nil {
				unblock()
				t.Fatal("missing active poller")
			}
			if mode == "context cancellation" {
				cancel()
			}
			select {
			case err := <-returned:
				if mode == "context cancellation" {
					if !errors.Is(err, context.Canceled) {
						unblock()
						t.Fatal(err)
					}
				} else {
					var timeout *TaskTimeoutError
					if !errors.As(err, &timeout) {
						unblock()
						t.Fatal(err)
					}
				}
			case <-time.After(3 * time.Second):
				unblock()
				t.Fatal("wait did not return while the HTTP response was blocked")
			}
			c.mu.Lock()
			remaining := len(p.waiters)
			c.mu.Unlock()
			if remaining != 0 {
				unblock()
				t.Fatal("departing waiter remains registered")
			}

			// Only now allow the response containing a different task's code.
			unblock()
			select {
			case <-p.done:
			case <-time.After(3 * time.Second):
				t.Fatal("idle poller did not finish")
			}
			c.mu.Lock()
			entry, stored := c.cache["default"]["late"]
			c.mu.Unlock()
			if !stored {
				t.Fatal("late response result was lost")
			}
			if ttl := time.Until(entry.expires); ttl < 59*time.Minute || ttl > time.Hour {
				t.Fatal("incorrect retention", ttl)
			}
			result, err := c.Wait(context.Background(), "late", WithTimeout(time.Second))
			if err != nil || result.Results["code"] != "123456" {
				t.Fatal(result, err)
			}
			if calls.Load() != 1 {
				t.Fatalf("later wait made another destructive poll: %d", calls.Load())
			}
			c.mu.Lock()
			groups := len(c.groups)
			c.mu.Unlock()
			if groups != 0 {
				t.Fatal("idle poller restarted")
			}
		})
	}
}

func TestDirectPollCallerCancellationOnlyBoundsAdmission(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
			page(w, success("direct-late"))
		case <-r.Context().Done():
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.Poll(ctx, "default"); done <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		unblock()
		t.Fatal("poll did not start")
	}
	cancel()
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("poll did not finish")
	}
	if r, err := c.Wait(context.Background(), "direct-late", WithTimeout(time.Second)); err != nil || r.ID != "direct-late" {
		t.Fatal(r, err)
	}
}

func TestCloseCancelsInFlightPoll(t *testing.T) {
	started := make(chan struct{})
	aborted := make(chan struct{})
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(aborted)
	})
	returned := make(chan error, 1)
	go func() { _, err := c.Wait(context.Background(), "a"); returned <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("poll did not start")
	}
	c.Close()
	select {
	case <-aborted:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel the request")
	}
	select {
	case err := <-returned:
		if !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not wake waiter")
	}
	c.mu.Lock()
	count := len(c.cache)
	c.mu.Unlock()
	if count != 0 {
		t.Fatal("Close retained an in-flight response")
	}
}

func isTooManyTasks(err error) bool {
	var capacity *TooManyTasksError
	return errors.As(err, &capacity)
}
