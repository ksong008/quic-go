package quic

import (
	"io"
	"net"
	"sync"
	"time"

	"github.com/daeuniverse/quic-go/internal/protocol"
	"github.com/daeuniverse/quic-go/internal/utils"
)

type connCapabilities struct {
	// This connection has the Don't Fragment (DF) bit set.
	// This means it makes to run DPLPMTUD.
	DF bool
	// GSO (Generic Segmentation Offload) supported
	GSO bool
	// ECN (Explicit Congestion Notifications) supported
	ECN bool
}

// rawConn is a connection that allow reading of a receivedPackeh.
type rawConn interface {
	ReadPacket() (receivedPacket, error)
	// WritePacket writes a packet on the wire.
	// gsoSize is the size of a single packet, or 0 to disable GSO.
	// It is invalid to set gsoSize if capabilities.GSO is not set.
	WritePacket(b []byte, addr net.Addr, packetInfoOOB []byte, gsoSize uint16, ecn protocol.ECN) (int, error)
	LocalAddr() net.Addr
	SetReadDeadline(time.Time) error
	io.Closer

	capabilities() connCapabilities
}

type closePacket struct {
	payload []byte
	addr    net.Addr
	info    packetInfo

	packetInfoOOB []byte
}

type packetHandlerMapCleanupKind uint8

const (
	packetHandlerMapCleanupRetired packetHandlerMapCleanupKind = iota
	packetHandlerMapCleanupClosed
)

type packetHandlerMapCleanupEntry struct {
	deadline time.Time
	kind     packetHandlerMapCleanupKind
	id       protocol.ConnectionID
	ids      []protocol.ConnectionID
}

type packetHandlerMap struct {
	mutex       sync.Mutex
	handlers    map[protocol.ConnectionID]packetHandler
	resetTokens map[protocol.StatelessResetToken] /* stateless reset token */ packetHandler

	closed           bool
	closeChan        chan struct{}
	cleanupScheduled chan struct{}
	pendingCleanups  []packetHandlerMapCleanupEntry

	enqueueClosePacket func(closePacket)

	deleteRetiredConnsAfter time.Duration

	logger utils.Logger
}

var _ packetHandlerManager = &packetHandlerMap{}

func newPacketHandlerMap(enqueueClosePacket func(closePacket), logger utils.Logger) *packetHandlerMap {
	h := &packetHandlerMap{
		closeChan:               make(chan struct{}),
		cleanupScheduled:        make(chan struct{}, 1),
		handlers:                make(map[protocol.ConnectionID]packetHandler),
		resetTokens:             make(map[protocol.StatelessResetToken]packetHandler),
		deleteRetiredConnsAfter: protocol.RetiredConnectionIDDeleteTimeout,
		enqueueClosePacket:      enqueueClosePacket,
		logger:                  logger,
	}
	go h.runCleanupQueue()
	if h.logger.Debug() {
		go h.logUsage()
	}
	return h
}

func (h *packetHandlerMap) logUsage() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var printedZero bool
	for {
		select {
		case <-h.closeChan:
			return
		case <-ticker.C:
		}

		h.mutex.Lock()
		numHandlers := len(h.handlers)
		numTokens := len(h.resetTokens)
		h.mutex.Unlock()
		// If the number tracked handlers and tokens is zero, only print it a single time.
		hasZero := numHandlers == 0 && numTokens == 0
		if !hasZero || (hasZero && !printedZero) {
			h.logger.Debugf("Tracking %d connection IDs and %d reset tokens.\n", numHandlers, numTokens)
			printedZero = false
			if hasZero {
				printedZero = true
			}
		}
	}
}

func (h *packetHandlerMap) Get(id protocol.ConnectionID) (packetHandler, bool) {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	handler, ok := h.handlers[id]
	return handler, ok
}

func (h *packetHandlerMap) Add(id protocol.ConnectionID, handler packetHandler) bool /* was added */ {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	if _, ok := h.handlers[id]; ok {
		h.logger.Debugf("Not adding connection ID %s, as it already exists.", id)
		return false
	}
	h.handlers[id] = handler
	h.logger.Debugf("Adding connection ID %s.", id)
	return true
}

func (h *packetHandlerMap) AddWithConnID(clientDestConnID, newConnID protocol.ConnectionID, handler packetHandler) bool {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	if _, ok := h.handlers[clientDestConnID]; ok {
		h.logger.Debugf("Not adding connection ID %s for a new connection, as it already exists.", clientDestConnID)
		return false
	}
	h.handlers[clientDestConnID] = handler
	h.handlers[newConnID] = handler
	h.logger.Debugf("Adding connection IDs %s and %s for a new connection.", clientDestConnID, newConnID)
	return true
}

func (h *packetHandlerMap) Remove(id protocol.ConnectionID) {
	h.mutex.Lock()
	delete(h.handlers, id)
	h.mutex.Unlock()
	h.logger.Debugf("Removing connection ID %s.", id)
}

func (h *packetHandlerMap) Retire(id protocol.ConnectionID) {
	h.logger.Debugf("Retiring connection ID %s in %s.", id, h.deleteRetiredConnsAfter)
	h.scheduleCleanup(packetHandlerMapCleanupEntry{
		deadline: time.Now().Add(h.deleteRetiredConnsAfter),
		kind:     packetHandlerMapCleanupRetired,
		id:       id,
	})
}

