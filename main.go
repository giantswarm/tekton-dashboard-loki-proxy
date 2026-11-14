// Command tekton-dashboard-loki-proxy serves container logs stored in Loki in
// the form Tekton Dashboard expects from an external logs provider, as
// configured with its --external-logs argument.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// listenAddress is the address the proxy listens on.
	listenAddress = ":3000"

	// timeFormat is the layout of the startTime and completionTime query
	// parameters, which Tekton Dashboard copies from the status of the TaskRun.
	timeFormat = "2006-01-02T15:04:05Z"

	// timestampFormat is the layout of the timestamps put in front of log
	// lines on request. It is the one of the Kubernetes API, where Tekton
	// Dashboard gets its logs from as long as the pod exists: other than
	// time.RFC3339Nano it keeps the zeros at the end of the nanoseconds,
	// without which Tekton Dashboard does not recognize some timestamps.
	timestampFormat = "2006-01-02T15:04:05.000000000Z07:00"

	// incompleteLine ends a log whose rest could not be fetched from Loki.
	// Tekton Dashboard shows a line starting like this as an error.
	incompleteLine = "::error::tekton-dashboard-loki-proxy could not get the rest of this log from Loki"

	// defaultBatchSize is the number of log entries requested from Loki at
	// once. Loki refuses queries asking for more than its
	// max_entries_limit_per_query, which is 5000 unless configured otherwise.
	defaultBatchSize = 5000

	// statusClientClosedRequest is the status of requests whose client did
	// not wait for the response. Nobody receives it, but it is logged.
	statusClientClosedRequest = 499
)

func main() {
	err := mainE()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// mainE is a helper function allowing main to handle os.Exit in a single place.
func mainE() error {
	var cfg config
	flag.StringVar(&cfg.lokiEndpoint, "loki-endpoint", "http://localhost:3100", "The loki endpoint to query against")
	flag.StringVar(&cfg.orgID, "org-id", "1", "The tenant org ID")
	flag.DurationVar(&cfg.since, "since", 120*time.Hour, "The duration to query for logs")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	p, err := newProxy(cfg)
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              listenAddress,
		Handler:           p.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	slog.Info("listening", "address", listenAddress)

	return server.ListenAndServe()
}

// config holds the settings of a proxy.
type config struct {
	// lokiEndpoint is the base URL of the Loki API.
	lokiEndpoint string
	// orgID is the Loki tenant to query. It is sent as X-Scope-OrgID header
	// unless it is empty.
	orgID string
	// since is how far to look back for logs when a request does not tell
	// when the container started.
	since time.Duration
}

// proxy answers the requests Tekton Dashboard sends to an external logs
// provider with the logs it finds in Loki.
type proxy struct {
	client    *http.Client
	queryURL  string
	orgID     string
	since     time.Duration
	batchSize int
}

func newProxy(cfg config) (*proxy, error) {
	endpoint, err := url.Parse(cfg.lokiEndpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid Loki endpoint: %w", err)
	}
	if (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" {
		return nil, fmt.Errorf("invalid Loki endpoint %#q: it must be an http or https URL", cfg.lokiEndpoint)
	}

	// Loki is queried directly: like the Loki client this proxy used before,
	// it ignores the proxies configured in the environment.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil

	p := &proxy{
		client:    &http.Client{Transport: transport},
		queryURL:  endpoint.JoinPath("loki", "api", "v1", "query_range").String(),
		orgID:     cfg.orgID,
		since:     cfg.since,
		batchSize: defaultBatchSize,
	}

	return p, nil
}

// handler returns the HTTP handler serving the API of the proxy.
func (p *proxy) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /logs/{namespace}/{pod}/{container}", p.serveLogs)

	return logRequests(mux)
}

