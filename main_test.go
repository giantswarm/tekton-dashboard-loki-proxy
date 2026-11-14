package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	// logsPath is where the proxy serves the logs of the container which the
	// streams made with logs belong to.
	logsPath = "/logs/ci/build-pod/step-build"

	// runTimes tells the proxy when the TaskRun of the tests ran: from
	// runStart on, for ten minutes.
	runTimes = "?startTime=2023-05-16T10:00:00Z&completionTime=2023-05-16T10:10:00Z"

	// logsQuery is how the proxy asks Loki for the logs of that container.
	logsQuery = `{container="step-build",namespace="ci",pod="build-pod"}`

	// queryPath is where Loki answers queries for logs.
	queryPath = "/loki/api/v1/query_range"

	// tenant is the Loki tenant the proxy of the tests queries.
	tenant = "tenant"

	// plainText is the content type Tekton Dashboard needs logs to have, it
	// parses anything else as JSON.
	plainText = "text/plain; charset=utf-8"

	failedBody = "failed to query Loki\n"
)

var runStart = time.Date(2023, time.May, 16, 10, 0, 0, 0, time.UTC)

func TestMain(m *testing.M) {
	// Keep the logs of the proxy out of the test output.
	slog.SetDefault(slog.New(slog.DiscardHandler))

	m.Run()
}

