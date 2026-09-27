package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/firmfact/cli/internal/httpx"
	"github.com/firmfact/cli/internal/ui"
)

// Body is a request body too large to hold in memory, such as the files of
// an upload, which SendBody streams. Each attempt opens it afresh, so a
// retry after a busy server or a renewed token sends the same bytes from
// the first one.
//
// Its bytes never reach the debug log. A Body that is a fmt.Stringer is
// described there by what String returns instead.
type Body interface {
	// Open returns the body from its first byte. The client closes it,
	// perhaps from another goroutine while a Read is under way.
	Open() (io.ReadCloser, error)
	// Size is the body's length in bytes. The request declares it up
	// front, so the server can turn away a body too large for it before
	// taking any of it.
	Size() int64
}

// Pace bounds a request that streams its body (SendBody). Its limit grows
// with the body: the client's usual limit for a request, plus the time the
// body takes to send at Rate, plus Answer.
type Pace struct {
	// Rate is the slowest sending speed the limit allows for, in bytes a
	// second.
	Rate int64
	// Stall is how long the body may go without moving before the request
	// is given up. A connection that takes nothing more fails in a minute,
	// not at the end of a limit sized for a slow line.
	Stall time.Duration
	// Answer is how long the server may take to answer once it has the
	// whole body: the firmfact service scans each file of an upload for
	// viruses before it answers.
	Answer time.Duration
}

// DefaultPace allows for a line of about 1 Mbit/s up, such as a poor mobile
// or hotel connection, on which 100 MB takes under 14 minutes.
var DefaultPace = Pace{Rate: 128 << 10, Stall: time.Minute, Answer: 5 * time.Minute}

// pace is c.Pace with DefaultPace's value for each field left at zero.
func (c *Client) pace() Pace {
	p := c.Pace
	if p.Rate <= 0 {
		p.Rate = DefaultPace.Rate
	}
	if p.Stall <= 0 {
		p.Stall = DefaultPace.Stall
	}
	if p.Answer <= 0 {
		p.Answer = DefaultPace.Answer
	}
	return p
}

// StreamLimit is how long one attempt at sending a body of size bytes may
// take in all, from the first byte to the last of the answer.
func (c *Client) StreamLimit(size int64) time.Duration {
	p := c.pace()
	sending := time.Duration((max(size, 0)+p.Rate-1)/p.Rate) * time.Second
	return c.HTTP.Timeout() + sending + p.Answer
}

// expectContinueFrom is the size from which a body waits for the server's
// go-ahead (Expect: 100-continue) before it is sent, as curl's does: a
// server, or a proxy on the way, that turns a request away by its headers
// alone (a body over its limit, a token it refuses) then says so before
// the body is sent rather than after. One that ignores the header costs
// the transport's ExpectContinueTimeout, a second, once a request.
const expectContinueFrom = 1 << 20

// SendBody is Send for a body too large to hold in memory. It differs from
// Send in four ways:
//
//   - each attempt opens body afresh (see Body);
//   - an attempt may take StreamLimit(body.Size()) in all rather than the
//     client's usual limit, but gives up once the body stops moving for
//     Pace.Stall, and waits Pace.Answer for the answer once all of it is
//     sent;
//   - with auth, a token that would run out within that limit is renewed
//     before anything is sent (see ensureFreshToken);
//   - a request that gets no answer returns a *StreamError, which says how
//     much of the body was sent, and an error reading the body itself (a
//     file that changed, say) is returned as it is.
//
// Only a busy answer (429, 503) and a 401 are sent again, as with Send:
// the server turned those away without acting on them. A connection that
// broke is not, since the server may have had the whole body; the
// StreamError says whether it can have.
func (c *Client) SendBody(ctx context.Context, method, path string, body Body, headers map[string]string, auth bool) (*http.Response, error) {
	s := &stream{body: body, pace: c.pace(), limit: c.StreamLimit(body.Size())}
	return c.send(ctx, headers, auth, s.limit, func(ctx context.Context) (*http.Response, error) {
		return c.doStream(ctx, method, path, s, headers, auth)
	})
}

// stream is a body on its way, and its limits.
type stream struct {
	body  Body
	pace  Pace
	limit time.Duration
}

// The reasons doStream gives up on an attempt.
var (
	errStalled = errors.New("the body stopped moving")
	errTooSlow = errors.New("the attempt ran out of time")
)

// doStream makes one attempt at sending s.
func (c *Client) doStream(ctx context.Context, method, path string, s *stream, headers map[string]string, auth bool) (*http.Response, error) {
	size := s.body.Size()
	// The attempt has a deadline of its own (s.limit), which httpx.Client.Do
	// takes in place of the client's usual limit, and is cancelled when the
	// body stalls. It lives until the answer's body is closed.
	sctx, stall := context.WithCancelCause(ctx)
	actx, timeUp := context.WithTimeoutCause(sctx, s.limit, errTooSlow)
	w := &watch{stall: s.pace.Stall, onStall: func() { stall(errStalled) }}
	end := func() {
		w.stop()
		timeUp()
		stall(nil)
	}
	req, err := c.newRequest(actx, method, path, nil, headers, auth)
	if err != nil {
		end()
		return nil, err
	}
	req.ContentLength = size
	req.Body = http.NoBody
	if size > 0 {
		// GetBody lets the transport send the body again on a connection
		// that closed before any of it went out.
		req.GetBody = func() (io.ReadCloser, error) { return w.open(s.body) }
		if req.Body, err = req.GetBody(); err != nil {
			end()
			return nil, err
		}
	}
	if size >= expectContinueFrom {
		req.Header.Set("Expect", "100-continue")
	}
	c.logRequest(req, nil, s.body)
	start := time.Now()
	resp, err := c.HTTP.WaitingAtLeast(s.pace.Answer).Do(req)
	c.logAnswer(req, resp, err, time.Since(start))
	// Whatever the transport still reads of the body from here is no
	// reason to give up on the answer.
	w.stop()
	if err != nil {
		cause := context.Cause(actx)
		end()
		return nil, c.streamError(ctx, req, s, w, cause, err)
	}
	resp.Body = &closer{ReadCloser: resp.Body, after: end}
	return resp, nil
}

