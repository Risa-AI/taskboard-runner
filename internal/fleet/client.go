package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/Risa-AI/taskboard-runner/pkg/protocol"
)

// Refusal is a call taskboard answered with a 4xx: a decision, not an
// outage, so it is never retried.
type Refusal struct {
	Status  int
	Code    string
	Message string
}

func (r *Refusal) Error() string {
	if r.Message == "" {
		return fmt.Sprintf("taskboard answered %d", r.Status)
	}
	return fmt.Sprintf("taskboard answered %d: %s", r.Status, r.Message)
}

// fatal reports whether a refusal means this runner cannot go on at all: its
// token was refused, or its protocol is one taskboard no longer speaks.
func fatal(err error) bool {
	var r *Refusal
	return errors.As(err, &r) && (r.Status == http.StatusUnauthorized || r.Status == http.StatusUpgradeRequired)
}

// client makes the calls of docs/protocol.md. It never logs or returns the
// fleet token: an error names the call and taskboard's answer, nothing else.
type client struct {
	base      string
	token     string
	http      *http.Client
	retryBase time.Duration
	retryMax  time.Duration
	sleep     func(context.Context, time.Duration) error
}

// once makes one call. out, when not nil, receives the envelope's data. It
// answers the HTTP status, a *Refusal for a 4xx, or a transport error.
func (c *client) once(ctx context.Context, method, path string, in, out interface{}) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+protocol.BasePath+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set(protocol.TokenHeader, c.token)
	req.Header.Set(protocol.VersionHeader, strconv.Itoa(protocol.ProtocolVersion))
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("%s %s: reading the answer: %w", method, path, err)
	}
	switch {
	case resp.StatusCode >= 500:
		return resp.StatusCode, fmt.Errorf("%s %s: taskboard answered %d", method, path, resp.StatusCode)
	case resp.StatusCode >= 400:
		var env protocol.Envelope[json.RawMessage]
		_ = json.Unmarshal(data, &env)
		r := &Refusal{Status: resp.StatusCode}
		if env.Error != nil {
			r.Code, r.Message = env.Error.Code, env.Error.Message
		}
		return resp.StatusCode, r
	}
	if out != nil && resp.StatusCode == http.StatusOK {
		env := protocol.Envelope[json.RawMessage]{}
		if err := json.Unmarshal(data, &env); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s: the answer is not JSON: %w", method, path, err)
		}
		if err := json.Unmarshal(env.Data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s: the answer's data: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

// call retries a call that taskboard did not answer, or answered with a 5xx,
// with exponential back-off, until it is answered or ctx ends. A refusal is
// returned at once. Every call here is safe to repeat: case posts are kept
// once per (run, index), and the others replace or ask again.
func (c *client) call(ctx context.Context, method, path string, in, out interface{}) (int, error) {
	wait := c.retryBase
	for {
		status, err := c.once(ctx, method, path, in, out)
		var r *Refusal
		if err == nil || errors.As(err, &r) {
			return status, err
		}
		if ctx.Err() != nil {
			return status, ctx.Err()
		}
		if err := c.sleep(ctx, wait); err != nil {
			return status, err
		}
		wait = min(wait*2, c.retryMax)
	}
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
