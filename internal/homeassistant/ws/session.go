package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Defaults for Options.
const (
	DefaultHandshakeTimeout  = 10 * time.Second
	DefaultWriteTimeout      = 10 * time.Second
	DefaultReadTimeout       = 90 * time.Second
	DefaultPingInterval      = 30 * time.Second
	DefaultPongTimeout       = 15 * time.Second
	DefaultHandshakeMaxBytes = 64 << 10
	DefaultMaxMessageBytes   = 16 << 20
)

// Options configures Dial. Zero values select the defaults above.
type Options struct {
	Endpoint string
	// Token is the long-lived access token. It is sent once and never logged.
	Token string
	// HandshakeTimeout bounds the dial plus the authentication exchange.
	HandshakeTimeout time.Duration
	WriteTimeout     time.Duration
	// ReadTimeout is the idle limit once running: any frame (event or pong)
	// resets it. PingInterval keeps a quiet connection from hitting it.
	ReadTimeout  time.Duration
	PingInterval time.Duration
	// PongTimeout is how long a ping may wait for its pong.
	PongTimeout time.Duration
	// HandshakeMaxBytes limits messages before authentication succeeds;
	// MaxMessageBytes limits messages afterwards (get_states can be large).
	HandshakeMaxBytes int64
	MaxMessageBytes   int64
	// OnConnected is called once the WebSocket upgrade succeeded, before
	// authentication starts, so callers can report the transport layer.
	OnConnected func()
	// Dialer overrides the dialer (tests).
	Dialer *websocket.Dialer
}

func (o *Options) defaults() {
	set := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	set(&o.HandshakeTimeout, DefaultHandshakeTimeout)
	set(&o.WriteTimeout, DefaultWriteTimeout)
	set(&o.ReadTimeout, DefaultReadTimeout)
	set(&o.PingInterval, DefaultPingInterval)
	set(&o.PongTimeout, DefaultPongTimeout)
	if o.HandshakeMaxBytes <= 0 {
		o.HandshakeMaxBytes = DefaultHandshakeMaxBytes
	}
	if o.MaxMessageBytes <= 0 {
		o.MaxMessageBytes = DefaultMaxMessageBytes
	}
}

// Handler receives raw event payloads (the "event" object) on the reader
// goroutine. It must be fast and must not block: a slow handler stalls the
// socket, and Home Assistant disconnects clients that fall behind.
type Handler func(subscriptionID int64, event json.RawMessage)

type reply struct {
	result json.RawMessage
	err    error
}

// Session is one authenticated connection. It owns exactly two goroutines,
// the reader and the pinger, both ended by Close or by a connection failure.
type Session struct {
	conn      *websocket.Conn
	opts      Options
	haVersion string

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan reply
	err     error

	handler    Handler
	done       chan struct{}
	failOnce   sync.Once
	wg         sync.WaitGroup
	lastFrame  atomic.Int64
	unexpected atomic.Uint64
}

// Dial connects, performs the authentication handshake and returns an
// authenticated but not yet running Session. Errors wrap ErrTransport,
// ErrAuthInvalid or ErrProtocol.
func Dial(ctx context.Context, opts Options) (*Session, error) {
	if opts.Endpoint == "" {
		return nil, fmt.Errorf("%w: no endpoint", ErrTransport)
	}
	opts.defaults()
	hctx, cancel := context.WithTimeout(ctx, opts.HandshakeTimeout)
	defer cancel()

	dialer := opts.Dialer
	if dialer == nil {
		dialer = &websocket.Dialer{HandshakeTimeout: opts.HandshakeTimeout}
	}
	conn, resp, err := dialer.DialContext(hctx, opts.Endpoint, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: connect failed", ErrTransport)
	}
	// Honor cancellation during the blocking handshake reads by closing the socket.
	stop := context.AfterFunc(hctx, func() { _ = conn.Close() })
	s := &Session{conn: conn, opts: opts, pending: make(map[int64]chan reply), done: make(chan struct{})}
	if err := s.handshake(hctx); err != nil {
		stop()
		_ = conn.Close()
		switch {
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case errors.Is(hctx.Err(), context.DeadlineExceeded):
			// The read deadline and the close-on-expiry race; either way the
			// peer did not finish the handshake in time.
			return nil, fmt.Errorf("%w: handshake did not complete", ErrTimeout)
		}
		return nil, err
	}
	if !stop() {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: handshake aborted", ErrTransport)
	}
	conn.SetReadLimit(opts.MaxMessageBytes)
	_ = conn.SetReadDeadline(time.Time{})
	s.lastFrame.Store(time.Now().UnixNano())
	return s, nil
}

func (s *Session) handshake(ctx context.Context) error {
	if s.opts.OnConnected != nil {
		s.opts.OnConnected()
	}
	s.conn.SetReadLimit(s.opts.HandshakeMaxBytes)
	deadline, _ := ctx.Deadline()
	_ = s.conn.SetReadDeadline(deadline)

	first, err := s.readHandshakeFrame()
	if err != nil {
		return err
	}
	if first.Type != typeAuthRequired {
		return fmt.Errorf("%w: expected %s", ErrProtocol, typeAuthRequired)
	}
	if err := s.write(map[string]any{"type": typeAuth, "access_token": s.opts.Token}); err != nil {
		return fmt.Errorf("%w: send authentication failed", ErrTransport)
	}
	answer, err := s.readHandshakeFrame()
	if err != nil {
		return err
	}
	switch answer.Type {
	case typeAuthOK:
		s.haVersion = answer.HAVersion
		return nil
	case typeAuthInvalid:
		return ErrAuthInvalid
	default:
		return fmt.Errorf("%w: unexpected answer to authentication", ErrProtocol)
	}
}

