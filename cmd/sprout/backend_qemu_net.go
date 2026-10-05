package main

import (
	"context"
	"fmt"
	"os"

	"github.com/containers/gvisor-tap-vsock/pkg/transport"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
)

// QEMU's `-netdev stream` dials this socket as a client and frames each
// Ethernet frame with a 4-byte length.
type qemuStream struct{}

func (qemuStream) protocol() types.Protocol { return types.QemuProtocol }

func (qemuStream) serve(ctx context.Context, vn *virtualnetwork.VirtualNetwork, netSock string) error {
	_ = os.Remove(netSock)
	ln, err := transport.Listen("unix://" + netSock)
	if err != nil {
		return fmt.Errorf("qemu network socket listen: %w", err)
	}
	keepSocketOnClose(ln)
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	go serveNICSessions(ctx, "qemu", ln.Accept, vn.AcceptQemu)
	return nil
}
