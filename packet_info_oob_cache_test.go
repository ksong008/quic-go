//go:build darwin || linux || freebsd

package quic

import (
	"net"
	"net/netip"
	"testing"

	"github.com/daeuniverse/quic-go/internal/protocol"

	"github.com/stretchr/testify/require"
)

func TestQueuedReceivedPacketCachesPacketInfoOOB(t *testing.T) {
	info := packetInfo{addr: netip.AddrFrom4([4]byte{127, 0, 0, 1})}
	p := receivedPacket{
		remoteAddr: &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1234},
		info:       info,
	}

	qp := newQueuedReceivedPacket(p)
	require.Equal(t, p, qp.receivedPacket)
	require.Equal(t, info.OOB(), qp.packetInfoOOB)
}

func TestClosePacketCachesPacketInfoOOB(t *testing.T) {
	var closePackets []closePacket
	m := newPacketHandlerMapForTest(t, func(p closePacket) {
		closePackets = append(closePackets, p)
	})
	handler := &mockPacketHandler{}
	connID := protocol.ParseConnectionID([]byte{4, 3, 2, 1})
	require.True(t, m.Add(connID, handler))
	m.ReplaceWithClosed([]protocol.ConnectionID{connID}, []byte("foobar"))

	info := packetInfo{addr: netip.AddrFrom4([4]byte{127, 0, 0, 1})}
	h, ok := m.Get(connID)
	require.True(t, ok)
	h.handlePacket(receivedPacket{
		remoteAddr: &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1234},
		info:       info,
	})

	require.Len(t, closePackets, 1)
	require.Equal(t, info.OOB(), closePackets[0].packetInfoOOB)
}

func TestClosePacketCachesNilPacketInfoOOB(t *testing.T) {
	var closePackets []closePacket
	m := newPacketHandlerMapForTest(t, func(p closePacket) {
		closePackets = append(closePackets, p)
	})
	handler := &mockPacketHandler{}
	connID := protocol.ParseConnectionID([]byte{1, 2, 3, 4})
	require.True(t, m.Add(connID, handler))
	m.ReplaceWithClosed([]protocol.ConnectionID{connID}, []byte("foobar"))

	h, ok := m.Get(connID)
	require.True(t, ok)
	h.handlePacket(receivedPacket{remoteAddr: &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 1234}})

	require.Len(t, closePackets, 1)
	require.Nil(t, closePackets[0].packetInfoOOB)
}
