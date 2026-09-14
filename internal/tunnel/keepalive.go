package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

const (
	// PingInterval is how often each side emits an app-level ping.
	PingInterval = 12 * time.Second
	// PongTimeout is how long a ping may go unanswered before the link is dead.
	PongTimeout = 45 * time.Second
	// ReadIdleTimeout is the TCP read deadline, refreshed on every inbound frame.
	ReadIdleTimeout = 60 * time.Second
	// TCPKeepAlivePeriod is the OS-level TCP keepalive idle/probe interval.
	TCPKeepAlivePeriod = 15 * time.Second
	// ReconnectMin is the first reconnect delay after a drop.
	ReconnectMin = 500 * time.Millisecond
	// ReconnectMax caps exponential reconnect backoff.
	ReconnectMax = 10 * time.Second
	// HealthyResetAfter resets reconnect backoff once a session lasts this long.
	HealthyResetAfter = 10 * time.Second
	writeIdleTimeout  = 30 * time.Second
)

// ErrPongTimeout is returned when a keepalive ping is not answered in time.
var ErrPongTimeout = errors.New("tunnel keepalive: missing pong")

// KeepaliveConfig tunes application-level ping/pong.
type KeepaliveConfig struct {
	Interval time.Duration
	PongWait time.Duration
}

// DefaultKeepalive is the production ping interval and pong deadline.
func DefaultKeepalive() KeepaliveConfig {
	return KeepaliveConfig{Interval: PingInterval, PongWait: PongTimeout}
}

func (c KeepaliveConfig) normalized() KeepaliveConfig {
	if c.Interval <= 0 {
		c.Interval = PingInterval
	}
	if c.PongWait <= 0 {
		c.PongWait = PongTimeout
	}
	return c
}

// EnableTCPKeepAlive turns on OS probes with a short period (NAT-friendly).
func EnableTCPKeepAlive(c net.Conn) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return
	}
	_ = tc.SetKeepAlive(true)
	_ = tc.SetKeepAlivePeriod(TCPKeepAlivePeriod)
}

// Dialer returns a TCP dialer with a short OS keepalive period.
func Dialer(timeout time.Duration) net.Dialer {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return net.Dialer{
		Timeout:   timeout,
		KeepAlive: TCPKeepAlivePeriod,
		KeepAliveConfig: net.KeepAliveConfig{
			Enable:   true,
			Idle:     TCPKeepAlivePeriod,
			Interval: TCPKeepAlivePeriod,
			Count:    3,
		},
	}
}

// SetReadIdle sets an absolute read deadline idle in the future.
func SetReadIdle(c net.Conn, idle time.Duration) error {
	if c == nil {
		return nil
	}
	return c.SetReadDeadline(time.Now().Add(idle))
}

// RefreshReadDeadline extends the read deadline after any inbound frame
// (including ping/pong) so idle NAT mappings stay aligned with keepalive.
func RefreshReadDeadline(c net.Conn) error {
	return SetReadIdle(c, ReadIdleTimeout)
}

// ReadFrameRefreshing reads one frame and refreshes the read deadline before
// and after so ping/pong count as activity.
func ReadFrameRefreshing(c net.Conn, r io.Reader, f *Frame) error {
	return ReadFrameRefreshingIdle(c, r, f, ReadIdleTimeout)
}

// ReadFrameRefreshingIdle is ReadFrameRefreshing with a caller-chosen idle time.
func ReadFrameRefreshingIdle(c net.Conn, r io.Reader, f *Frame, idle time.Duration) error {
	if err := SetReadIdle(c, idle); err != nil {
		return err
	}
	if err := ReadFrame(r, f); err != nil {
		return err
	}
	return SetReadIdle(c, idle)
}

// ClassifyReadError annotates idle timeouts so reconnect logs are explicit.
func ClassifyReadError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrPongTimeout) {
		return err
	}
	if IsTimeout(err) {
		return fmt.Errorf("read idle timeout (%s): %w", ReadIdleTimeout, err)
	}
	return err
}

// IsTimeout reports whether err is a network timeout (including deadlines).
func IsTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// NextReconnectBackoff doubles cur, starting at ReconnectMin and capping at ReconnectMax.
func NextReconnectBackoff(cur time.Duration) time.Duration {
	if cur < ReconnectMin {
		return ReconnectMin
	}
	next := cur * 2
	if next > ReconnectMax {
		return ReconnectMax
	}
	return next
}

// Pinger tracks whether our keepalive pings have been answered.
type Pinger struct {
	mu       sync.Mutex
	lastPong time.Time
	pingSent bool
}

// NewPinger starts a healthy keepalive window from now.
func NewPinger() *Pinger {
	return &Pinger{lastPong: time.Now()}
}

// OnPong records a pong (or equivalent proof that our ping was answered).
func (p *Pinger) OnPong() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastPong = time.Now()
	p.pingSent = false
}

// MarkPing records that we have an outstanding ping.
func (p *Pinger) MarkPing() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pingSent = true
}

// TimedOut reports whether a ping has gone unanswered longer than PongTimeout.
func (p *Pinger) TimedOut() bool {
	return p.Overdue(PongTimeout)
}

// Overdue is TimedOut with a caller-chosen wait (used by tests and StartKeepalive).
func (p *Pinger) Overdue(wait time.Duration) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.pingSent {
		return false
	}
	return time.Since(p.lastPong) > wait
}

// StartKeepalive emits pings on Interval and calls die if no pong arrives
// within PongWait. onPong must be invoked when a pong frame is read.
func StartKeepalive(ctx context.Context, cfg KeepaliveConfig, write func(Frame) error, die func(error)) (onPong func(), stop func()) {
	cfg = cfg.normalized()
	p := NewPinger()
	ctx, cancel := context.WithCancel(ctx)
	var stopOnce sync.Once
	stop = func() { stopOnce.Do(cancel) }

	sendPing := func() bool {
		if p.Overdue(cfg.PongWait) {
			die(fmt.Errorf("%w after %s", ErrPongTimeout, cfg.PongWait))
			return false
		}
		p.MarkPing()
		if err := write(Frame{Type: TypePing, ID: pingID()}); err != nil {
			die(fmt.Errorf("tunnel keepalive ping: %w", err))
			return false
		}
		return true
	}

	go func() {
		if !sendPing() {
			return
		}
		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !sendPing() {
					return
				}
			}
		}
	}()
	return p.OnPong, stop
}

func pingID() string {
	return strconv.FormatInt(time.Now().UnixNano(), 10)
}

// SetWriteIdle is the short write deadline used for tunnel frames.
func SetWriteIdle(c net.Conn) error {
	if c == nil {
		return nil
	}
	return c.SetWriteDeadline(time.Now().Add(writeIdleTimeout))
}

// ClearWriteDeadline removes a write deadline after a successful frame write.
func ClearWriteDeadline(c net.Conn) {
	if c == nil {
		return
	}
	_ = c.SetWriteDeadline(time.Time{})
}
