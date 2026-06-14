package internal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	netx "github.com/pedramktb/go-netx"
	"github.com/spf13/cobra"
)

// peerReadTimeout bounds how long the self-healing peer connection waits for
// inbound bytes before it re-dials (re-running dialControl → a fresh bind to the
// current physical interface; see netx.WithMuxClientSelfHeal). It is a
// LAST-RESORT backstop — the host app's own network-change detection (native path
// monitors + a through-tunnel health probe that triggers a full reconnect)
// recovers far faster — so the only thing this value must NOT do is fire on a
// healthy-but-idle tunnel. The VPN server peer sets no PersistentKeepalive, so on
// an idle link the client receives nothing EXCEPT WireGuard's periodic rekey
// handshake response, which lands about every 120 s (REKEY_AFTER_TIME). The
// timeout must sit comfortably above that: 240 s mirrors the client's 180 s
// handshake-freshness window plus the same ~60 s slack. A genuinely dead inbound
// path (no rx at all for 240 s, i.e. even rekey failing) is then re-dialled; a
// quiet healthy one is left alone.
const peerReadTimeout = 240 * time.Second

const tunExample = `	netx tun \
		--from "tcp+tls{cert=$(cat server.crt | xxd -p),key=$(cat server.key | xxd -p)}://:9000" \
 		--to "udp+aesgcm{key=00112233445566778899aabbccddeeff}://127.0.0.1:5555"
`

func tun(cancel context.CancelFunc) *cobra.Command {
	var from string
	var to string

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
			err := runTun(ctx, cancel, from, to)
			if err != nil {
				return errors.Join(err, cmd.Help())
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&from, "from", "", "<uri>")
	cmd.Flags().StringVar(&to, "to", "", "<uri>")

	_ = cmd.MarkFlagRequired("from")
	_ = cmd.MarkFlagRequired("to")

	return cmd
}

func runTun(ctx context.Context, cancel context.CancelFunc, from, to string) error {
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

	tm.SetRoute(struct{}{}, func(ctx context.Context, conn net.Conn) (bool, context.Context, netx.Tun) {
		// dialControl is a no-op except on darwin, where it binds netx's outbound
		// socket to the primary physical interface (IP_BOUND_IF) so the obfuscation
		// dial does not loop back into the NEPacketTunnelProvider's tunnel (rx=0).
		dialPeer := func() (net.Conn, error) {
			return toURI.Dial(ctx, netx.WithDialConfig(net.Dialer{Control: dialControl}))
		}
		// Eager first dial: validates the upstream and fast-fails the route if it
		// is unreachable (unchanged behaviour). The live connection is then handed
		// to a SELF-HEALING MuxClient. On darwin the dialed socket is pinned to a
		// physical interface (IP_BOUND_IF) and goes half-open when that interface
		// changes on a network switch — writes buffer, reads block forever, no
		// error fires — so the tunnel sits "connected" with dead rx. The MuxClient
		// re-dials (re-running dialControl → a fresh interface bind + fresh
		// handshake) when a read times out or errors, restoring rx with no external
		// stop→start. Seeding it with this conn avoids a second handshake on connect.
		pconn, err := dialPeer()
		if err != nil {
			slog.Error("dial tun", "err", err)
			_ = conn.Close()
			return false, ctx, netx.Tun{}
		}
		peer := netx.NewMuxClient(
			dialPeer,
			netx.WithMuxClientConn(pconn),
			netx.WithMuxClientSelfHeal(peerReadTimeout),
		)

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
	shutdownCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	_ = tm.Shutdown(shutdownCtx)

	return nil
}
