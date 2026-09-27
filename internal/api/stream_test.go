package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/firmfact/cli/internal/config"
	"github.com/firmfact/cli/internal/httpx"
)

// pattern is what a patternBody is made of. Its words are the marker the
// debug log must never show.
const pattern = "0123456789abcdef streamed-body-bytes\n"

// patternBody is a body of size bytes made up as it is read, so that a test
// can send tens of megabytes without holding them, and a server can check
// that every byte arrived.
type patternBody struct {
	size  int64
	opens atomic.Int32
	// failAt, when set, is where a read fails with errBodyBroke.
	failAt int64
}

var errBodyBroke = errors.New("the disk went away")

func (b *patternBody) Open() (io.ReadCloser, error) {
	b.opens.Add(1)
	return io.NopCloser(&patternReader{size: b.size, failAt: b.failAt}), nil
}

func (b *patternBody) Size() int64    { return b.size }
func (b *patternBody) String() string { return "a test pattern" }

type patternReader struct {
	pos, size, failAt int64
}

func (r *patternReader) Read(p []byte) (int, error) {
	if r.failAt > 0 && r.pos >= r.failAt {
		return 0, errBodyBroke
	}
	if r.pos >= r.size {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), r.size-r.pos))
	for i := range n {
		p[i] = pattern[(r.pos+int64(i))%int64(len(pattern))]
	}
	r.pos += int64(n)
	return n, nil
}

// readPattern reads a request body and says whether it is the whole of a
// patternBody of size bytes.
func readPattern(r io.Reader, size int64) error {
	buf := make([]byte, 32<<10)
	var pos int64
	for {
		n, err := r.Read(buf)
		for i := range n {
			if buf[i] != pattern[(pos+int64(i))%int64(len(pattern))] {
				return fmt.Errorf("byte %d differs", pos+int64(i))
			}
		}
		pos += int64(n)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("after %d bytes: %w", pos, err)
		}
	}
	if pos != size {
		return fmt.Errorf("got %d bytes, want %d", pos, size)
	}
	return nil
}

// sendFailing sends body to the upload path for a test that expects no
// answer, and closes any that comes.
func sendFailing(ctx context.Context, c *Client, body Body) error {
	resp, err := c.SendBody(ctx, http.MethodPost, uploadPath, body, nil, true)
	if resp != nil {
		resp.Body.Close()
	}
	return err
}

