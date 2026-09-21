// Package eslclient maintains the INBOUND ESL connections: this service
// connecting out to each FreeSWITCH node.
//
//	outbound ESL   FS -> controller :8022   one socket per call, FS initiates
//	inbound  ESL   controller -> FS :8021   originate, uuid_*, the event stream
//
// This is the second. It is a small number of long-lived control connections,
// not one per call.
//
// # Why a pool rather than dialling per command
//
// The connections are authenticated and mostly idle — they carry nothing
// between originates. Dialling per command would put a TCP handshake and an
// auth round trip in front of every one, which for an originate is time the
// caller spends listening to silence. Holding them open also means a switch
// that has gone away is noticed by the reconnect loop rather than discovered
// by the first call that needed it.
package eslclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/Adebayobenjamin/intron-sip-project/services/internal/esl"
)

// Reconnect backoff. Short at first because most failures are a restart that
// is already over; capped because a switch that has been gone for ten minutes
// will not be helped by asking more often.
const (
	minBackoff = time.Second
	maxBackoff = 30 * time.Second

	commandTimeout = 10 * time.Second
)

// ErrNoNode means no FreeSWITCH node is currently connected.
var ErrNoNode = errors.New("eslclient: no FreeSWITCH node is connected")

// Config is what the pool needs.
type Config struct {
	// Nodes are host addresses, without a port.
	Nodes []string
	Port  int

	Password  string
	Keepalive time.Duration
}

// Pool holds one connection per node and keeps them up.
//
// A note on log fields: the FreeSWITCH address is logged as `fs_node`, never
// `node`. The shared logger already attaches `node` for the APP instance, and
// a second key of the same name produces a record with duplicate JSON keys —
// which parsers resolve inconsistently, and which makes "which host was this?"
// ambiguous in exactly the query someone runs during an incident.
type Pool struct {
	cfg Config
	log *slog.Logger

	mu    sync.RWMutex
	conns map[string]*esl.Conn

	wg sync.WaitGroup
}

// New builds a Pool. Nothing connects until Run.
func New(cfg Config, log *slog.Logger) *Pool {
	return &Pool{
		cfg:   cfg,
		log:   log,
		conns: make(map[string]*esl.Conn),
	}
}

// Run maintains a connection to every node until the context ends.
//
// It returns nil rather than an error when nodes are unreachable. A switch
// being down is a condition to report through the health endpoint and keep
// retrying, not a reason to take this service down — the calls it is already
// carrying arrive on the OTHER ESL direction and do not depend on this one.
func (p *Pool) Run(ctx context.Context) error {
	for _, node := range p.cfg.Nodes {
		p.wg.Add(1)
		go func(node string) {
			defer p.wg.Done()
			p.maintain(ctx, node)
		}(node)
	}

	<-ctx.Done()
	p.closeAll()
	p.wg.Wait()

	p.log.Info("inbound esl pool stopped")
	return nil
}

// maintain keeps one node connected, reconnecting with backoff.
func (p *Pool) maintain(ctx context.Context, node string) {
	addr := fmt.Sprintf("%s:%d", node, p.cfg.Port)
	backoff := minBackoff

	for ctx.Err() == nil {
		conn, err := esl.Dial(ctx, addr, p.cfg.Password, p.cfg.Keepalive)
		if err != nil {
			if ctx.Err() != nil {
				return
			}

			p.log.Warn("inbound esl connect failed; retrying",
				"fs_node", addr, "retry_in", backoff.String(), "error", err)

			if !sleep(ctx, jitter(backoff)) {
				return
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		p.log.Info("inbound esl connected", "fs_node", addr)
		p.set(node, conn)

		// Reset only after a connection succeeds. Resetting on each attempt
		// would turn the backoff into a fixed one-second retry against a
		// switch that is down.
		backoff = minBackoff

		// Serve blocks until the connection drops. Events are ignored: nothing
		// subscribes on this direction yet, and an unsubscribed connection
		// receives none.
		err = conn.Serve(nil)

		p.clear(node)

		if ctx.Err() != nil {
			return
		}

		p.log.Warn("inbound esl disconnected; reconnecting", "fs_node", addr, "error", err)

		if !sleep(ctx, jitter(minBackoff)) {
			return
		}
	}
}

// jitter spreads reconnects so every node does not retry in lockstep after a
// switch restart — which would arrive as a burst of simultaneous auth attempts.
func jitter(d time.Duration) time.Duration {
	return d + time.Duration(rand.Int64N(int64(d/2)+1))
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (p *Pool) set(node string, conn *esl.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conns[node] = conn
}

func (p *Pool) clear(node string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.conns, node)
}

func (p *Pool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for node, conn := range p.conns {
		_ = conn.Close()
		delete(p.conns, node)
	}
}

// Connected reports which nodes are currently usable.
func (p *Pool) Connected() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make([]string, 0, len(p.conns))
	for node := range p.conns {
		out = append(out, node)
	}
	return out
}

// HealthCheck reports whether the switch is reachable at all.
//
// One node is enough. With a farm of several, some being down is degraded
// rather than broken — and reporting this service unhealthy because one of
// three switches is restarting would take it out of service for a problem it
// is not having.
func (p *Pool) HealthCheck(context.Context) error {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.conns) == 0 {
		return fmt.Errorf("no FreeSWITCH node connected (of %d configured)",
			len(p.cfg.Nodes))
	}
	return nil
}

// ChannelExists reports whether FreeSWITCH still holds a channel.
//
// Used to tell a TRANSFERRED call from one that ended: both stop bridging, and
// only the switch knows which happened. Asked over this connection rather than
// the call's own socket, because that socket may be exactly what the transfer
// took away.
//
// `uuid_exists` answers "true" or "false". Anything else is treated as an
// error rather than as "false" — a malformed reply read as "the call ended"
// would close the record on a call still in progress.
func (p *Pool) ChannelExists(_ context.Context, uuid string) (bool, error) {
	out, err := p.API("uuid_exists " + uuid)
	if err != nil {
		return false, err
	}

	switch strings.TrimSpace(strings.TrimPrefix(out, "+OK ")) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("eslclient: unexpected uuid_exists reply %q", out)
	}
}

// API runs a synchronous API command on any connected node.
func (p *Pool) API(cmd string) (string, error) {
	conn, err := p.any()
	if err != nil {
		return "", err
	}
	return conn.API(cmd, commandTimeout)
}

// BgAPI runs a command in the background and returns its Job-UUID.
//
// This is what originate uses. A synchronous originate would hold the
// connection for the whole ring — seconds to tens of seconds — and block every
// other command queued behind it.
func (p *Pool) BgAPI(cmd string) (string, error) {
	conn, err := p.any()
	if err != nil {
		return "", err
	}
	return conn.BgAPI(cmd, commandTimeout)
}

// any returns a connected node.
//
// Arbitrary choice, and that is correct for now: every node in the farm can
// originate, and with one node there is nothing to choose between. When a
// command has to reach the node holding a specific channel — uuid_transfer,
// say — it needs the node from that call's record, not this.
func (p *Pool) any() (*esl.Conn, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for _, conn := range p.conns {
		return conn, nil
	}
	return nil, ErrNoNode
}