// serveLogs responds with the log of a container, one entry per line. The
// lines are sent as they arrive from Loki, batch by batch.
func (p *proxy) serveLogs(w http.ResponseWriter, r *http.Request) {
	// %q quotes the values as LogQL string literals, so that whatever is
	// requested cannot change the meaning of the query.
	query := fmt.Sprintf(`{container=%q,namespace=%q,pod=%q}`, r.PathValue("container"), r.PathValue("namespace"), r.PathValue("pod"))
	params := r.URL.Query()
	start, end := p.timeRange(params, time.Now())
	// Tekton Dashboard asks for timestamps when its user wants to see them.
	timestamps, _ := strconv.ParseBool(params.Get("timestamps"))

	// Tekton Dashboard shows a response as log only if it is declared as
	// plain text. Go would guess the type from the content if it was not set,
	// and so would a browser unless it is told not to.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	response := http.NewResponseController(w)
	started := false
	for entries, err := range p.queryLogs(r.Context(), query, start, end) {
		switch {
		case r.Context().Err() != nil:
			// The client is gone, which also ended the query. That is no failure.
			if !started {
				w.WriteHeader(statusClientClosedRequest)
			}
			return
		case err != nil && !started:
			slog.Error("failed to query Loki", "query", query, "error", err)
			http.Error(w, "failed to query Loki", http.StatusBadGateway)
			return
		case err != nil:
			// The status is sent already, so it cannot tell about the failure
			// any more. Aborting the response does: its end is missing then.
			// Tekton Dashboard does not pass that on to its user though, to
			// whom the log would look complete, hence the line telling so.
			slog.Error("failed to query Loki, the log is incomplete", "query", query, "error", err)
			_, _ = fmt.Fprintln(w, incompleteLine)
			_ = response.Flush()
			panic(http.ErrAbortHandler)
		}

		started = true
		for _, e := range entries {
			err := writeEntry(w, e, timestamps)
			if err != nil {
				// The client is gone, there is nobody left to tell.
				return
			}
		}
		// Send the batch off before the next one is fetched.
		_ = response.Flush()
	}
}

// writeEntry writes a log entry as a line, with its timestamp in front if
// timestamps is set.
func writeEntry(w io.Writer, e entry, timestamps bool) error {
	line := strings.TrimSuffix(e.line, "\n")
	if timestamps {
		line = time.Unix(0, e.timestamp).UTC().Format(timestampFormat) + " " + line
	}

	_, err := fmt.Fprintln(w, line)

	return err
}

// timeRange returns the time range in which to look for the logs of a
// request. Tekton Dashboard tells when the TaskRun started and completed as
// far as it knows.
func (p *proxy) timeRange(params url.Values, now time.Time) (time.Time, time.Time) {
	start := now.Add(-p.since)
	if t, err := time.Parse(timeFormat, params.Get("startTime")); err == nil {
		// Add an extra hour buffer to account for any jitter
		start = t.Add(-time.Hour)
	}

	end := now
	if t, err := time.Parse(timeFormat, params.Get("completionTime")); err == nil {
		// Add an extra hour buffer to account for any jitter
		end = t.Add(time.Hour)
	}

	return start, end
}

// queryLogs returns the entries logged between start and end to the streams
// selected by query, oldest first, in the batches they are fetched in. An
// error ends it.
//
// Loki limits how many entries it returns per query, hence the batches. Loki
// has no cursor to continue a query with: the next batch starts at the
// timestamp of the last entry instead. Any number of entries can share a
// timestamp, so the next batch repeats the entries the previous one ended
// with, and those are left out.
func (p *proxy) queryLogs(ctx context.Context, query string, start, end time.Time) iter.Seq2[[]entry, error] {
	return func(yield func([]entry, error) bool) {
		from, until := start.UnixNano(), end.UnixNano()
		// seen counts the lines of the previous batch which have its last
		// timestamp, the one the current batch starts at.
		var seen map[string]int

		// Each batch either brings from closer to until or is the last one.
		for from < until {
			entries, err := p.queryRange(ctx, query, from, until)
			if err != nil {
				yield(nil, err)
				return
			}
			if len(entries) == 0 {
				return
			}

			last := entries[len(entries)-1].timestamp
			repeated := map[string]int{}
			count := 0
			for i := len(entries) - 1; i >= 0 && entries[i].timestamp == last; i-- {
				repeated[entries[i].line]++
				count++
			}

			unseen := entries[:0]
			for _, e := range entries {
				if e.timestamp < from {
					// Such entries would be returned over and over again.
					yield(nil, errors.New("got a log entry from Loki which is older than asked for"))
					return
				}
				if e.timestamp == from && seen[e.line] > 0 {
					seen[e.line]--
					continue
				}

				unseen = append(unseen, e)
			}
			if len(unseen) > 0 && !yield(unseen, nil) {
				return
			}

			switch {
			case count >= p.batchSize:
				// The batch is full of entries sharing one timestamp, so
				// asking for that timestamp again would return the very same
				// batch. Continue after it: this is the only way forward,
				// even though it misses the entries of that timestamp which
				// did not fit.
				slog.Warn("log entries may be missing, too many of them have the same timestamp", "query", query, "timestamp", time.Unix(0, last).UTC(), "batchSize", p.batchSize)
				from, seen = last+1, nil
			case last == from:
				// Loki has nothing after the timestamp this batch started
				// at, and asking again would not change that.
				return
			default:
				from, seen = last, repeated
			}
		}
	}
}

