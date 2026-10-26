package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/portforwardws"
)

func (a *app) portForwardCmd() *cobra.Command {
	var stdio bool
	var address string
	cmd := &cobra.Command{
		Use:   "port-forward KIND NAME [LOCAL:]REMOTE",
		Short: "Forward a local TCP port to a port of a container or virtual machine",
		Long: "Listens on LOCAL (REMOTE when it is not given) and relays each connection\n" +
			"to port REMOTE of the machine's first interface, through the API, whatever\n" +
			"its security groups let in, until interrupted. With --stdio, relays stdin\n" +
			"and stdout as one connection instead, as an OpenSSH ProxyCommand would.\n" +
			"It needs compute.machines.portforward.",
		Example: "  hangar port-forward container web 8080\n" +
			"  hangar port-forward vm db 15432:5432\n" +
			"  hangar port-forward container web 3000 --stdio",
		Args: args(3, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, err := forwardKind(args[0])
			if err != nil {
				return err
			}
			local, remote, err := forwardPorts(args[2])
			if err != nil {
				return err
			}
			sc, err := a.capabilityContext(kind)
			if err != nil {
				return err
			}
			if stdio && strings.Contains(args[2], ":") {
				return usageErr("--stdio listens on no local port: give REMOTE alone")
			}
			path := portforwardws.Path(sc.project, kind.Plural, args[1], remote)
			if stdio {
				return a.tunnel(cmd.Context(), sc, path, portforwardws.Subprotocol, portforwardws.MaxMessage, a.io.In, a.io.Out)
			}
			return a.listenForward(cmd.Context(), sc, path, net.JoinHostPort(address, strconv.Itoa(int(local))))
		},
	}
	cmd.Flags().BoolVar(&stdio, "stdio", false, "relay stdin and stdout as one connection instead of listening")
	cmd.Flags().StringVar(&address, "address", "127.0.0.1", "the local address to listen on")
	return cmd
}

// forwardKind is the kind KIND names, which must run as a machine.
func forwardKind(kind string) (registry.Resource, error) {
	r, err := findKind(kind)
	if err != nil {
		return registry.Resource{}, err
	}
	if r.Kind != registry.Container.Kind && r.Kind != registry.VirtualMachine.Kind {
		return registry.Resource{}, usageErr("only containers and virtual machines take a port forward, not %s", r.Plural)
	}
	return r, nil
}

// forwardPorts reads [LOCAL:]REMOTE; LOCAL is REMOTE when not given.
func forwardPorts(s string) (local, remote uint16, err error) {
	l, r, ok := strings.Cut(s, ":")
	if !ok {
		r = l
	}
	remote, err = portOf(r)
	if err != nil {
		return 0, 0, err
	}
	if !ok {
		return remote, remote, nil
	}
	local, err = portOf(l)
	return local, remote, err
}

func portOf(s string) (uint16, error) {
	p, err := strconv.ParseUint(s, 10, 16)
	if err != nil || p == 0 {
		return 0, usageErr("port %q is not 1 to 65535", s)
	}
	return uint16(p), nil
}

// listenForward listens on addr and relays each connection over its own
// port forward until ctx ends.
func (a *app) listenForward(ctx context.Context, sc *sshContext, path, addr string) error {
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = l.Close() })
	defer stop()
	_, _ = fmt.Fprintf(a.io.Err, "Forwarding from %s\n", l.Addr())
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() {
			defer func() { _ = conn.Close() }()
			err := a.tunnel(ctx, sc, path, portforwardws.Subprotocol, portforwardws.MaxMessage, conn, conn)
			if err != nil && !errors.Is(err, context.Canceled) {
				_, _ = fmt.Fprintf(a.io.Err, "connection from %s: %v\n", conn.RemoteAddr(), err)
			}
		}()
	}
}
