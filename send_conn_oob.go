//go:build darwin || linux || freebsd

package quic

import (
	"net"

	"github.com/daeuniverse/quic-go/internal/protocol"
)

func (c *sconn) initOOBCache() {
	c.packetInfoOOBIPv4ECN = buildECNOOBCache(c.packetInfoOOB, appendIPv4ECNMsg)
	c.packetInfoOOBIPv6ECN = buildECNOOBCache(c.packetInfoOOB, appendIPv6ECNMsg)
}

func buildECNOOBCache(base []byte, appendECN func([]byte, protocol.ECN) []byte) [sendConnOOBCacheSize][]byte {
	var cache [sendConnOOBCacheSize][]byte
	for _, ecn := range [...]protocol.ECN{protocol.ECNNon, protocol.ECT1, protocol.ECT0, protocol.ECNCE} {
		b := append([]byte{}, base...)
		cache[ecn] = appendECN(b, ecn)
	}
	return cache
}

func (c *sconn) cachedECNOOB(addr net.Addr, ecn protocol.ECN) []byte {
	if ecn == protocol.ECNUnsupported {
		return c.packetInfoOOB
	}
	udpAddr, ok := addr.(*net.UDPAddr)
	if !ok {
		return nil
	}
	if udpAddr.IP.To4() != nil {
		return c.packetInfoOOBIPv4ECN[ecn]
	}
	return c.packetInfoOOBIPv6ECN[ecn]
}

func (c *sconn) prepareOOB(addr net.Addr, gsoSize uint16, ecn protocol.ECN, oobBuffer []byte) ([]byte, uint16, protocol.ECN) {
	if gsoSize == 0 {
		if cached := c.cachedECNOOB(addr, ecn); cached != nil {
			return cached, 0, protocol.ECNUnsupported
		}
	}

	oob := oobBuffer[:0]
	if len(c.packetInfoOOB)+sendConnOOBExtraCapacity > cap(oob) {
		oob = make([]byte, 0, len(c.packetInfoOOB)+sendConnOOBExtraCapacity)
	}
	oob = append(oob, c.packetInfoOOB...)

	if gsoSize > 0 {
		oob = appendUDPSegmentSizeMsg(oob, gsoSize)
	}
	if ecn != protocol.ECNUnsupported {
		if udpAddr, ok := addr.(*net.UDPAddr); ok {
			if udpAddr.IP.To4() != nil {
				oob = appendIPv4ECNMsg(oob, ecn)
			} else {
				oob = appendIPv6ECNMsg(oob, ecn)
			}
		}
	}
	return oob, 0, protocol.ECNUnsupported
}
