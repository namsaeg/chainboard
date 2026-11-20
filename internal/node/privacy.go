package node

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/namsaeg/chainboard/internal/anonnet"
	"github.com/namsaeg/chainboard/internal/keystore"
)

// onionLoop publishes the listener as a Tor onion service (retrying while
// Tor is not up) and announces its address; the same key, kept locked in
// onion.key, keeps the same address across restarts.
func (n *Node) onionLoop(ctx context.Context) {
	defer n.wg.Done()
	target := n.Host.Addr()
	_, portStr, _ := net.SplitHostPort(target)
	port, _ := strconv.Atoi(portStr)
	keyPath := n.rlnPath("onion.key")
	for {
		key := ""
		if b, err := keystore.Read(keyPath, n.cfg.Password); err == nil {
			key = string(b)
		} else if !errors.Is(err, os.ErrNotExist) {
			n.cfg.Logf("node: onion.key: %v", err)
			return
		}
		o, err := anonnet.PublishOnion(n.cfg.TorControl, n.cfg.TorPassword, key, port, target)
		if err == nil {
			if o.Key != "" {
				if err := keystore.Write(keyPath, []byte(o.Key), n.cfg.Password); err != nil {
					n.cfg.Logf("node: saving onion.key: %v", err)
				}
			}
			n.Host.SetOnion(o.Addr)
			n.cfg.Logf("node: onion service %s", o.Addr)
			<-ctx.Done()
			_ = o.Close()
			return
		}
		n.cfg.Logf("node: onion service: %v (retrying in a minute)", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

// PrivacyInfo describes how this node connects, for the UI.
type PrivacyInfo struct {
	Proxy       bool    `json:"proxy"`
	ProxyOnly   bool    `json:"proxy_only"`
	Onion       string  `json:"onion,omitempty"`
	Cover       bool    `json:"cover"`
	CoverBps    float64 `json:"cover_bytes_per_second,omitempty"`
	CoverGBMon  float64 `json:"cover_gb_per_month,omitempty"`
	CoverSent   uint64  `json:"cover_frames_sent,omitempty"`
	CoverFilled uint64  `json:"cover_frames_filled,omitempty"`
}

// Privacy reports the proxy, onion service and cover traffic.
func (n *Node) Privacy() PrivacyInfo {
	pi := PrivacyInfo{Proxy: n.cfg.Proxy != nil, ProxyOnly: n.cfg.ProxyOnly, Onion: n.Host.Onion(), Cover: n.cfg.Cover.On()}
	if pi.Cover {
		pi.CoverBps = n.cfg.Cover.BytesPerSecond()
		pi.CoverGBMon = pi.CoverBps * 30 * 86400 / 1e9
		pi.CoverSent, pi.CoverFilled, _ = n.Host.CoverStats()
	}
	return pi
}

// Settings are the node's start-up settings that the settings page shows
// (they are changed with command-line flags, then a restart).
type Settings struct {
	Mine         bool    `json:"mine"`
	PledgeGB     float64 `json:"pledge_gb"`
	SendDelaySec int     `json:"send_delay_seconds"`
	Locked       bool    `json:"keys_locked"` // key files are password-protected
}

// Settings reports the start-up settings.
func (n *Node) Settings() Settings {
	return Settings{Mine: n.cfg.Mine, PledgeGB: float64(n.cfg.PledgeBytes) / 1e9, SendDelaySec: int(n.cfg.SendDelayMax / time.Second),
		Locked: n.cfg.Password != ""}
}