// signedIn is a client for host with a token good for another hour.
func signedIn(t *testing.T, host string) *Client {
	t.Helper()
	useFileStore(t)
	c := New(host)
	if err := c.SetToken(&config.Token{AccessToken: "at-upload", RefreshToken: "rt-upload", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return c
}

const uploadPath = "/api/v1/cli/workspaces/Demo/documents"

// A streamed body arrives whole, with its length declared, and from a
// megabyte on it waits for the server's go-ahead first. The debug log says
// how large it was and what it was, never what was in it.
func TestStreamedBodyArrivesWhole(t *testing.T) {
	const size = 2 << 20
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength != size || r.Header.Get("Expect") != "100-continue" || r.Header.Get("Content-Type") != "application/x-test" {
			t.Errorf("Content-Length %d, Expect %q, Content-Type %q", r.ContentLength, r.Header.Get("Expect"), r.Header.Get("Content-Type"))
		}
		got.Store(fmt.Sprint(readPattern(r.Body, size)))
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"data":{}}`)
	}))
	defer srv.Close()
	c := signedIn(t, srv.URL)
	var log bytes.Buffer
	c.Debug = NewDebugLog(&log)

	body := &patternBody{size: size}
	resp, err := c.SendBody(context.Background(), http.MethodPost, uploadPath, body, map[string]string{"Content-Type": "application/x-test"}, true)
	if err != nil {
		t.Fatalf("upload failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || got.Load() != "<nil>" {
		t.Errorf("status %d, body check: %v", resp.StatusCode, got.Load())
	}
	if strings.Contains(log.String(), "streamed-body-bytes") {
		t.Errorf("the debug log shows the body:\n%s", log.String())
	}
	for _, want := range []string{"POST " + srv.URL + uploadPath, "(a body of 2 MB, not shown) a test pattern", "Expect: 100-continue", "201 Created"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the debug log lacks %q:\n%s", want, log.String())
		}
	}
}

// A server that takes the body slowly, but steadily, gets all of it, even
// when that takes longer than the client's usual limit for a request: the
// limit grows with the body.
func TestSlowServerGetsTheWholeUpload(t *testing.T) {
	const size = 1 << 20
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		for {
			n, err := io.CopyN(&buf, r.Body, 64<<10)
			if n == 0 || err != nil {
				break
			}
			time.Sleep(40 * time.Millisecond)
		}
		if err := readPattern(&buf, size); err != nil {
			t.Errorf("body: %v", err)
		}
		io.WriteString(w, `{"data":{}}`)
	}))
	defer srv.Close()
	c := signedIn(t, srv.URL)
	c.HTTP = httpx.New(httpx.Options{Timeout: 300 * time.Millisecond, HeaderTimeout: 300 * time.Millisecond})
	c.Pace = Pace{Rate: 256 << 10}

	start := time.Now()
	resp, err := c.SendBody(context.Background(), http.MethodPost, uploadPath, &patternBody{size: size}, nil, true)
	if err != nil {
		t.Fatalf("upload failed: %v", err)
	}
	resp.Body.Close()
	if took := time.Since(start); took < 300*time.Millisecond {
		t.Errorf("took %s; the test needs a server slower than the usual limit", took)
	}
	if want := 300*time.Millisecond + 4*time.Second + DefaultPace.Answer; c.StreamLimit(size) != want {
		t.Errorf("limit %s, want %s", c.StreamLimit(size), want)
	}
}

// stalledServer takes the first 64 KB of a body, then no more until the
// test ends, and counts its requests.
func stalledServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	release := make(chan struct{})
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.CopyN(io.Discard, r.Body, 64<<10)
		<-release
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv, &requests
}

// A connection that stops taking the body is given up once nothing has
// moved for Pace.Stall, not at the end of a limit sized for a slow line;
// and it is not sent again, as the server may be busy with it.
func TestStalledUploadIsGivenUp(t *testing.T) {
	srv, requests := stalledServer(t)
	c := signedIn(t, srv.URL)
	c.Pace = Pace{Stall: 300 * time.Millisecond}

	// More than the buffers on the way hold, so the sending blocks.
	body := &patternBody{size: 64 << 20}
	start := time.Now()
	err := sendFailing(context.Background(), c, body)
	took := time.Since(start)
	var streamErr *StreamError
	var netErr *httpx.Error
	if !errors.As(err, &streamErr) || !errors.As(err, &netErr) || netErr.Kind != httpx.Stalled {
		t.Fatalf("want a stalled *StreamError, got %#v", err)
	}
	if streamErr.AllSent() || streamErr.Sent <= 0 {
		t.Errorf("sent %d of %d, want some but not all", streamErr.Sent, streamErr.Size)
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	if want := host + " stopped taking the upload: nothing more went out for 300ms ("; !strings.HasPrefix(err.Error(), want) || !strings.HasSuffix(err.Error(), " of 64 MB sent)") {
		t.Errorf("message %q, want it to start %q", err, want)
	}
	if took > 10*time.Second {
		t.Errorf("gave up after %s", took)
	}
	if n := requests.Load(); n != 1 || body.opens.Load() != 1 {
		t.Errorf("%d requests, %d openings of the body; want 1 each", n, body.opens.Load())
	}
}

// Ctrl-C ends an upload at once, and says nothing more about it.
func TestCancelAbortsAnUpload(t *testing.T) {
	srv, _ := stalledServer(t)
	c := signedIn(t, srv.URL)
	ctx, cancelled := cancelAfter(200 * time.Millisecond)
	err := sendFailing(ctx, c, &patternBody{size: 64 << 20})
	returned := time.Now()
	if !errors.Is(err, context.Canceled) || err.Error() != context.Canceled.Error() {
		t.Fatalf("want context.Canceled as it is, got %#v", err)
	}
	if lag := returned.Sub(<-cancelled); lag > time.Second {
		t.Errorf("returned %s after the cancel", lag)
	}
}

// A body that keeps moving, but too slowly to finish within the limit its
// size allows, is given up at that limit.
func TestUploadRunsOutOfTime(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for {
			select {
			case <-release:
				return
			case <-time.After(10 * time.Millisecond):
			}
			if _, err := io.CopyN(io.Discard, r.Body, 4<<10); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	defer close(release)
	c := signedIn(t, srv.URL)
	c.HTTP = httpx.New(httpx.Options{Timeout: 100 * time.Millisecond})
	c.Pace = Pace{Rate: 1 << 30, Answer: 200 * time.Millisecond}

	err := sendFailing(context.Background(), c, &patternBody{size: 64 << 20})
	var streamErr *StreamError
	var netErr *httpx.Error
	if !errors.As(err, &streamErr) || !errors.As(err, &netErr) || netErr.Kind != httpx.AnswerTimeout || streamErr.AllSent() {
		t.Fatalf("want an unfinished upload that ran out of time, got %#v", err)
	}
	if want := "the upload to " + strings.TrimPrefix(srv.URL, "http://") + " did not finish within 1.3s ("; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("message %q, want it to start %q", err, want)
	}
}

// A server that drops the connection part of the way through: the error
// says so, and how much went, and the upload is not sent again, since only
// the caller knows whether that is safe.
func TestDroppedUploadSaysHowFarItGot(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.CopyN(io.Discard, r.Body, 256<<10)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	defer srv.Close()
	c := signedIn(t, srv.URL)

	err := sendFailing(context.Background(), c, &patternBody{size: 64 << 20})
	var streamErr *StreamError
	var netErr *httpx.Error
	if !errors.As(err, &streamErr) || !errors.As(err, &netErr) || netErr.Kind != httpx.Unreachable {
		t.Fatalf("want a broken *StreamError, got %#v", err)
	}
	if streamErr.AllSent() || streamErr.Sent < 256<<10 {
		t.Errorf("sent %d of %d, want at least what the server read, not all", streamErr.Sent, streamErr.Size)
	}
	if want := "the connection to " + strings.TrimPrefix(srv.URL, "http://") + " broke: "; !strings.HasPrefix(err.Error(), want) || !strings.HasSuffix(err.Error(), " of 64 MB sent)") {
		t.Errorf("message %q, want it to start %q and say how much was sent", err, want)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
}

// A server that turns the body away by its headers (a proxy's size limit)
// says so before any of it is sent, and its answer is the error.
func TestEarlyRefusalOfALargeUpload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		io.WriteString(w, `{"error":"The upload is larger than 100 MB, so nothing was stored.","code":"REQUEST_TOO_LARGE"}`)
	}))
	defer srv.Close()
	c := signedIn(t, srv.URL)

	start := time.Now()
	err := sendFailing(context.Background(), c, &patternBody{size: 200 << 20})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusRequestEntityTooLarge || apiErr.Code != "REQUEST_TOO_LARGE" {
		t.Fatalf("want the 413, got %#v", err)
	}
	if err.Error() != "The upload is larger than 100 MB, so nothing was stored." {
		t.Errorf("message %q", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("took %s: the body must not be sent first", took)
	}
}

// bodyCounter is an upload endpoint that checks each body it gets, after
// answering the first ones with the statuses in first.
type bodyCounter struct {
	mu     sync.Mutex
	first  []int
	header func(http.Header)
	token  string // the access token it takes, when set
	bodies []error
	auths  []string
}

func (s *bodyCounter) serve(size int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// As Puma does: the whole body first, then the application.
		err := readPattern(r.Body, size)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.bodies = append(s.bodies, err)
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		if s.token != "" && r.Header.Get("Authorization") != "Bearer "+s.token {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":"Token expired"}`)
			return
		}
		if len(s.first) > 0 {
			status := s.first[0]
			s.first = s.first[1:]
			if s.header != nil {
				s.header(w.Header())
			}
			w.WriteHeader(status)
			io.WriteString(w, `{"error":"Rate limit exceeded"}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"data":{}}`)
	}
}