func (s *Session) readHandshakeFrame() (frame, error) {
	typ, data, err := s.conn.ReadMessage()
	if err != nil {
		return frame{}, classifyReadError(err)
	}
	if typ != websocket.TextMessage {
		return frame{}, fmt.Errorf("%w: non-text frame", ErrProtocol)
	}
	return decodeFrame(data)
}

// classifyReadError maps socket errors onto the package's error classes
// without echoing anything from the peer.
func classifyReadError(err error) error {
	var closeErr *websocket.CloseError
	var netErr net.Error
	switch {
	case errors.Is(err, websocket.ErrReadLimit):
		return fmt.Errorf("%w: message exceeds the size limit", ErrProtocol)
	case errors.As(err, &closeErr):
		return fmt.Errorf("%w: closed by peer (code %d)", ErrTransport, closeErr.Code)
	case errors.As(err, &netErr) && netErr.Timeout():
		return fmt.Errorf("%w: read timed out", ErrTimeout)
	default:
		return fmt.Errorf("%w: read failed", ErrTransport)
	}
}

// HAVersion is the Home Assistant version announced during authentication.
func (s *Session) HAVersion() string { return s.haVersion }

// LastFrame is when the last valid frame (event, result or pong) arrived.
func (s *Session) LastFrame() time.Time { return time.Unix(0, s.lastFrame.Load()) }

// Unexpected counts messages the session did not recognize or could not match.
func (s *Session) Unexpected() uint64 { return s.unexpected.Load() }

// Done is closed when the session ended, for any reason.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err is the reason the session ended (nil while it is running).
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Start runs the reader and the pinger. handler may be nil.
func (s *Session) Start(handler Handler) {
	s.handler = handler
	s.wg.Add(2)
	go s.readLoop()
	go s.pingLoop()
}

// Close ends the session (a graceful close frame is attempted) and waits for
// both goroutines to exit. It is idempotent and safe to call after a failure.
func (s *Session) Close() {
	s.shutdown(ErrClosed)
	s.wg.Wait()
}

func (s *Session) shutdown(cause error) {
	s.failOnce.Do(func() {
		s.mu.Lock()
		s.err = cause
		s.mu.Unlock()
		if errors.Is(cause, ErrClosed) {
			msg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
			_ = s.conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(time.Second))
		}
		close(s.done)
		_ = s.conn.Close()
	})
}

func (s *Session) readLoop() {
	defer s.wg.Done()
	for {
		_ = s.conn.SetReadDeadline(time.Now().Add(s.opts.ReadTimeout))
		typ, data, err := s.conn.ReadMessage()
		if err != nil {
			s.shutdown(classifyReadError(err))
			return
		}
		if typ != websocket.TextMessage {
			s.shutdown(fmt.Errorf("%w: non-text frame", ErrProtocol))
			return
		}
		f, err := decodeFrame(data)
		if err != nil {
			s.shutdown(err)
			return
		}
		s.lastFrame.Store(time.Now().UnixNano())
		switch f.Type {
		case typeResult, typePong:
			s.deliver(f)
		case typeEvent:
			if s.handler != nil {
				s.handler(f.ID, f.Event)
			}
		default:
			s.unexpected.Add(1)
		}
	}
}

func (s *Session) deliver(f frame) {
	s.mu.Lock()
	ch, ok := s.pending[f.ID]
	delete(s.pending, f.ID)
	s.mu.Unlock()
	if !ok {
		s.unexpected.Add(1)
		return
	}
	if f.Success != nil && !*f.Success {
		e := &RejectedError{}
		if f.Error != nil {
			e.Code, e.Message = f.Error.Code, f.Error.Message
		}
		ch <- reply{err: e}
		return
	}
	ch <- reply{result: f.Result}
}

func (s *Session) pingLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.opts.PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), s.opts.PongTimeout)
			_, err := s.Call(ctx, typePing, nil)
			cancel()
			if errors.Is(err, context.DeadlineExceeded) {
				s.shutdown(fmt.Errorf("%w: no pong", ErrTimeout))
				return
			}
			if err != nil {
				return
			}
		}
	}
}

// Call sends a command and waits for its result. Home Assistant requires a
// strictly increasing id per connection; results are matched by id, so calls
// may be made concurrently.
func (s *Session) Call(ctx context.Context, typ string, params map[string]any) (json.RawMessage, error) {
	ch := make(chan reply, 1)
	s.mu.Lock()
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return nil, err
	}
	s.nextID++
	id := s.nextID
	s.pending[id] = ch
	s.mu.Unlock()

	if err := s.write(command(id, typ, params)); err != nil {
		s.forget(id)
		s.shutdown(fmt.Errorf("%w: write failed", ErrTransport))
		return nil, s.Err()
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-ctx.Done():
		s.forget(id)
		return nil, ctx.Err()
	case <-s.done:
		s.forget(id)
		return nil, s.Err()
	}
}

func (s *Session) forget(id int64) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}

// write serializes writers (gorilla allows one at a time) and bounds each write.
func (s *Session) write(v any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(s.opts.WriteTimeout))
	return s.conn.WriteJSON(v)
}
