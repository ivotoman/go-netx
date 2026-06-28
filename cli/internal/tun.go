package internal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	netx "github.com/pedramktb/go-netx"
	"github.com/spf13/cobra"
)

const tunExample = `	netx tun \
		--from "tcp+tls{cert=$(cat server.crt | xxd -p),key=$(cat server.key | xxd -p)}://:9000" \
 		--to "udp+aesgcm{key=00112233445566778899aabbccddeeff}://127.0.0.1:5555"
`

func tun(cancel context.CancelFunc) *cobra.Command {
	var from string
	var to string
	var eager bool

	if cancel == nil {
		cancel = func() {}
	}

	cmd := &cobra.Command{
		Use:           "tun",
		Short:         "Relay between two endpoints with chainable transforms.",
		Long:          "tun relays between two endpoints with chainable transforms, this can be used for obfuscation tunnels, proxies, reverse proxies, etc.",
		Example:       tunExample,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			err := runTun(ctx, cancel, from, to, eager)
			if err != nil {
				return errors.Join(err, cmd.Help())
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&from, "from", "", "<uri>")
	cmd.Flags().StringVar(&to, "to", "", "<uri>")
	cmd.Flags().BoolVar(&eager, "eager", false,
		"pre-dial the --to upstream at start (warming the obfuscation handshake) instead of lazily on the first inbound packet")

	_ = cmd.MarkFlagRequired("from")
	_ = cmd.MarkFlagRequired("to")

	return cmd
}

// handshaker is any dialed conn whose security handshake is DEFERRED to the first
// I/O (pion *dtls.Conn, stdlib *tls.Conn). Driving it eagerly under a bounded,
// cancellable ctx (see dialPeer) turns a silent first-handshake stall — which
// pion's 5-tuple conn keying would otherwise make permanent until a full client
// reconnect ("first connect dark, 2nd attempt works") — into a fast, retryable
// dial error. AES-GCM/frame/plain conns don't implement this and have no blocking
// handshake, so the assertion no-ops for them.
type handshaker interface {
	HandshakeContext(ctx context.Context) error
}

const (
	// 1 initial + 2 retries. Per-attempt 4s comfortably covers a DTLS Certificate
	// flight on a lossy/censored path; worst case (4+0.25+4+0.25+4 ≈ 12.5s) stays
	// under the Dart-side netx/wg start timeout so a genuinely dead path returns a
	// clean transient error to the establishment-retry layer, not a hang.
	dialHandshakeAttempts = 3
	dialHandshakeTimeout  = 4 * time.Second
	dialHandshakeBackoff  = 250 * time.Millisecond
)

func runTun(ctx context.Context, cancel context.CancelFunc, from, to string, eager bool) error {
	var fromURI netx.ListenerURI
	var toURI netx.DialerURI
	if err := fromURI.UnmarshalText([]byte(from)); err != nil {
		return fmt.Errorf("parse --from: %w", err)
	}
	if err := toURI.UnmarshalText([]byte(to)); err != nil {
		return fmt.Errorf("parse --to: %w", err)
	}

	ln, err := fromURI.Listen(ctx)
	if err != nil {
		return err
	}
	defer ln.Close()

	tm := netx.TunMaster[struct{}]{}

	// dialControl is a no-op except on iOS, where it binds netx's outbound socket
	// to the primary physical interface (IP_BOUND_IF) so the obfuscation dial does
	// not loop back into the NEPacketTunnelProvider's tunnel (rx=0).
	//
	// dialPeer also DRIVES the deferred security handshake (DTLS/TLS) eagerly under
	// a bounded, cancellable context instead of letting pion run it later under an
	// uncancellable context.Background() at first I/O. pion keys conns by 5-tuple,
	// so a first handshake that stalls is never re-entered and the relay stays
	// silently dark until a full reconnect. Driving it here turns a stall into a
	// fast (retryable) dial error. AES-GCM/frame/plain conns aren't handshakers and
	// no-op. Both callers — the lazy route handler and the eager pre-warm
	// (deferredConn) — go through dialPeer, so both get a handshake-proven upstream.
	dialPeer := func(c context.Context) (net.Conn, error) {
		conn, err := toURI.Dial(c, netx.WithDialConfig(net.Dialer{Control: dialControl}))
		if err != nil {
			return nil, err
		}
		if hs, ok := conn.(handshaker); ok {
			hctx, cancelHS := context.WithTimeout(c, dialHandshakeTimeout)
			err = hs.HandshakeContext(hctx)
			cancelHS()
			if err != nil {
				_ = conn.Close()
				return nil, err
			}
		}
		return conn, nil
	}

	// In eager mode, pre-dial the upstream the moment the listener is bound so
	// the obfuscation handshake to the server is warm (or already complete)
	// before the first inbound packet — the win that makes a speculative
	// pre-connect near-instant. The warm conn is consumed by the first inbound
	// connection; any later connection falls back to a lazy per-conn dial.
	var warmPeer atomic.Pointer[deferredConn]
	if eager {
		warmPeer.Store(newDeferredConn(ctx, dialPeer,
			stubAddr{network: toURI.Transport.String(), addr: toURI.Addr}))
	}

	tm.SetRoute(struct{}{}, func(ctx context.Context, conn net.Conn) (bool, context.Context, netx.Tun) {
		var peer net.Conn
		if w := warmPeer.Swap(nil); w != nil {
			peer = w
		} else {
			// Bounded-retry the dial+handshake (dialPeer drives the deferred
			// DTLS/TLS handshake). A first handshake that stalls becomes a fast,
			// retryable error rather than a permanently dark relay. ctx cancel
			// (NetxInterrupt) aborts between attempts and during the backoff.
			var pconn net.Conn
			var lastErr error
			for attempt := 1; attempt <= dialHandshakeAttempts; attempt++ {
				if err := ctx.Err(); err != nil {
					lastErr = err
					break
				}
				c, err := dialPeer(ctx)
				if err != nil {
					lastErr = err
					slog.Warn("dial tun", "attempt", attempt, "err", err)
				} else {
					pconn = c
					break
				}
				if attempt < dialHandshakeAttempts {
					select {
					case <-ctx.Done():
					case <-time.After(dialHandshakeBackoff):
					}
				}
			}
			if pconn == nil {
				slog.Error("dial tun", "err", lastErr)
				_ = conn.Close()
				return false, ctx, netx.Tun{}
			}
			peer = pconn
			// Once-per-connection (the route handler runs per inbound conn, not per
			// datagram), and only on the lazy path — the eager path logs its own
			// pre-warm. For handshaker schemes (DTLS/TLS) dialPeer has now PROVEN
			// the upstream handshake; AES-GCM/plain are dialed-only (no blocking
			// handshake). Pinpoints a session whose upstream never dialed/handshook.
			slog.Info("netx tun upstream dialed", "to", toURI.Redacted())
		}

		return true, ctx, netx.Tun{Conn: conn, Peer: peer}
	})

	go func() {
		if err := tm.Serve(ctx, ln); err != nil && !errors.Is(err, netx.ErrServerClosed) {
			slog.Error("serve error", "err", err)
			cancel()
		}
	}()

	// Embedders (the c-shared library forwards info-level slog output to a C
	// callback) latch on this line to know the relay is bound and serving.
	// Redacted() masks secret param values with driver-supplied protocol-standard
	// fingerprints, so the protocol chain stays visible without leaking key
	// material.
	slog.Info("netx tun started", "listen", ln.Addr().String(), "from", fromURI.Redacted(), "to", toURI.Redacted())

	<-ctx.Done()
	// Close a pre-warmed upstream that was never consumed (e.g. an abandoned
	// speculative pre-connect) so its dialed conn doesn't outlive the relay.
	if w := warmPeer.Swap(nil); w != nil {
		_ = w.Close()
	}
	shutdownCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	_ = tm.Shutdown(shutdownCtx)

	return nil
}