// ReplaceWithClosed is called when a connection is closed.
// Depending on which side closed the connection, we need to:
// * remote close: absorb delayed packets
// * local close: retransmit the CONNECTION_CLOSE packet, in case it was lost
func (h *packetHandlerMap) ReplaceWithClosed(ids []protocol.ConnectionID, connClosePacket []byte) {
	var handler packetHandler
	if connClosePacket != nil {
		handler = newClosedLocalConn(
			func(addr net.Addr, info packetInfo) {
				h.enqueueClosePacket(closePacket{
					payload:       connClosePacket,
					addr:          addr,
					info:          info,
					packetInfoOOB: info.OOB(),
				})
			},
			h.logger,
		)
	} else {
		handler = newClosedRemoteConn()
	}

	h.mutex.Lock()
	for _, id := range ids {
		h.handlers[id] = handler
	}
	h.mutex.Unlock()
	h.logger.Debugf("Replacing connection for connection IDs %s with a closed connection.", ids)

	h.scheduleCleanup(packetHandlerMapCleanupEntry{
		deadline: time.Now().Add(h.deleteRetiredConnsAfter),
		kind:     packetHandlerMapCleanupClosed,
		ids:      append([]protocol.ConnectionID(nil), ids...),
	})
}

func (h *packetHandlerMap) AddResetToken(token protocol.StatelessResetToken, handler packetHandler) {
	h.mutex.Lock()
	h.resetTokens[token] = handler
	h.mutex.Unlock()
}

func (h *packetHandlerMap) RemoveResetToken(token protocol.StatelessResetToken) {
	h.mutex.Lock()
	delete(h.resetTokens, token)
	h.mutex.Unlock()
}

func (h *packetHandlerMap) GetByResetToken(token protocol.StatelessResetToken) (packetHandler, bool) {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	handler, ok := h.resetTokens[token]
	return handler, ok
}

func (h *packetHandlerMap) Close(e error) {
	h.mutex.Lock()

	if h.closed {
		h.mutex.Unlock()
		return
	}

	h.closed = true
	close(h.closeChan)

	var wg sync.WaitGroup
	for _, handler := range h.handlers {
		wg.Add(1)
		go func(handler packetHandler) {
			handler.destroy(e)
			wg.Done()
		}(handler)
	}
	h.mutex.Unlock()
	wg.Wait()
}

func (h *packetHandlerMap) scheduleCleanup(entry packetHandlerMapCleanupEntry) {
	h.mutex.Lock()
	if h.closed {
		h.mutex.Unlock()
		return
	}
	h.pendingCleanups = append(h.pendingCleanups, entry)
	h.mutex.Unlock()

	select {
	case h.cleanupScheduled <- struct{}{}:
	default:
	}
}

func (h *packetHandlerMap) runCleanupQueue() {
	timer := utils.NewTimer()
	defer timer.Stop()

	for {
		timer.Reset(h.nextCleanupDeadline())

		select {
		case <-h.closeChan:
			return
		case <-h.cleanupScheduled:
			continue
		case <-timer.Chan():
			timer.SetRead()
			h.runDueCleanups(time.Now())
		}
	}
}

func (h *packetHandlerMap) nextCleanupDeadline() time.Time {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	if len(h.pendingCleanups) == 0 {
		return time.Time{}
	}

	deadline := h.pendingCleanups[0].deadline
	for _, entry := range h.pendingCleanups[1:] {
		if entry.deadline.Before(deadline) {
			deadline = entry.deadline
		}
	}
	return deadline
}

func (h *packetHandlerMap) runDueCleanups(now time.Time) {
	h.mutex.Lock()
	if len(h.pendingCleanups) == 0 {
		h.mutex.Unlock()
		return
	}

	pending := h.pendingCleanups
	remaining := pending[:0]
	var due []packetHandlerMapCleanupEntry
	for _, entry := range pending {
		if !entry.deadline.After(now) {
			due = append(due, entry)
			continue
		}
		remaining = append(remaining, entry)
	}
	for i := len(remaining); i < len(pending); i++ {
		pending[i] = packetHandlerMapCleanupEntry{}
	}
	h.pendingCleanups = remaining
	h.mutex.Unlock()

	for _, entry := range due {
		h.runCleanup(entry)
	}
}

func (h *packetHandlerMap) runCleanup(entry packetHandlerMapCleanupEntry) {
	h.mutex.Lock()
	switch entry.kind {
	case packetHandlerMapCleanupRetired:
		delete(h.handlers, entry.id)
	case packetHandlerMapCleanupClosed:
		for _, id := range entry.ids {
			delete(h.handlers, id)
		}
	}
	h.mutex.Unlock()

	switch entry.kind {
	case packetHandlerMapCleanupRetired:
		h.logger.Debugf("Removing connection ID %s after it has been retired.", entry.id)
	case packetHandlerMapCleanupClosed:
		h.logger.Debugf("Removing connection IDs %s for a closed connection after it has been retired.", entry.ids)
	}
}