// streamError is what the user hears about an attempt at sending s that got
// no answer, after cause stopped it (nil when nothing did).
func (c *Client) streamError(ctx context.Context, req *http.Request, s *stream, w *watch, cause, err error) error {
	// Ctrl-C, or a deadline of the caller's own.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	// The body could not be read: a local problem, not the connection's.
	if readErr := w.readErr(); readErr != nil {
		return readErr
	}
	host, size, sent := req.URL.Host, s.body.Size(), w.sent()
	var netErr *httpx.Error
	switch {
	case errors.Is(cause, errStalled):
		err = httpx.NewError(httpx.Stalled, host, err,
			fmt.Sprintf("%s stopped taking the upload: nothing more went out for %s", host, httpx.Span(s.pace.Stall)))
	case errors.Is(cause, errTooSlow) && sent < size:
		err = httpx.NewError(httpx.AnswerTimeout, host, err,
			fmt.Sprintf("the upload to %s did not finish within %s", host, httpx.Span(s.limit)))
	case errors.Is(cause, errTooSlow):
		err = httpx.NewError(httpx.AnswerTimeout, host, err,
			fmt.Sprintf("%s did not finish answering within %s", host, httpx.Span(s.limit)))
	case errors.As(err, &netErr) && netErr.Kind == httpx.Unreachable && sent > 0:
		// httpx says "could not reach" the host, but it was reached: the
		// connection broke on the way.
		err = httpx.NewError(httpx.Unreachable, netErr.Host, netErr.Err,
			fmt.Sprintf("the connection to %s broke: %s", netErr.Host, httpx.Cause(netErr.Err)))
	}
	return &StreamError{Err: err, Sent: sent, Size: size}
}

// StreamError is a request with a streamed body (SendBody) that got no
// answer: the connection broke, the body stopped moving, or the attempt ran
// out of time. It unwraps to the *httpx.Error that says why.
//
// Sent is how much of the body the CLI handed to the connection; less of
// it may have reached the server. The firmfact service acts on an upload
// only once it has all of it, so when Sent is less than Size nothing was
// stored and the request can be sent again; when all of it was sent, the
// server may have acted on it before the answer was lost.
type StreamError struct {
	Err        error
	Sent, Size int64
}

func (e *StreamError) Error() string {
	switch {
	case e.Sent <= 0:
		return e.Err.Error()
	case e.Sent < e.Size:
		return fmt.Sprintf("%s (%s of %s sent)", e.Err, ui.Bytes(e.Sent), ui.Bytes(e.Size))
	}
	return fmt.Sprintf("%s (all %s sent)", e.Err, ui.Bytes(e.Size))
}

func (e *StreamError) Unwrap() error { return e.Err }

// AllSent reports whether the whole body went out, so that the server may
// have acted on the request although no answer came.
func (e *StreamError) AllSent() bool { return e.Sent >= e.Size }

// closer runs after once the body it wraps is closed.
type closer struct {
	io.ReadCloser
	once  sync.Once
	after func()
}

func (c *closer) Close() error {
	err := c.ReadCloser.Close()
	c.once.Do(c.after)
	return err
}

// watch follows a streamed body on its way out: how much of it has gone,
// whether it has stopped moving, and whether reading it failed.
type watch struct {
	stall   time.Duration
	onStall func()

	mu      sync.Mutex
	timer   *time.Timer
	done    bool  // the body is all sent, or the attempt is over
	n       int64 // bytes read from the latest opening
	failure error // the body's own read error
}

// open opens b for the transport, counting from nought again: the
// transport may open it more than once.
func (w *watch) open(b Body) (io.ReadCloser, error) {
	rc, err := b.Open()
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.n = 0
	w.mu.Unlock()
	return &watched{rc: rc, w: w}, nil
}

// read records a Read of n bytes that returned err. The stall timer starts
// with the first read, so the wait for the connection and for the server's
// go-ahead is not counted, and every read that moves the body starts it
// again.
func (w *watch) read(n int, err error, closed bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.n += int64(n)
	switch {
	case w.done:
	case errors.Is(err, io.EOF):
		w.stopLocked()
	case err != nil:
		// Once the transport has closed the body, its reads fail because
		// of that, which says nothing about the body.
		if !closed {
			w.failure = err
		}
		w.stopLocked()
	case w.timer == nil:
		w.timer = time.AfterFunc(w.stall, w.stalled)
	default:
		w.timer.Reset(w.stall)
	}
}

func (w *watch) stalled() {
	w.mu.Lock()
	fire := !w.done
	w.done = true
	w.mu.Unlock()
	if fire {
		w.onStall()
	}
}

// stop ends the watch: the attempt is over, or has its answer.
func (w *watch) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopLocked()
}

func (w *watch) stopLocked() {
	w.done = true
	if w.timer != nil {
		w.timer.Stop()
	}
}

func (w *watch) sent() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}

func (w *watch) readErr() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.failure
}

// watched is a body the transport reads through its watch.
type watched struct {
	rc     io.ReadCloser
	w      *watch
	closed atomic.Bool
}

func (r *watched) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	r.w.read(n, err, r.closed.Load())
	return n, err
}

func (r *watched) Close() error {
	r.closed.Store(true)
	return r.rc.Close()
}
