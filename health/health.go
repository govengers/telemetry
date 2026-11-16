// Package health serves the liveness and readiness probes of a govengers service (standards
// LAY-003, LAY-004) as plain http.Handlers.
//
// Liveness answers 200 while the process can serve HTTP at all and never looks at a
// dependency, so a database outage does not make the kubelet restart every pod. Readiness runs
// the checks the caller passes in (a pool ping, a Redis ping) concurrently under one timeout,
// caches the result briefly and answers 503 when a check fails or after SetDraining, so the
// load balancer stops routing to the pod before its server shuts down.
package health

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Routes of the two probes; the ginadapter registers them, and they are the routes to skip in
// the request metrics, the wide events and the traces.
const (
	LivenessRoute  = "/api/health"
	ReadinessRoute = "/api/ready"
)

const (
	// DefaultTimeout bounds one readiness evaluation when Options.Timeout is zero. It stays
	// below the Kubernetes default probe timeoutSeconds of 1, so the kubelet gets the 503 body.
	DefaultTimeout = 800 * time.Millisecond
	// DefaultCacheFor is how long a readiness result is reused when Options.CacheFor is zero.
	DefaultCacheFor = time.Second
)

// Values of the readiness response body: the overall status and the status of each check.
const (
	StatusReady       = "ready"
	StatusUnavailable = "unavailable"
	StatusDraining    = "draining"
	CheckOK           = "ok"
	CheckFailed       = "failed"
	CheckTimeout      = "timeout"
)

// Check reports whether one dependency can serve: nil means it can. It must return when ctx
// is done; a check that does not still counts as timed out once the readiness timeout passes,
// its goroutine keeps running until it returns, and later evaluations report it as timed out
// without starting it again until then.
type Check func(ctx context.Context) error

// Options configures New.
type Options struct {
	// Checks are run by readiness, keyed by a name that appears in the response body. None
	// means readiness only reports draining.
	Checks map[string]Check
	// Timeout bounds one evaluation of all checks; zero means DefaultTimeout. Keep it below the
	// probe's timeoutSeconds, or the kubelet times out before the 503 body arrives.
	Timeout time.Duration
	// CacheFor is how long an evaluation's result answers further probes; zero means
	// DefaultCacheFor. It keeps a flood of unauthenticated probe requests from pinging the
	// database once per request.
	CacheFor time.Duration
	// OnCheckFailure is called with a failing check's name and error, once per evaluation, so
	// main can record why the pod is unready; the response body carries only the check's
	// status, never the error text. It runs after the result is published, so a panic in it
	// fails only the probe that ran the evaluation. Two evaluations can call it at the same
	// time, so it must be safe for concurrent use. nil reports nothing. It must not block.
	OnCheckFailure func(name string, err error)
}

// Health holds the readiness checks and the draining flag. It is safe for concurrent use.
type Health struct {
	checks         []*namedCheck
	timeout        time.Duration
	cacheFor       time.Duration
	onCheckFailure func(name string, err error)
	now            func() time.Time
	draining       atomic.Bool

	mutex    sync.Mutex
	last     *evaluation   // the newest finished evaluation, nil before the first
	inFlight chan struct{} // closed when the running evaluation finishes, nil when none runs
}

type namedCheck struct {
	name    string
	check   Check
	running atomic.Bool // set while a run of check has not returned
}

// evaluation is the outcome of one run of every check.
type evaluation struct {
	checks     map[string]string // check name to CheckOK, CheckFailed or CheckTimeout
	ready      bool
	finishedAt time.Time
}

// New validates options and returns the probes. A nil options value means no checks and the
// default timeout and cache duration.
func New(options *Options) (*Health, error) {
	if options == nil {
		options = &Options{}
	}
	if options.Timeout < 0 || options.CacheFor < 0 {
		return nil, errors.New("telemetry health: Timeout and CacheFor must not be negative")
	}
	health := &Health{
		timeout:        cmp.Or(options.Timeout, DefaultTimeout),
		cacheFor:       cmp.Or(options.CacheFor, DefaultCacheFor),
		onCheckFailure: options.OnCheckFailure,
		now:            time.Now,
	}
	for name, check := range options.Checks {
		if name == "" || check == nil {
			return nil, fmt.Errorf("telemetry health: check %q needs a name and a function", name)
		}
		health.checks = append(health.checks, &namedCheck{name: name, check: check})
	}
	return health, nil
}

// SetDraining makes readiness answer 503 from now on, without running the checks. Call it
// first in the drain sequence, then keep serving until the load balancer has seen the failed
// probe, then shut the server down. Liveness is unaffected. It cannot be undone.
func (h *Health) SetDraining() {
	h.draining.Store(true)
}

// LivenessHandler answers 200 with the body "ok" to every request.
func (h *Health) LivenessHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		writer.Header().Set("Cache-Control", "no-store")
		_, _ = writer.Write([]byte("ok\n"))
	})
}