// A 429 with Retry-After is waited out and the upload sent again, from its
// first byte.
func TestRateLimitedUploadIsSentAgainWhole(t *testing.T) {
	const size = 3 << 20
	s := &bodyCounter{first: []int{http.StatusTooManyRequests}, header: retryIn("1")}
	srv := httptest.NewServer(s.serve(size))
	defer srv.Close()
	c := signedIn(t, srv.URL)
	var said bytes.Buffer
	c.Busy = &said

	body := &patternBody{size: size}
	resp, err := c.SendBody(context.Background(), http.MethodPost, uploadPath, body, nil, true)
	if err != nil {
		t.Fatalf("upload failed: %v", err)
	}
	resp.Body.Close()
	if len(s.bodies) != 2 || s.bodies[0] != nil || s.bodies[1] != nil || body.opens.Load() != 2 {
		t.Errorf("bodies %v, %d openings; want two whole ones", s.bodies, body.opens.Load())
	}
	if want := "\rServer busy; retrying in 1s...\n"; said.String() != want {
		t.Errorf("said %q, want %q", said.String(), want)
	}
}

// A 401 renews the token, and the upload is sent again, whole, with the
// new one.
func TestUploadRefusedTokenIsRenewedAndSentAgain(t *testing.T) {
	const size = 3 << 20
	s := &bodyCounter{token: "at-renewed"}
	mux := http.NewServeMux()
	mux.HandleFunc(uploadPath, s.serve(size))
	var renewals atomic.Int32
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		renewals.Add(1)
		io.WriteString(w, `{"access_token":"at-renewed","refresh_token":"rt-renewed","expires_in":3600}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := signedIn(t, srv.URL)

	resp, err := c.SendBody(context.Background(), http.MethodPost, uploadPath, &patternBody{size: size}, nil, true)
	if err != nil {
		t.Fatalf("upload failed: %v", err)
	}
	resp.Body.Close()
	if renewals.Load() != 1 || len(s.bodies) != 2 || s.bodies[0] != nil || s.bodies[1] != nil {
		t.Errorf("%d renewals, bodies %v; want one renewal and two whole bodies", renewals.Load(), s.bodies)
	}
	if want := []string{"Bearer at-upload", "Bearer at-renewed"}; fmt.Sprint(s.auths) != fmt.Sprint(want) {
		t.Errorf("tokens sent %v, want %v", s.auths, want)
	}
}

// A token that would run out while the upload is on its way is renewed
// before it is sent: the server reads the token only once it has the whole
// body, and would refuse it then. One that lasts is not.
func TestTokenIsRenewedBeforeALongUpload(t *testing.T) {
	cases := map[string]struct {
		left  time.Duration
		pace  Pace
		renew bool
	}{
		"runs out on the way": {left: 10 * time.Minute, pace: Pace{Rate: 1 << 10}, renew: true},
		"runs out waiting":    {left: 5 * time.Minute, renew: true},
		"lasts":               {left: time.Hour, renew: false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			const size = 1 << 20
			s := &bodyCounter{}
			mux := http.NewServeMux()
			mux.HandleFunc(uploadPath, s.serve(size))
			var renewals atomic.Int32
			mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
				renewals.Add(1)
				io.WriteString(w, `{"access_token":"at-renewed","refresh_token":"rt-renewed","expires_in":7200}`)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			useFileStore(t)
			c := New(srv.URL)
			if err := c.SetToken(&config.Token{AccessToken: "at-old", RefreshToken: "rt-old", ExpiresAt: time.Now().Add(tc.left)}); err != nil {
				t.Fatal(err)
			}
			c.Pace = tc.pace
			var log bytes.Buffer
			c.Debug = NewDebugLog(&log)

			resp, err := c.SendBody(context.Background(), http.MethodPost, uploadPath, &patternBody{size: size}, nil, true)
			if err != nil {
				t.Fatalf("upload failed: %v", err)
			}
			resp.Body.Close()
			wantAuth, wantRenewals := "Bearer at-old", int32(0)
			if tc.renew {
				wantAuth, wantRenewals = "Bearer at-renewed", 1
			}
			if renewals.Load() != wantRenewals || len(s.auths) != 1 || s.auths[0] != wantAuth {
				t.Errorf("%d renewals, tokens sent %v; want %d and [%s]", renewals.Load(), s.auths, wantRenewals, wantAuth)
			}
			if said := strings.Contains(log.String(), "runs out within the"); said != tc.renew {
				t.Errorf("the debug log says why it renewed: %v, want %v:\n%s", said, tc.renew, log.String())
			}
		})
	}
}

// An error reading the body itself, such as a file that went away, is the
// error: not a connection problem.
func TestBodyReadErrorIsTheError(t *testing.T) {
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(fmt.Sprint(readPattern(r.Body, 4<<20)))
	}))
	defer srv.Close()
	c := signedIn(t, srv.URL)

	err := sendFailing(context.Background(), c, &patternBody{size: 4 << 20, failAt: 1 << 20})
	var netErr *httpx.Error
	if !errors.Is(err, errBodyBroke) || errors.As(err, &netErr) {
		t.Fatalf("want the body's own error, got %#v", err)
	}
	if err.Error() != errBodyBroke.Error() {
		t.Errorf("message %q, want %q", err, errBodyBroke)
	}
	if v, _ := got.Load().(string); v == "<nil>" {
		t.Errorf("the server got a whole body")
	}
}

// StreamError says how much of the body went, and whether all of it did.
func TestStreamErrorSaysHowFarTheBodyGot(t *testing.T) {
	base := errors.New("the connection to firmfact.com broke: connection reset by peer")
	cases := []struct {
		sent, size int64
		want       string
		all        bool
	}{
		{0, 5 << 20, "the connection to firmfact.com broke: connection reset by peer", false},
		{1536 << 10, 5 << 20, "the connection to firmfact.com broke: connection reset by peer (1.5 MB of 5 MB sent)", false},
		{5 << 20, 5 << 20, "the connection to firmfact.com broke: connection reset by peer (all 5 MB sent)", true},
	}
	for _, tc := range cases {
		e := &StreamError{Err: base, Sent: tc.sent, Size: tc.size}
		if e.Error() != tc.want || e.AllSent() != tc.all || !errors.Is(e, base) {
			t.Errorf("%d of %d: %q (all sent %v), want %q (%v)", tc.sent, tc.size, e.Error(), e.AllSent(), tc.want, tc.all)
		}
	}
}
