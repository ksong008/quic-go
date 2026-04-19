package quic

import (
	"net"
	"sync/atomic"

	"github.com/daeuniverse/quic-go/internal/protocol"
	"github.com/daeuniverse/quic-go/internal/utils"
)

const (
	sendConnOOBCacheSize     = int(protocol.ECNCE) + 1
	sendConnOOBBufferSize    = 128
	sendConnOOBExtraCapacity = 64
)

// A sendConn allows sending using a simple Write() on a non-connected packet conn.
type sendConn interface {
	Write(b []byte, gsoSize uint16, ecn protocol.ECN) error
	Close() error
	LocalAddr() net.Addr
	RemoteAddr() net.Addr
	SetRemoteAddr(net.Addr)

	capabilities() connCapabilities
}

type sconn struct {
	rawConn

	localAddr  net.Addr
	remoteAddr atomic.Value

	logger utils.Logger

	packetInfoOOB []byte
	// Immutable cached OOB payloads for the common no-GSO ECN send path.
	packetInfoOOBIPv4ECN [sendConnOOBCacheSize][]byte
	packetInfoOOBIPv6ECN [sendConnOOBCacheSize][]byte
	// If GSO enabled, and we receive a GSO error for this remote address, GSO is disabled.
	gotGSOError bool
	// Used to catch the error sometimes returned by the first sendmsg call on Linux,
	// see https://github.com/golang/go/issues/63322.
	wroteFirstPacket bool
}

var _ sendConn = &sconn{}

func newSendConn(c rawConn, remote net.Addr, info packetInfo, logger utils.Logger) *sconn {
	localAddr := c.LocalAddr()
	if info.addr.IsValid() {
		if udpAddr, ok := localAddr.(*net.UDPAddr); ok {
			addrCopy := *udpAddr
			addrCopy.IP = info.addr.AsSlice()
			localAddr = &addrCopy
		}
	}

	oob := info.OOB()
	// increase oob slice capacity, so we can add the UDP_SEGMENT and ECN control messages without allocating
	l := len(oob)
	oob = append(oob, make([]byte, sendConnOOBExtraCapacity)...)[:l]
	sc := &sconn{
		rawConn:       c,
		localAddr:     localAddr,
		remoteAddr:    atomic.Value{},
		packetInfoOOB: oob,
		logger:        logger,
	}
	sc.initOOBCache()
	sc.SetRemoteAddr(remote)
	return sc
}

func (c *sconn) Write(p []byte, gsoSize uint16, ecn protocol.ECN) error {
	remoteAddr := c.remoteAddr.Load().(net.Addr)
	err := c.writePacket(p, remoteAddr, gsoSize, ecn)
	if err != nil && gsoSize > 0 && isGSOError(err) {
		// disable GSO for future calls
		c.gotGSOError = true
		if c.logger.Debug() {
			c.logger.Debugf("GSO failed when sending to %s", remoteAddr)
		}
		// send out the packets one by one
		for len(p) > 0 {
			l := len(p)
			if l > int(gsoSize) {
				l = int(gsoSize)
			}
			if err := c.writePacket(p[:l], remoteAddr, 0, ecn); err != nil {
				return err
			}
			p = p[l:]
		}
		return nil
	}
	return err
}

func (c *sconn) writePacket(p []byte, addr net.Addr, gsoSize uint16, ecn protocol.ECN) error {
	var oobBuffer [sendConnOOBBufferSize]byte
	oob, preparedGSOSize, preparedECN := c.prepareOOB(addr, gsoSize, ecn, oobBuffer[:0])
	_, err := c.WritePacket(p, addr, oob, preparedGSOSize, preparedECN)
	if err != nil && !c.wroteFirstPacket && isPermissionError(err) {
		oob, preparedGSOSize, preparedECN = c.prepareOOB(addr, gsoSize, ecn, oobBuffer[:0])
		_, err = c.WritePacket(p, addr, oob, preparedGSOSize, preparedECN)
	}
	c.wroteFirstPacket = true
	return err
}

func (c *sconn) capabilities() connCapabilities {
	capabilities := c.rawConn.capabilities()
	if capabilities.GSO {
		capabilities.GSO = !c.gotGSOError
	}
	return capabilities
}

func (c *sconn) RemoteAddr() net.Addr { return c.remoteAddr.Load().(net.Addr) }
func (c *sconn) LocalAddr() net.Addr  { return c.localAddr }

func (c *sconn) SetRemoteAddr(addr net.Addr) {
	if addr == nil {
		return
	}
	c.remoteAddr.Store(addr)
}
