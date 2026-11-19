package p2p

import (
	"context"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/namsaeg/chainboard/internal/nat"
)

// Port mapping: a node that listens behind a home router asks it (NAT-PMP,
// then UPnP) to forward the port, and announces the external address so
// peers can dial it. Without a mapping the node still works over its own
// outbound connections; data only it holds then reaches others by relay.

const (
	natLease = 2 * time.Hour
	natRetry = 10 * time.Minute
)

type natState struct {
	mu     sync.Mutex
	m      *nat.Mapping
	ext    string // external address once mapped
	status string // for the UI
}

// NATStatus says whether the listening port is reachable from outside
// through a mapping, and how.
func (h *Host) NATStatus() (ext, status string) {
	h.nat.mu.Lock()
	defer h.nat.mu.Unlock()
	return h.nat.ext, h.nat.status
}

func (h *Host) natLoop() {
	defer h.wg.Done()
	_, portStr, err := net.SplitHostPort(h.ln.Addr().String())
	if err != nil {
		return
	}
	port, _ := strconv.Atoi(portStr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-h.quit
		cancel()
	}()
	for {
		m, err := nat.Map(ctx, *h.cfg.NAT, port, natLease)
		wait := natRetry
		h.nat.mu.Lock()
		if err != nil {
			h.nat.status = "포트 열기 실패: 공유기가 NAT-PMP/UPnP를 지원하지 않거나 꺼져 있어요 (중계로 동작)"
			if h.nat.m == nil {
				h.nat.ext = ""
			}
		} else {
			if h.nat.m == nil || h.nat.ext != m.Addr() {
				h.cfg.Logf("p2p: port %d forwarded by %s: reachable at %s", port, m.Method, m.Addr())
			}
			h.nat.m, h.nat.ext = m, m.Addr()
			h.nat.status = m.Method + "로 포트를 열었어요"
			if m.Lifetime > 0 {
				wait = m.Lifetime / 2
			} else {
				wait = natLease
			}
		}
		h.nat.mu.Unlock()
		select {
		case <-h.quit:
			h.nat.mu.Lock()
			last := h.nat.m
			h.nat.mu.Unlock()
			if last != nil {
				uctx, ucancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = last.Unmap(uctx)
				ucancel()
			}
			return
		case <-time.After(wait):
		}
	}
}
