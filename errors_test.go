package mailmantasks

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestNetworkErrorWrapsTransportCause(t *testing.T) {
	for _, operation := range []string{"verify", "create", "poll"} {
		t.Run(operation, func(t *testing.T) {
			cause := errors.New("wire failure containing secret-key")
			calls := 0
			c := New("secret-key", WithHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, cause
			})}))
			defer c.Close()
			var err error
			switch operation {
			case "verify":
				err = c.Verify(context.Background())
			case "create":
				_, err = c.CreateTask(context.Background(), task())
			case "poll":
				_, err = c.Poll(context.Background(), "default")
			}
			var network *NetworkError
			var transport *url.Error
			if !errors.As(err, &network) || !errors.As(err, &transport) || !errors.Is(err, cause) {
				t.Fatalf("cause not preserved: %T %v", err, err)
			}
			if network.MayHaveBeenAccepted != (operation == "create") {
				t.Fatal("incorrect create acceptance flag")
			}
			if strings.Contains(err.Error(), "secret-key") {
				t.Fatal("network message exposed API key")
			}
			if network.Unwrap() != transport || calls != 1 {
				t.Fatal("transport failure retried or cause changed")
			}
		})
	}
}

type brokenResponseBody struct{ cause error }

func (b brokenResponseBody) Read([]byte) (int, error) { return 0, b.cause }
func (b brokenResponseBody) Close() error             { return nil }

func TestNetworkErrorWrapsResponseReadFailure(t *testing.T) {
	for _, operation := range []string{"verify", "create", "poll"} {
		t.Run(operation, func(t *testing.T) {
			calls := 0
			c := New("key", WithHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: brokenResponseBody{io.ErrUnexpectedEOF}}, nil
			})}))
			defer c.Close()
			var err error
			switch operation {
			case "verify":
				err = c.Verify(context.Background())
			case "create":
				_, err = c.CreateTask(context.Background(), task())
			case "poll":
				_, err = c.Poll(context.Background(), "default")
			}
			var network *NetworkError
			if !errors.As(err, &network) || !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("read failure not wrapped: %T %v", err, err)
			}
			if network.MayHaveBeenAccepted != (operation == "create") || calls != 1 {
				t.Fatal(network, calls)
			}
		})
	}
}

func TestTooManyTasksErrorBeforeSending(t *testing.T) {
	calls := 0
	c := New("key", WithHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("must not send")
	})}))
	defer c.Close()
	c.trackingLimit = 0
	_, err := c.CreateTask(context.Background(), task())
	var capacity *TooManyTasksError
	if !errors.As(err, &capacity) || capacity.Limit != 0 || calls != 0 {
		t.Fatalf("expected typed local rejection: %T %v; requests %d", err, err, calls)
	}
}

func TestNetworkErrorPreservesContextCause(t *testing.T) {
	c := New("key", WithHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})}))
	defer c.Close()
	_, err := c.CreateTask(context.Background(), task())
	var network *NetworkError
	if !errors.As(err, &network) || !errors.Is(err, context.DeadlineExceeded) || !network.MayHaveBeenAccepted {
		t.Fatal(err)
	}
}
