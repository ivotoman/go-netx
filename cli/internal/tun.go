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

	// dialControl is a no-op except on darwin, where it binds netx's outbound
	// socket to the primary physical interface (IP_BOUND_IF) so the obfuscation
	// dial does not loop back into the NEPacketTunnelProvider's tunnel (rx=0).
	dialPeer := func(c context.Context) (net.Conn, error) {
		return toURI.Dial(c, netx.WithDialConfig(net.Dialer{Control: dialControl}))
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
			pconn, err := dialPeer(ctx)
			if err != nil {
				slog.Error("dial tun", "err", err)
				_ = conn.Close()
				return false, ctx, netx.Tun{}
			}
			peer = pconn
			// Once-per-connection (the route handler runs per inbound conn, not
			// per datagram), and only on the lazy path — the eager path logs its
			// own pre-warm. Gives the lifecycle signal that was missing between
			// "netx tun started" (listener bound) and "tunnel closed": the
			// upstream for this session is dialed. For obfs schemes the handshake
			// is lazy (completes on first I/O), so this is "dialed", not
			// "handshake proven" — but it pinpoints a session whose upstream never
			// even dialed vs one stuck waiting on the far side.
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