func Test_Proxy_Logs(t *testing.T) {
	testCases := []struct {
		name         string
		streams      []lokiStream
		batchSize    int
		path         string
		expectedBody string
	}{
		{
			name: "case 0: the lines of a container are returned in the order they were logged",
			streams: []lokiStream{
				logs("stdout", at(1*time.Second, "one"), at(2*time.Second, "two"), at(3*time.Second, "three")),
			},
			path:         logsPath + runTimes,
			expectedBody: "one\ntwo\nthree\n",
		},
		{
			name: "case 1: the streams of a container are merged by time",
			streams: []lokiStream{
				logs("stdout", at(1*time.Second, "out 1"), at(3*time.Second, "out 2")),
				logs("stderr", at(2*time.Second, "err 1"), at(4*time.Second, "err 2")),
			},
			path:         logsPath + runTimes,
			expectedBody: "out 1\nerr 1\nout 2\nerr 2\n",
		},
		{
			name: "case 2: the logs of other containers, pods and namespaces are left out",
			streams: []lokiStream{
				{labels: labels("step-test", "ci", "build-pod"), entries: []entry{at(1*time.Second, "other container")}},
				{labels: labels("step-build", "ci", "test-pod"), entries: []entry{at(2*time.Second, "other pod")}},
				{labels: labels("step-build", "cd", "build-pod"), entries: []entry{at(3*time.Second, "other namespace")}},
				logs("stdout", at(4*time.Second, "mine")),
			},
			path:         logsPath + runTimes,
			expectedBody: "mine\n",
		},
		{
			name: "case 3: a line break at the end of a line is not doubled",
			streams: []lokiStream{
				logs("stdout",
					at(1*time.Second, "with line break\n"),
					at(2*time.Second, "without"),
					at(3*time.Second, ""),
					at(4*time.Second, "two\nlines"),
					at(5*time.Second, "two line breaks\n\n"),
				),
			},
			path:         logsPath + runTimes,
			expectedBody: "with line break\nwithout\n\ntwo\nlines\ntwo line breaks\n\n",
		},
		{
			name:         "case 4: a container without logs gets an empty response",
			streams:      nil,
			path:         logsPath + runTimes,
			expectedBody: "",
		},
		{
			name: "case 5: logs from more than an hour before the start or after the completion are left out",
			streams: []lokiStream{
				logs("stdout",
					at(-61*time.Minute, "too early"),
					at(-59*time.Minute, "early"),
					at(69*time.Minute, "late"),
					at(71*time.Minute, "too late"),
				),
			},
			path:         logsPath + runTimes,
			expectedBody: "early\nlate\n",
		},
		{
			name: "case 6: without start and completion the logs of the configured duration until now are returned",
			streams: []lokiStream{
				logs("stdout",
					ago(121*time.Hour, "too old"),
					ago(119*time.Hour, "old"),
					ago(1*time.Minute, "new"),
					ago(-1*time.Minute, "not yet logged"),
				),
			},
			path:         logsPath,
			expectedBody: "old\nnew\n",
		},
		{
			name: "case 7: a log longer than a batch is returned completely",
			streams: []lokiStream{
				logs("stdout",
					at(1*time.Second, "1"),
					at(2*time.Second, "2"),
					at(3*time.Second, "3"),
					at(4*time.Second, "4"),
					at(5*time.Second, "5"),
					at(6*time.Second, "6"),
					at(7*time.Second, "7"),
					at(8*time.Second, "8"),
				),
			},
			batchSize:    3,
			path:         logsPath + runTimes,
			expectedBody: "1\n2\n3\n4\n5\n6\n7\n8\n",
		},
		{
			name: "case 8: entries with the same timestamp are returned once when a batch ends between them",
			streams: []lokiStream{
				logs("stdout",
					at(1*time.Second, "a"),
					at(2*time.Second, "b"),
					at(3*time.Second, "c"),
					at(3*time.Second, "d"),
					at(3*time.Second, "e"),
					at(4*time.Second, "f"),
				),
			},
			batchSize:    4,
			path:         logsPath + runTimes,
			expectedBody: "a\nb\nc\nd\ne\nf\n",
		},
		{
			name: "case 9: equal lines with the same timestamp are all returned when a batch ends between them",
			streams: []lokiStream{
				logs("stdout", at(1*time.Second, "a"), at(2*time.Second, "b"), at(3*time.Second, "same")),
				logs("stderr", at(3*time.Second, "same"), at(4*time.Second, "c")),
			},
			batchSize:    3,
			path:         logsPath + runTimes,
			expectedBody: "a\nb\nsame\nsame\nc\n",
		},
		{
			name: "case 10: equal lines with different timestamps are all returned",
			streams: []lokiStream{
				logs("stdout", at(1*time.Second, "same"), at(2*time.Second, "same"), at(3*time.Second, "same")),
			},
			batchSize:    2,
			path:         logsPath + runTimes,
			expectedBody: "same\nsame\nsame\n",
		},
		{
			// Loki cannot return what is left of a timestamp once a batch is
			// full of its entries: "b3" is lost, the rest of the log is not.
			name: "case 11: the log goes on after more entries with the same timestamp than fit into a batch",
			streams: []lokiStream{
				logs("stdout",
					at(1*time.Second, "a"),
					at(2*time.Second, "b1"),
					at(2*time.Second, "b2"),
					at(2*time.Second, "b3"),
					at(3*time.Second, "c"),
				),
			},
			batchSize:    2,
			path:         logsPath + runTimes,
			expectedBody: "a\nb1\nb2\nc\n",
		},
		{
			name: "case 12: names are quoted in the query to Loki",
			streams: []lokiStream{
				{labels: labels(`step"} or {container=~".+\`, "ci", "build-pod"), entries: []entry{at(1*time.Second, "odd name")}},
				logs("stdout", at(2*time.Second, "usual name")),
			},
			path:         "/logs/ci/build-pod/" + url.PathEscape(`step"} or {container=~".+\`) + runTimes,
			expectedBody: "odd name\n",
		},
		{
			name: "case 13: lines are returned as they were logged",
			streams: []lokiStream{
				logs("stdout",
					at(1*time.Second, "<!DOCTYPE html><html><body>not a page</body></html>"),
					at(2*time.Second, `{"level":"info","msg":"a \"quoted\" word & more"}`),
					at(3*time.Second, "\x1b[32mgrün\x1b[0m\ttab 日本語 🚀"),
				),
			},
			path: logsPath + runTimes,
			expectedBody: "<!DOCTYPE html><html><body>not a page</body></html>\n" +
				`{"level":"info","msg":"a \"quoted\" word & more"}` + "\n" +
				"\x1b[32mgrün\x1b[0m\ttab 日本語 🚀\n",
		},
		{
			// Loki would refuse to look for logs which end before they start.
			name: "case 14: nothing is logged in a time range without duration",
			streams: []lokiStream{
				logs("stdout", at(1*time.Second, "line")),
			},
			path:         logsPath + "?startTime=2023-05-16T13:00:00Z&completionTime=2023-05-16T10:00:00Z",
			expectedBody: "",
		},
		{
			// As the Kubernetes API does it: in UTC, with all digits of the
			// nanoseconds, once per entry.
			name: "case 15: lines begin with the time they were logged at when timestamps are asked for",
			streams: []lokiStream{
				logs("stdout", at(1*time.Second, "one"), at(2500*time.Millisecond, "two\n")),
				logs("stderr", at(3*time.Second+123456789*time.Nanosecond, "two\nlines")),
			},
			path: logsPath + runTimes + "&timestamps=true",
			expectedBody: "2023-05-16T10:00:01.000000000Z one\n" +
				"2023-05-16T10:00:02.500000000Z two\n" +
				"2023-05-16T10:00:03.123456789Z two\nlines\n",
		},
		{
			name: "case 16: lines are left as they are when timestamps are not asked for",
			streams: []lokiStream{
				logs("stdout", at(1*time.Second, "one"), at(2*time.Second, "two")),
			},
			path:         logsPath + runTimes + "&timestamps=false",
			expectedBody: "one\ntwo\n",
		},
		{
			name: "case 17: timestamps are added to every batch of a long log",
			streams: []lokiStream{
				logs("stdout", at(1*time.Second, "1"), at(2*time.Second, "2"), at(3*time.Second, "3")),
			},
			batchSize: 2,
			path:      logsPath + runTimes + "&timestamps=true",
			expectedBody: "2023-05-16T10:00:01.000000000Z 1\n" +
				"2023-05-16T10:00:02.000000000Z 2\n" +
				"2023-05-16T10:00:03.000000000Z 3\n",
		},
	}

	for i, tc := range testCases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Log(tc.name)

			proxyURL := startProxy(t, &fakeLoki{streams: tc.streams}, tc.batchSize)

			resp, body := get(t, http.MethodGet, proxyURL+tc.path)

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status == %d, want %d", resp.StatusCode, http.StatusOK)
			}
			if body != tc.expectedBody {
				t.Fatalf("body == %q, want %q", body, tc.expectedBody)
			}
			contentType := resp.Header.Get("Content-Type")
			if contentType != plainText {
				t.Fatalf("Content-Type == %q, want %q", contentType, plainText)
			}
			// Nor may a browser take the log for anything else.
			sniffing := resp.Header.Get("X-Content-Type-Options")
			if sniffing != "nosniff" {
				t.Fatalf("X-Content-Type-Options == %q, want %q", sniffing, "nosniff")
			}
		})
	}
}

// Test_Proxy_queryLogs_random compares the lines found in random streams,
// which have many equal timestamps and lines, with the lines put into them.
func Test_Proxy_queryLogs_random(t *testing.T) {
	loki := &fakeLoki{}
	lokiServer := httptest.NewServer(loki)
	t.Cleanup(lokiServer.Close)

	p, err := newProxy(config{lokiEndpoint: lokiServer.URL})
	if err != nil {
		t.Fatalf("error == %#v, want nil", err)
	}

	// #nosec G404 -- the streams have to be the same whenever the test runs.
	random := rand.New(rand.NewPCG(1, 2))
	for i := range 500 {
		p.batchSize = 2 + random.IntN(5)

		streams := []lokiStream{logs("stdout"), logs("stderr"), logs("other")}
		var expectedLines []string
		for second := range 1 + random.IntN(25) {
			// A batch must have room for all entries of a timestamp and one
			// more, or some of them cannot be fetched.
			for range random.IntN(p.batchSize) {
				line := fmt.Sprintf("%02d %c", second, 'a'+random.IntN(3))
				n := random.IntN(len(streams))
				streams[n].entries = append(streams[n].entries, at(time.Duration(second)*time.Second, line))
				expectedLines = append(expectedLines, line)
			}
		}
		loki.setStreams(streams)

		var lines []string
		for entries, err := range p.queryLogs(t.Context(), logsQuery, runStart, runStart.Add(time.Minute)) {
			if err != nil {
				t.Fatalf("round %d: error == %#v, want nil", i, err)
			}
			if len(entries) == 0 || len(entries) > p.batchSize {
				t.Fatalf("round %d: entries in a batch == %d, want 1 to %d", i, len(entries), p.batchSize)
			}
			for _, e := range entries {
				lines = append(lines, e.line)
			}
		}

		// The lines start with the second they were logged in.
		if !slices.IsSortedFunc(lines, func(a, b string) int { return cmp.Compare(a[:2], b[:2]) }) {
			t.Fatalf("round %d: lines == %q, want them ordered by time", i, lines)
		}
		slices.Sort(lines)
		slices.Sort(expectedLines)
		if !slices.Equal(lines, expectedLines) {
			t.Fatalf("round %d: lines == %q, want %q", i, lines, expectedLines)
		}
	}
}

// Test_Proxy_LokiErrors is about queries which fail before any of the log is
// sent, so that the response can tell.
func Test_Proxy_LokiErrors(t *testing.T) {
	testCases := []struct {
		name    string
		respond func(n int, w http.ResponseWriter) bool
	}{
		{
			name: "case 0: Loki responds with an error",
			respond: func(_ int, w http.ResponseWriter) bool {
				http.Error(w, "too many outstanding requests", http.StatusTooManyRequests)
				return true
			},
		},
		{
			name: "case 1: the response is not JSON",
			respond: func(_ int, w http.ResponseWriter) bool {
				_, _ = io.WriteString(w, "<html><body>Sign in</body></html>")
				return true
			},
		},
		{
			name: "case 2: the response does not contain log streams",
			respond: func(_ int, w http.ResponseWriter) bool {
				_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
				return true
			},
		},
		{
			name: "case 3: the timestamp of an entry is not a number",
			respond: func(_ int, w http.ResponseWriter) bool {
				_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"streams","result":[{"stream":{},"values":[["yesterday","line"]]}]}}`)
				return true
			},
		},
		{
			name: "case 4: Loki responds with an error which reads like a result",
			respond: func(_ int, w http.ResponseWriter) bool {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"streams","result":[]}}`)
				return true
			},
		},
		{
			name: "case 5: the entries are older than asked for",
			respond: func(_ int, w http.ResponseWriter) bool {
				_, _ = io.WriteString(w, streamsResponse(at(-3*time.Hour, "first"), at(-2*time.Hour, "second")))
				return true
			},
		},
	}

	for i, tc := range testCases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Log(tc.name)

			loki := &fakeLoki{
				streams: []lokiStream{
					logs("stdout", at(1*time.Second, "1"), at(2*time.Second, "2"), at(3*time.Second, "3")),
				},
				respond: tc.respond,
			}
			proxyURL := startProxy(t, loki, 2)

			resp, body := get(t, http.MethodGet, proxyURL+logsPath+runTimes)

			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("status == %d, want %d", resp.StatusCode, http.StatusBadGateway)
			}
			if body != failedBody {
				t.Fatalf("body == %q, want %q", body, failedBody)
			}
		})
	}
}

// Test_Proxy_LokiErrors_late is about queries which fail after a part of the
// log is sent, along with the status saying that all is well.
func Test_Proxy_LokiErrors_late(t *testing.T) {
	testCases := []struct {
		name         string
		respond      func(n int, w http.ResponseWriter) bool
		expectedBody string
	}{
		{
			name: "case 0: Loki responds with an error after the first batch",
			respond: func(n int, w http.ResponseWriter) bool {
				if n == 1 {
					return false
				}
				http.Error(w, "context deadline exceeded", http.StatusGatewayTimeout)
				return true
			},
			expectedBody: "1\n2\n" + incompleteLine + "\n",
		},
		{
			// Without the check for it the proxy would send these entries
			// forever.
			name: "case 1: Loki returns the first batch again",
			respond: func(_ int, w http.ResponseWriter) bool {
				_, _ = io.WriteString(w, streamsResponse(at(1*time.Second, "first"), at(2*time.Second, "second")))
				return true
			},
			expectedBody: "first\nsecond\n" + incompleteLine + "\n",
		},
	}

	for i, tc := range testCases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Log(tc.name)

			loki := &fakeLoki{
				streams: []lokiStream{
					logs("stdout", at(1*time.Second, "1"), at(2*time.Second, "2"), at(3*time.Second, "3")),
				},
				respond: tc.respond,
			}
			proxyURL := startProxy(t, loki, 2)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, proxyURL+logsPath+runTimes, nil)
			if err != nil {
				t.Fatalf("error == %#v, want nil", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("error == %#v, want nil", err)
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status == %d, want %d", resp.StatusCode, http.StatusOK)
			}
			// The response ends without its end, which is how a client can
			// tell that it is incomplete.
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("error == %#v, want %#v", err, io.ErrUnexpectedEOF)
			}
			// Tekton Dashboard does not tell its user, so the log does.
			if string(body) != tc.expectedBody {
				t.Fatalf("body == %q, want %q", body, tc.expectedBody)
			}
		})
	}
}

// Test_Proxy_Logs_streamed makes sure a batch is sent to the client before
// the next one is fetched.
func Test_Proxy_Logs_streamed(t *testing.T) {
	second := make(chan struct{})
	fetchSecond := sync.OnceFunc(func() { close(second) })
	loki := &fakeLoki{
		streams: []lokiStream{
			logs("stdout", at(1*time.Second, "1"), at(2*time.Second, "2"), at(3*time.Second, "3")),
		},
		respond: func(n int, _ http.ResponseWriter) bool {
			if n == 2 {
				// Loki holds the second batch back until the client has the first.
				<-second
			}
			return false
		},
	}
	proxyURL := startProxy(t, loki, 2)
	// Let Loki go on in any case, the test would not end otherwise.
	t.Cleanup(fetchSecond)

	// Without streaming the first batch never arrives, the time is up instead.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxyURL+logsPath+runTimes, nil)
	if err != nil {
		t.Fatalf("error == %#v, want nil", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("error == %#v, want nil", err)
	}
	defer func() { _ = resp.Body.Close() }()

	first := make([]byte, len("1\n2\n"))
	_, err = io.ReadFull(resp.Body, first)
	if err != nil {
		t.Fatalf("error == %#v, want nil", err)
	}
	if string(first) != "1\n2\n" {
		t.Fatalf("first batch == %q, want %q", first, "1\n2\n")
	}

	fetchSecond()
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("error == %#v, want nil", err)
	}
	if string(rest) != "3\n" {
		t.Fatalf("rest == %q, want %q", rest, "3\n")
	}
}

// Test_Proxy_Logs_dashboard makes sure Tekton Dashboard reads the lines the
// way they are meant, by reading them the way it does.
func Test_Proxy_Logs_dashboard(t *testing.T) {
	// From packages/components/src/components/Log/Log.jsx of Tekton Dashboard.
	dashboardLine := regexp.MustCompile(`(?s)^((?<timestamp>\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2})?(.\d{3,9})?(Z|[+-]\d{2}:\d{2}))\s?)?(::(?<command>group|endgroup|error|warning|info|notice|debug|trace)::)?(?<message>.*)?$`)

	loki := &fakeLoki{
		streams: []lokiStream{
			logs("stdout",
				at(1*time.Second, "whole second"),
				at(2*time.Second+100*time.Millisecond, "tenth of a second"),
				at(3*time.Second+1*time.Nanosecond, "::warning::with a level"),
				at(4*time.Second, "2023-05-16 starts with a date itself"),
			),
		},
		respond: func(n int, w http.ResponseWriter) bool {
			if n == 1 {
				return false
			}
			http.Error(w, "context deadline exceeded", http.StatusGatewayTimeout)
			return true
		},
	}
	proxyURL := startProxy(t, loki, 4)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, proxyURL+logsPath+runTimes+"&timestamps=true", nil)
	if err != nil {
		t.Fatalf("error == %#v, want nil", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("error == %#v, want nil", err)
	}
	// The response is incomplete, so reading it ends with an error.
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	expected := [][3]string{
		{"2023-05-16T10:00:01.000000000Z", "", "whole second"},
		{"2023-05-16T10:00:02.100000000Z", "", "tenth of a second"},
		{"2023-05-16T10:00:03.000000001Z", "warning", "with a level"},
		{"2023-05-16T10:00:04.000000000Z", "", "2023-05-16 starts with a date itself"},
		{"", "error", strings.TrimPrefix(incompleteLine, "::error::")},
	}
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	if len(lines) != len(expected) {
		t.Fatalf("lines == %q, want %d of them", lines, len(expected))
	}
	for i, line := range lines {
		match := dashboardLine.FindStringSubmatch(line)
		read := [3]string{
			match[dashboardLine.SubexpIndex("timestamp")],
			match[dashboardLine.SubexpIndex("command")],
			match[dashboardLine.SubexpIndex("message")],
		}
		if read != expected[i] {
			t.Fatalf("line %d read as timestamp, level and message == %q, want %q", i, read, expected[i])
		}
	}
}

func Test_Proxy_LokiUnreachable(t *testing.T) {
	loki := httptest.NewServer(http.NotFoundHandler())
	loki.Close()

	p, err := newProxy(config{lokiEndpoint: loki.URL, since: time.Hour})
	if err != nil {
		t.Fatalf("error == %#v, want nil", err)
	}
	server := httptest.NewServer(p.handler())
	t.Cleanup(server.Close)

	resp, body := get(t, http.MethodGet, server.URL+logsPath)

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status == %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
	if body != failedBody {
		t.Fatalf("body == %q, want %q", body, failedBody)
	}
}

// Test_Proxy_ClientGone makes sure a client which does not wait for its logs
// is not mistaken for a failure of Loki.
func Test_Proxy_ClientGone(t *testing.T) {
	queried := make(chan struct{})
	loki := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Loki answers only after the client of the proxy is gone.
		close(queried)
		<-r.Context().Done()
	}))
	t.Cleanup(loki.Close)

	p, err := newProxy(config{lokiEndpoint: loki.URL, since: time.Hour})
	if err != nil {
		t.Fatalf("error == %#v, want nil", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-queried
		cancel()
	}()
	recorder := httptest.NewRecorder()
	p.handler().ServeHTTP(recorder, httptest.NewRequestWithContext(ctx, http.MethodGet, logsPath, nil))

	if recorder.Code != statusClientClosedRequest {
		t.Fatalf("status == %d, want %d", recorder.Code, statusClientClosedRequest)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("body == %q, want none", recorder.Body.String())
	}
}

// Test_Proxy_LokiResponse makes sure the proxy understands entries the way
// Loki encodes them when they carry structured metadata.
func Test_Proxy_LokiResponse(t *testing.T) {
	loki := &fakeLoki{
		respond: func(n int, w http.ResponseWriter) bool {
			if n > 1 {
				// There is nothing after the first batch.
				return false
			}
			entry := at(1*time.Second, "line")
			_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"streams","encodingFlags":["categorize-labels"],"result":[{"stream":{"pod":"build-pod"},"values":[["%d","%s",{"structuredMetadata":{"trace_id":"0242ac120002"},"parsed":{"level":"info"}}]]}],"stats":{}}}`,
				entry.timestamp, entry.line)
			return true
		},
	}
	proxyURL := startProxy(t, loki, 0)

	resp, body := get(t, http.MethodGet, proxyURL+logsPath+runTimes)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status == %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if body != "line\n" {
		t.Fatalf("body == %q, want %q", body, "line\n")
	}
}