// ReadinessHandler answers 200 with {"status":"ready","checks":{...}} when every check
// passed, 503 with status "unavailable" when one failed or timed out, and 503 with status
// "draining" after SetDraining. Concurrent requests share one evaluation, and a result is
// reused for CacheFor.
func (h *Health) ReadinessHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := readinessBody{Status: StatusDraining}
		if !h.draining.Load() {
			result := h.evaluate(request.Context())
			body = readinessBody{Status: StatusUnavailable, Checks: result.checks}
			if result.ready {
				body.Status = StatusReady
			}
			if h.draining.Load() { // SetDraining came while the checks ran
				body = readinessBody{Status: StatusDraining}
			}
		}
		statusCode := http.StatusServiceUnavailable
		if body.Status == StatusReady {
			statusCode = http.StatusOK
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Cache-Control", "no-store")
		writer.WriteHeader(statusCode)
		_ = json.NewEncoder(writer).Encode(body)
	})
}

type readinessBody struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

// evaluate returns a fresh enough result, waits for the running evaluation, or runs one. The
// checks run on a context detached from the request, so a probe that hangs up does not cancel
// the evaluation other probes wait for.
func (h *Health) evaluate(ctx context.Context) *evaluation {
	h.mutex.Lock()
	if h.last != nil && h.now().Sub(h.last.finishedAt) < h.cacheFor {
		last := h.last
		h.mutex.Unlock()
		return last
	}
	if h.inFlight != nil {
		done := h.inFlight
		h.mutex.Unlock()
		<-done // bounded by the timeout: runChecks never waits longer
		h.mutex.Lock()
		defer h.mutex.Unlock()
		return h.last
	}
	done := make(chan struct{})
	h.inFlight = done
	h.mutex.Unlock()

	result, failures := h.runChecks(context.WithoutCancel(ctx))

	h.mutex.Lock()
	h.last, h.inFlight = result, nil
	h.mutex.Unlock()
	close(done)
	if h.onCheckFailure != nil {
		for _, failure := range failures {
			h.onCheckFailure(failure.name, failure.err)
		}
	}
	return result
}

type checkResult struct {
	name string
	err  error
}

// errStillRunning reports a check whose run from an earlier evaluation has not returned.
var errStillRunning = fmt.Errorf("telemetry health: check still running from an earlier evaluation: %w", context.DeadlineExceeded)

// runChecks runs every check in its own goroutine and collects the results until the timeout;
// a check that has not answered by then counts as CheckTimeout. It returns the failures for
// OnCheckFailure in the order they were recorded.
func (h *Health) runChecks(ctx context.Context) (*evaluation, []checkResult) {
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	results := make(chan checkResult, len(h.checks)) // buffered: a late check never blocks on send
	for _, named := range h.checks {
		if !named.running.CompareAndSwap(false, true) {
			results <- checkResult{name: named.name, err: errStillRunning}
			continue
		}
		go func() {
			err := runRecovering(ctx, named)
			results <- checkResult{name: named.name, err: err}
		}()
	}
	result := &evaluation{checks: make(map[string]string, len(h.checks)), ready: true}
	var failures []checkResult
	for range h.checks {
		select {
		case finished := <-results:
			failures = result.record(finished, failures)
		case <-ctx.Done():
			failures = result.drain(results, failures)
			for _, named := range h.checks {
				if _, answered := result.checks[named.name]; !answered {
					failures = result.record(checkResult{name: named.name, err: ctx.Err()}, failures)
				}
			}
			result.finishedAt = h.now()
			return result, failures
		}
	}
	result.finishedAt = h.now()
	return result, failures
}

// drain records the checks that finished in the same instant the timeout fired.
func (e *evaluation) drain(results <-chan checkResult, failures []checkResult) []checkResult {
	for {
		select {
		case finished := <-results:
			failures = e.record(finished, failures)
		default:
			return failures
		}
	}
}

// record stores a check's status and appends it to failures when it did not pass.
func (e *evaluation) record(finished checkResult, failures []checkResult) []checkResult {
	if finished.err == nil {
		e.checks[finished.name] = CheckOK
		return failures
	}
	e.ready = false
	e.checks[finished.name] = CheckFailed
	if errors.Is(finished.err, context.DeadlineExceeded) {
		e.checks[finished.name] = CheckTimeout
	}
	return append(failures, finished)
}

// runRecovering turns a panicking check into a failed one: the check runs in a goroutine of
// its own, where a panic would end the process. It clears the running flag in its deferred
// function, so the flag also clears when the check ends through runtime.Goexit, and it clears
// before the caller sends the result, so a directly following evaluation does not see it set.
func runRecovering(ctx context.Context, named *namedCheck) (err error) {
	defer func() {
		named.running.Store(false)
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("telemetry health: check panicked: %v", recovered)
		}
	}()
	return named.check(ctx)
}
