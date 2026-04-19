//go:build !darwin && !linux && !freebsd

package quic

import (
	"net"

	"github.com/daeuniverse/quic-go/internal/protocol"
)

func (c *sconn) initOOBCache() {}

func (c *sconn) prepareOOB(_ net.Addr, gsoSize uint16, ecn protocol.ECN, _ []byte) ([]byte, uint16, protocol.ECN) {
	return c.packetInfoOOB, gsoSize, ecn
}
