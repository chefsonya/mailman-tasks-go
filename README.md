# Mailman Tasks for Go

[![Go Reference](https://pkg.go.dev/badge/github.com/chefsonya/mailman-tasks-go.svg)](https://pkg.go.dev/github.com/chefsonya/mailman-tasks-go)

Go 1.21+ client for Mailman's hosted Tasks API. Version 0.1.0; standard
library only. See the [developer page](https://getmailman.app/developers) and
[API guide](https://getmailman.app/tasks-api.md).

## Install

```sh
go get github.com/chefsonya/mailman-tasks-go@v0.1.0
```

## Quick start

```go
package main

import (
    "context"
    "fmt"
    "log"
    "os"
    "time"

    mailmantasks "github.com/chefsonya/mailman-tasks-go"
)

func main() {
    ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
    defer cancel()

    client := mailmantasks.New(os.Getenv("MAILMAN_TASKS_KEY"),
        mailmantasks.WithGroup("signup-bot"))
    defer client.Close()

    if err := client.Verify(ctx); err != nil {
        log.Fatal(err)
    }
    // Create before triggering the email. Run suits flows where the email
    // is triggered independently; use CreateTask then Wait to control ordering.
    result, err := client.Run(ctx, mailmantasks.CodeTask(
        "alias@example.com", "noreply@example.com", "verification", 6))
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println(result.Results["code"])
}
```

The key comes from Mailman → Settings → API & agents → Tasks API.
Mailman must be open, signed in, licensed and connected on the user's computer.
Keys are sent only in the Authorization header. Redirects are not followed.

## API and concurrency

- `New(apiKey, opts...) *Client`: `WithGroup` (default `"default"`),
  `WithBaseURL` (tests/advanced use only), `WithHTTPClient`, `WithUserAgent`.
- `Verify(ctx) error`: verify the key and desktop connectivity.
- `CreateTask(ctx, MailTask) (string, error)`: defaults receivedAt to now in
  **Unix seconds**, timeout to **300 seconds**, and group to the client's group.
  A nonempty task group overrides the client group. Set `WithGroup("")` for
  ungrouped tasks. Timeouts above 3600 are clamped to 3600.
- `Wait(ctx, id, opts...) (TaskResult, error)`: shares one polling goroutine
  per group. Created IDs retain their own group and creation-based deadline:
  task timeout + 15 seconds. For an external ID, use `WithWaitGroup` and
  `WithDeadline` (or `WithTimeout`); otherwise the default is 315 seconds.
  `WithTimeout` starts when that option is constructed. The context also
  bounds the wait. Wait returns the wire result, including error/timeout status.
- `Run(ctx, task) (TaskResult, error)`: create then wait; returns a successful
  result or typed task failure/timeout.
- `Poll(ctx, group) ([]TaskResult, error)`: one destructive HTTP request;
  store **every** returned result immediately. The caller owns low-level error
  handling. Results are also copied into the client's result store. The caller
  context controls whether the request starts (including waiting for pacing).
  Once sent, Poll waits for the response under its own 15-second timeout;
  caller cancellation does not abort the request or discard its results.
- `Close()`: cancels all requests, joins pollers, wakes waiters, and closes
  the client. Unconsumed responses in flight are discarded.
- `CodeTask(email, from, subjectIncludes, length)`: from/subject includes
  filters and a body digit regex; result key `code`.
- `LinkTask(email, from, subjectIncludes)`: same filters and a body
  `a[href*='/login']` selector with link formatter; result key `loginUrl`.

Concurrent Run/Wait calls use one goroutine per group; it starts with the first
waiter. When the last waiter leaves or reaches its deadline, it returns
immediately; the poller starts no new requests, lets any in-flight poll finish,
stores all its results for later waits, then stops. In-flight polling uses the
client's lifetime context, which only Close cancels, plus its own HTTP timeout.
Every completed HTTP attempt is followed
by at least 3 seconds (1 second after a full page of 100), including failures.
Retry-After and backoff can only increase that delay. Wait keeps polling through
401 (about 10 seconds apart), 429, 408, 503 and transport failures until its
deadline. Other HTTP failures return immediately.

Create retries only explicit 429/503 responses, up to **three total attempts**.
Pending-limit 429 is never retried. A dropped connection, reset or client
timeout is never retried on create: the task may already exist.
Backoff is 1, 2, 4 … 30 seconds with positive jitter; Retry-After is respected.

Destructive polling uses a non-replayable empty request body so Go's standard
HTTP transport cannot silently retry a GET on a reused connection. This also
works with caller-supplied HTTP clients; custom transports must respect the
non-replayable body and must not add their own hidden retry loop.

Every HTTP request has its own 15-second total context timeout. Verify and
create also honor caller cancellation and deadlines while in flight; destructive
polls honor the caller context only before starting. A caller-supplied HTTP
client's shorter transport timeout still applies. Close cancels every request.
The default User-Agent is `mailman-tasks-go/0.1.0`.

Results, including IDs with no current waiter, stay in a per-group store for
one hour. Retention is bounded to **10,000 results across the client**; oldest
entries are evicted first. Task group/deadline metadata is retained for at least one hour plus 15 seconds,
and always while its task is live, a waiter is active, or its result is retained.
Tracking has 10,000 slots per client, including in-flight creates. When full,
CreateTask returns `*TooManyTasksError` **before sending**; accepted tasks' metadata
is never evicted to make room. Slots become reusable after safe expiry.
Expiry is lazy. Repeated Wait calls can read retained
results; explicit deadlines still apply to cached reads. Low-level Poll still
returns only what this particular server request consumed.

Use a distinct group per independent client or bot. Independent consumers of
the same group split the server's results, even if they run on another machine.

## Errors and limits

Use `errors.As` with `*AuthError`, `*InvalidRequestError`,
`*PayloadTooLargeError`, `*RateLimitError`, `*PendingLimitError`,
`*ServerBusyError` or `*RequestTimeoutError`. These unwrap to `*MailmanError`,
which exposes `Status`, `Message` and `RetryAfter` (a `time.Duration`).
The 401 message is: “The key was rejected, or Mailman isn't open on your computer.”

`Run` maps task status `error` to `*TaskFailedError` with `ID`,
`ErrorStatus` and `ErrorMessage`; task timeout or a lost result deadline becomes
`*TaskTimeoutError` (“lost: create it again”). Caller cancellation and caller
deadlines return context errors. `ErrClosed` indicates a closed client;
`*TooManyTasksError` indicates local tracking capacity was reached without
sending the create request. Local validation messages redact the API key while
preserving their typed error.

Transport and response-read failures return `*NetworkError`. Its `Cause` is
available through `Unwrap()`: use `errors.Is` and `errors.As` to inspect it.
`MayHaveBeenAccepted` is conservatively true for create failures; the task may
already exist, so do not retry automatically. The error message omits the cause
text to keep credentials out of messages. Caller cancellation and deadlines
remain detectable with `errors.Is(err, context.Canceled)` or
`errors.Is(err, context.DeadlineExceeded)`.

Request validation enforces: strings up to 1024 Unicode characters,
1–10 filters and elements, at most 10 orValues per filter, exact enums, positive
receivedAt, and a selector or regex for body elements. Missing values use the
client defaults above. Regex compilation happens on the desktop; invalid
regexes can finish as task errors. CodeTask JSON safely escapes regex
backslashes; choose a positive code length appropriate for your site.

Create bodies must fit in 64 KiB. The server allows 10,000 pending tasks per key,
returns at most 100 results per poll and truncates result values to 8192
characters. Rate limits are 5000 requests/minute per IP and 10,000 per key.
If a result is lost, trigger a fresh email before recreating the task:
Mailman deduplicates email use for one hour.

## Development

```sh
go vet ./...
go test -race ./...
```

Tests use httptest and short polling intervals; no credentials or live service
are needed. MIT license; see [LICENSE](LICENSE).