func Test_Proxy_LokiRequest(t *testing.T) {
	testCases := []struct {
		name           string
		endpointPath   string
		orgID          string
		expectedPath   string
		expectedOrgIDs []string
	}{
		{
			name:           "case 0: the logs of the container are queried for the tenant",
			endpointPath:   "",
			orgID:          tenant,
			expectedPath:   queryPath,
			expectedOrgIDs: []string{tenant},
		},
		{
			name:           "case 1: the path of the endpoint is kept",
			endpointPath:   "/some/prefix/",
			orgID:          tenant,
			expectedPath:   "/some/prefix" + queryPath,
			expectedOrgIDs: []string{tenant},
		},
		{
			name:           "case 2: no tenant is sent when none is configured",
			endpointPath:   "",
			orgID:          "",
			expectedPath:   queryPath,
			expectedOrgIDs: nil,
		},
	}

	for i, tc := range testCases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Log(tc.name)

			var mutex sync.Mutex
			var requests []*http.Request
			loki := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mutex.Lock()
				requests = append(requests, r)
				mutex.Unlock()
				_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"streams","result":[]}}`)
			}))
			t.Cleanup(loki.Close)

			p, err := newProxy(config{lokiEndpoint: loki.URL + tc.endpointPath, orgID: tc.orgID, since: time.Hour})
			if err != nil {
				t.Fatalf("error == %#v, want nil", err)
			}
			server := httptest.NewServer(p.handler())
			t.Cleanup(server.Close)

			resp, _ := get(t, http.MethodGet, server.URL+logsPath+runTimes)

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status == %d, want %d", resp.StatusCode, http.StatusOK)
			}
			mutex.Lock()
			defer mutex.Unlock()
			if len(requests) != 1 {
				t.Fatalf("requests to Loki == %d, want 1", len(requests))
			}
			request := requests[0]
			if request.Method != http.MethodGet {
				t.Fatalf("method == %q, want %q", request.Method, http.MethodGet)
			}
			if request.URL.Path != tc.expectedPath {
				t.Fatalf("path == %q, want %q", request.URL.Path, tc.expectedPath)
			}
			if !slices.Equal(request.Header.Values("X-Scope-OrgID"), tc.expectedOrgIDs) {
				t.Fatalf("X-Scope-OrgID == %q, want %q", request.Header.Values("X-Scope-OrgID"), tc.expectedOrgIDs)
			}
			if request.UserAgent() != "tekton-dashboard-loki-proxy" {
				t.Fatalf("User-Agent == %q, want %q", request.UserAgent(), "tekton-dashboard-loki-proxy")
			}
			// The logs are searched from an hour before the start until an
			// hour after the completion of the TaskRun.
			expectedParameters := url.Values{
				"query":     {logsQuery},
				"start":     {strconv.FormatInt(runStart.Add(-time.Hour).UnixNano(), 10)},
				"end":       {strconv.FormatInt(runStart.Add(10*time.Minute+time.Hour).UnixNano(), 10)},
				"limit":     {"5000"},
				"direction": {"forward"},
			}
			if request.URL.Query().Encode() != expectedParameters.Encode() {
				t.Fatalf("parameters == %q, want %q", request.URL.Query().Encode(), expectedParameters.Encode())
			}
		})
	}
}

func Test_Proxy_Routing(t *testing.T) {
	testCases := []struct {
		name           string
		method         string
		path           string
		expectedStatus int
	}{
		{
			name:           "case 0: the logs of a container are served",
			method:         http.MethodGet,
			path:           logsPath,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "case 1: parameters the proxy does not know are ignored",
			method:         http.MethodGet,
			path:           logsPath + "?logLevel=error&logLevel=info&follow=true",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "case 2: the headers can be asked for alone",
			method:         http.MethodHead,
			path:           logsPath,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "case 3: logs cannot be written",
			method:         http.MethodPost,
			path:           logsPath,
			expectedStatus: http.StatusMethodNotAllowed,
		},
		{
			name:           "case 4: there is nothing but logs",
			method:         http.MethodGet,
			path:           "/",
			expectedStatus: http.StatusNotFound,
		},
		{
			name:           "case 5: logs need a container",
			method:         http.MethodGet,
			path:           "/logs/ci/build-pod",
			expectedStatus: http.StatusNotFound,
		},
		{
			name:           "case 6: a container has nothing below it",
			method:         http.MethodGet,
			path:           logsPath + "/more",
			expectedStatus: http.StatusNotFound,
		},
		{
			// An empty name would select the streams of all containers.
			name:           "case 7: a container needs a name",
			method:         http.MethodGet,
			path:           "/logs/ci/build-pod/",
			expectedStatus: http.StatusNotFound,
		},
	}

	for i, tc := range testCases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Log(tc.name)

			loki := &fakeLoki{streams: []lokiStream{logs("stdout", ago(time.Minute, "line"))}}
			proxyURL := startProxy(t, loki, 0)

			resp, body := get(t, tc.method, proxyURL+tc.path)

			if resp.StatusCode != tc.expectedStatus {
				t.Fatalf("status == %d, want %d", resp.StatusCode, tc.expectedStatus)
			}
			if tc.method == http.MethodGet && tc.expectedStatus == http.StatusOK && body != "line\n" {
				t.Fatalf("body == %q, want %q", body, "line\n")
			}
		})
	}
}

func Test_Proxy_timeRange(t *testing.T) {
	now := time.Date(2026, time.November, 14, 18, 0, 0, 0, time.UTC)
	since := 120 * time.Hour

	testCases := []struct {
		name          string
		parameters    string
		expectedStart time.Time
		expectedEnd   time.Time
	}{
		{
			name:          "case 0: the configured duration until now when nothing is known",
			parameters:    "",
			expectedStart: now.Add(-since),
			expectedEnd:   now,
		},
		{
			name:          "case 1: an hour around start and completion",
			parameters:    "startTime=2026-11-14T12:00:00Z&completionTime=2026-11-14T12:10:00Z",
			expectedStart: time.Date(2026, time.November, 14, 11, 0, 0, 0, time.UTC),
			expectedEnd:   time.Date(2026, time.November, 14, 13, 10, 0, 0, time.UTC),
		},
		{
			name:          "case 2: until now when the TaskRun is not complete",
			parameters:    "startTime=2026-11-14T12:00:00Z",
			expectedStart: time.Date(2026, time.November, 14, 11, 0, 0, 0, time.UTC),
			expectedEnd:   now,
		},
		{
			name:          "case 3: the configured duration when only the completion is known",
			parameters:    "completionTime=2026-11-14T12:10:00Z",
			expectedStart: now.Add(-since),
			expectedEnd:   time.Date(2026, time.November, 14, 13, 10, 0, 0, time.UTC),
		},
		{
			name:          "case 4: times which cannot be read are ignored",
			parameters:    "startTime=yesterday&completionTime=12%3A10",
			expectedStart: now.Add(-since),
			expectedEnd:   now,
		},
	}

	for i, tc := range testCases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Log(tc.name)

			parameters, err := url.ParseQuery(tc.parameters)
			if err != nil {
				t.Fatalf("error == %#v, want nil", err)
			}
			p := &proxy{since: since}

			start, end := p.timeRange(parameters, now)

			if !start.Equal(tc.expectedStart) {
				t.Fatalf("start == %s, want %s", start, tc.expectedStart)
			}
			if !end.Equal(tc.expectedEnd) {
				t.Fatalf("end == %s, want %s", end, tc.expectedEnd)
			}
		})
	}
}

func Test_newProxy(t *testing.T) {
	testCases := []struct {
		name             string
		lokiEndpoint     string
		expectedQueryURL string
		expectedError    bool
	}{
		{
			name:             "case 0: an endpoint",
			lokiEndpoint:     "http://loki-read.loki:3100",
			expectedQueryURL: "http://loki-read.loki:3100/loki/api/v1/query_range",
		},
		{
			name:             "case 1: an endpoint ending with a slash",
			lokiEndpoint:     "http://loki-read.loki:3100/",
			expectedQueryURL: "http://loki-read.loki:3100/loki/api/v1/query_range",
		},
		{
			name:             "case 2: an endpoint with a path",
			lokiEndpoint:     "https://example.com/some/prefix",
			expectedQueryURL: "https://example.com/some/prefix/loki/api/v1/query_range",
		},
		{
			name:          "case 3: no endpoint",
			lokiEndpoint:  "",
			expectedError: true,
		},
		{
			name:          "case 4: an endpoint without scheme",
			lokiEndpoint:  "loki-read.loki:3100",
			expectedError: true,
		},
		{
			name:          "case 5: an endpoint which is not an HTTP one",
			lokiEndpoint:  "ftp://loki-read.loki",
			expectedError: true,
		},
		{
			name:          "case 6: an endpoint without host",
			lokiEndpoint:  "http:///loki",
			expectedError: true,
		},
		{
			name:          "case 7: an endpoint which is not a URL",
			lokiEndpoint:  "http://loki-read.loki:port",
			expectedError: true,
		},
	}

	for i, tc := range testCases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Log(tc.name)

			p, err := newProxy(config{lokiEndpoint: tc.lokiEndpoint})

			switch {
			case err == nil && tc.expectedError:
				t.Fatalf("error == nil, want non-nil")
			case err != nil && !tc.expectedError:
				t.Fatalf("error == %#v, want nil", err)
			case err != nil:
				return
			}

			if p.queryURL != tc.expectedQueryURL {
				t.Fatalf("query URL == %q, want %q", p.queryURL, tc.expectedQueryURL)
			}
		})
	}
}

// startProxy starts a proxy which gets its logs from loki in batches of
// batchSize entries, or of the default size if that is 0. It returns the URL
// of the proxy.
func startProxy(t *testing.T, loki http.Handler, batchSize int) string {
	t.Helper()

	lokiServer := httptest.NewServer(loki)
	t.Cleanup(lokiServer.Close)

	p, err := newProxy(config{lokiEndpoint: lokiServer.URL, orgID: tenant, since: 120 * time.Hour})
	if err != nil {
		t.Fatalf("error == %#v, want nil", err)
	}
	if batchSize != 0 {
		p.batchSize = batchSize
	}

	server := httptest.NewServer(p.handler())
	t.Cleanup(server.Close)

	return server.URL
}

// get sends a request without body and returns the response along with its
// body.
func get(t *testing.T, method, url string) (*http.Response, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Fatalf("error == %#v, want nil", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("error == %#v, want nil", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("error == %#v, want nil", err)
	}

	return resp, string(body)
}

// streamsResponse returns the response of Loki with the given entries in one
// stream, whatever was asked for.
func streamsResponse(entries ...entry) string {
	values := make([]string, 0, len(entries))
	for _, e := range entries {
		values = append(values, fmt.Sprintf("[%q,%q]", strconv.FormatInt(e.timestamp, 10), e.line))
	}

	return `{"status":"success","data":{"resultType":"streams","result":[{"stream":{},"values":[` + strings.Join(values, ",") + `]}]}}`
}

// labels returns the labels the proxy selects the streams of a container by.
func labels(container, namespace, pod string) map[string]string {
	return map[string]string{"container": container, "namespace": namespace, "pod": pod}
}

// logs returns a stream of the container whose logs are served at logsPath.
// As in a real Loki, the output it stems from sets it apart from the other
// streams of the container.
func logs(output string, entries ...entry) lokiStream {
	l := labels("step-build", "ci", "build-pod")
	l["stream"] = output

	return lokiStream{labels: l, entries: entries}
}

// at returns an entry logged the given duration after the TaskRun started.
func at(sinceStart time.Duration, line string) entry {
	return entry{timestamp: runStart.Add(sinceStart).UnixNano(), line: line}
}

// ago returns an entry logged the given duration before now.
func ago(age time.Duration, line string) entry {
	return entry{timestamp: time.Now().Add(-age).UnixNano(), line: line}
}

// lokiStream is a stream of log entries kept by a fakeLoki. The entries are
// ordered by time.
type lokiStream struct {
	labels  map[string]string
	entries []entry
}

// fakeLoki answers queries for logs the way Loki does, as far as it matters
// to the proxy.
type fakeLoki struct {
	streams []lokiStream
	// respond, if set, is asked to answer the n-th query in place of the
	// fake, and tells whether it did.
	respond func(n int, w http.ResponseWriter) bool

	mutex   sync.Mutex
	queries int
}

// setStreams replaces the streams of the fake.
func (l *fakeLoki) setStreams(streams []lokiStream) {
	l.mutex.Lock()
	defer l.mutex.Unlock()

	l.streams = streams
}

// selector is the only form of LogQL query the fake understands, which is the
// one the proxy uses. As in LogQL, the values are quoted strings.
var selector = regexp.MustCompile(`^\{container=("(?:[^"\\]|\\.)*"),namespace=("(?:[^"\\]|\\.)*"),pod=("(?:[^"\\]|\\.)*")\}$`)