// entry is a log line and the time it was logged at, in nanoseconds since the
// Unix epoch.
type entry struct {
	timestamp int64
	line      string
}

// queryResponse is the part the proxy needs of the response of Loki to a
// query for logs.
//
// See https://grafana.com/docs/loki/latest/reference/loki-http-api/#query-logs-within-a-range-of-time
type queryResponse struct {
	Data struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			// Values are the entries of a stream, each one a timestamp
			// followed by the log line.
			Values [][2]string `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// queryRange asks Loki for the entries logged between start and end to the
// streams selected by query. It returns at most p.batchSize entries, those
// closest to start, ordered by time.
func (p *proxy) queryRange(ctx context.Context, query string, start, end int64) ([]entry, error) {
	params := url.Values{}
	params.Set("query", query)
	params.Set("start", strconv.FormatInt(start, 10))
	params.Set("end", strconv.FormatInt(end, 10))
	params.Set("limit", strconv.Itoa(p.batchSize))
	params.Set("direction", "forward")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.queryURL, nil)
	if err != nil {
		return nil, err
	}
	req.URL.RawQuery = params.Encode()
	req.Header.Set("User-Agent", "tekton-dashboard-loki-proxy")
	if p.orgID != "" {
		req.Header.Set("X-Scope-OrgID", p.orgID)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	// Reading the whole body, whatever the status, lets the connection to
	// Loki be reused.
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("reading the response of Loki: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from Loki: %.500s", resp.StatusCode, body)
	}

	var result queryResponse
	err = json.Unmarshal(body, &result)
	if err != nil {
		return nil, fmt.Errorf("decoding the response of Loki: %w", err)
	}
	if result.Data.ResultType != "streams" {
		return nil, fmt.Errorf("unexpected result type %#q from Loki", result.Data.ResultType)
	}

	var entries []entry
	for _, stream := range result.Data.Result {
		for _, value := range stream.Values {
			timestamp, err := strconv.ParseInt(value[0], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid timestamp from Loki: %w", err)
			}

			entries = append(entries, entry{timestamp: timestamp, line: value[1]})
		}
	}

	// Each stream is ordered already, what is left to do is to merge them.
	// The stable sort keeps entries of the same time in the order of Loki.
	slices.SortStableFunc(entries, func(a, b entry) int {
		return cmp.Compare(a.timestamp, b.timestamp)
	})

	return entries, nil
}

// logRequests logs the requests handled by next.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		// Deferred, as a handler which aborts its response does not return.
		defer func() {
			slog.Info("handled request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", recorder.status,
				"duration", time.Since(start).String(),
				"remoteAddr", r.RemoteAddr,
			)
		}()

		next.ServeHTTP(recorder, r)
	})
}

// statusRecorder remembers the status code a handler responds with.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Unwrap gives access to what the recorded ResponseWriter can do beyond
// writing, sending off what is written so far in particular.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