func (l *fakeLoki) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.mutex.Lock()
	l.queries++
	n := l.queries
	streams := l.streams
	l.mutex.Unlock()

	if l.respond != nil && l.respond(n, w) {
		return
	}
	if r.Method != http.MethodGet || r.URL.Path != queryPath {
		http.NotFound(w, r)
		return
	}

	parameters := r.URL.Query()
	start, startErr := strconv.ParseInt(parameters.Get("start"), 10, 64)
	end, endErr := strconv.ParseInt(parameters.Get("end"), 10, 64)
	limit, limitErr := strconv.Atoi(parameters.Get("limit"))
	matchers := selector.FindStringSubmatch(parameters.Get("query"))
	if startErr != nil || endErr != nil || limitErr != nil || matchers == nil ||
		end < start || limit <= 0 || !strings.EqualFold(parameters.Get("direction"), "forward") {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var selected [3]string
	for i := range selected {
		value, err := strconv.Unquote(matchers[i+1])
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		selected[i] = value
	}

	// Loki returns the first entries of all selected streams taken together,
	// as many as asked for, each in its stream.
	type hit struct {
		stream int
		entry  entry
	}
	var hits []hit
	for i, s := range streams {
		if s.labels["container"] != selected[0] || s.labels["namespace"] != selected[1] || s.labels["pod"] != selected[2] {
			continue
		}
		for _, e := range s.entries {
			if e.timestamp >= start && e.timestamp < end {
				hits = append(hits, hit{stream: i, entry: e})
			}
		}
	}
	slices.SortStableFunc(hits, func(a, b hit) int {
		return cmp.Compare(a.entry.timestamp, b.entry.timestamp)
	})
	hits = hits[:min(len(hits), limit)]

	type result struct {
		Stream map[string]string `json:"stream"`
		Values [][2]string       `json:"values"`
	}
	values := map[int][][2]string{}
	for _, h := range hits {
		values[h.stream] = append(values[h.stream], [2]string{strconv.FormatInt(h.entry.timestamp, 10), h.entry.line})
	}
	results := []result{}
	for i, s := range streams {
		if len(values[i]) > 0 {
			results = append(results, result{Stream: s.labels, Values: values[i]})
		}
	}

	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "streams",
			"result":     results,
			"stats":      map[string]any{},
		},
	})
}
