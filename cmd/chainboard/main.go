// Command chainboard runs a chainboard node with its local web UI.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/namsaeg/chainboard/internal/anonnet"
	"github.com/namsaeg/chainboard/internal/api"
	"github.com/namsaeg/chainboard/internal/nat"
	"github.com/namsaeg/chainboard/internal/node"
	"github.com/namsaeg/chainboard/internal/p2p"
	"github.com/namsaeg/chainboard/internal/params"
)

func defaultDataDir() string {
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "chainboard")
	}
	return "chainboard-data"
}

func main() {
	network := flag.String("network", "testnet", "network: testnet or devnet")
	dataDir := flag.String("datadir", defaultDataDir(), "data directory")
	listen := flag.String("listen", ":7700", "p2p listen address (empty disables inbound)")
	seeds := flag.String("seeds", "", "comma-separated peers to connect to (host:port)")
	httpAddr := flag.String("http", "127.0.0.1:7780", "web UI and API address (keep it on localhost)")
	mine := flag.Bool("mine", false, "store data and mine")
	pledge := flag.Float64("pledge-gb", 100, "disk space offered for storage, in GB")
	delay := flag.Duration("send-delay", -1, "max random delay before own transactions leave (default 5m; 0 on devnet)")
	version := flag.Bool("version", false, "print version and exit")
	pwFile := flag.String("password-file", "", "file holding the key password (else CHAINBOARD_PASSWORD, else asked)")
	changePw := flag.Bool("change-password", false, "lock, relock or unlock the key files and exit")
	natMode := flag.String("nat", "auto", "ask the router to forward the p2p port (NAT-PMP, then UPnP): auto or none")
	proxy := flag.String("proxy", "", "connect to peers through a SOCKS5 proxy: Tor (socks5://127.0.0.1:9050) or Nym's socks5 client")
	torControl := flag.String("tor-control", "", "Tor control port (127.0.0.1:9051): publish this node as an onion service")
	torPwFile := flag.String("tor-password-file", "", "file holding the Tor control password, if Tor uses one")
	torOnly := flag.Bool("tor-only", false, "never connect directly: needs -proxy; listens only for the onion service")
	cover := flag.Bool("cover", false, "send cover traffic: fixed-size frames to the stem peer at a fixed rate (costs upload, see -cover-size)")
	coverSize := flag.Int("cover-size", 2048, "cover frame size in bytes")
	coverEvery := flag.Duration("cover-every", 2*time.Second, "time between cover frames")
	flag.Parse()

	if *version {
		fmt.Println("chainboard", params.Version)
		return
	}
	p := params.ByName(*network)
	if p == nil {
		log.Fatalf("unknown network %q", *network)
	}
	keyDir := filepath.Join(*dataDir, p.Name)
	if *changePw {
		if err := changePassword(keyDir); err != nil {
			log.Fatal(err)
		}
		return
	}
	pw, err := password(keyDir, *pwFile)
	if err != nil {
		log.Fatal(err)
	}
	if *delay < 0 {
		*delay = 5 * time.Minute
		if *network == "devnet" {
			*delay = 0
		}
	}
	var embargo time.Duration // Dandelion++ default (30–60 s)
	if *network == "devnet" {
		embargo = 3 * time.Second // devnet favours speed over sender privacy
	}
	host, _, err := net.SplitHostPort(*httpAddr)
	if err == nil && host != "127.0.0.1" && host != "localhost" && host != "::1" {
		log.Printf("WARNING: the API controls your wallet; %s is reachable from other machines", *httpAddr)
	}
	var seedList []string
	for _, s := range strings.Split(*seeds, ",") {
		if s = strings.TrimSpace(s); s != "" {
			seedList = append(seedList, s)
		}
	}
	ns := privacyConfig(*proxy, *torControl, *torPwFile, *torOnly, *cover, *coverSize, *coverEvery, *listen)
	if ns.err != nil {
		log.Fatal(ns.err)
	}
	n, err := node.New(node.Config{
		Network:      *network,
		DataDir:      *dataDir,
		Listen:       ns.listen,
		Seeds:        seedList,
		Mine:         *mine,
		PledgeBytes:  uint64(*pledge * 1e9),
		SendDelayMax: *delay,
		Embargo:      embargo,
		NAT:          natConfig(*natMode, ns.listen, *torOnly),
		Proxy:        ns.proxy,
		ProxyOnly:    *torOnly,
		TorControl:   *torControl,
		TorPassword:  ns.torPassword,
		Cover:        ns.cover,
		Password:     pw,
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := n.Start(); err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Addr: *httpAddr, Handler: api.Handler(n), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	log.Printf("chainboard %s on %s · wallet %s", params.Version, n.P.Name, n.Wallet.Address())
	log.Printf("web UI: http://%s  · p2p: %s · mining: %v", *httpAddr, n.Host.Addr(), *mine)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	log.Print("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	n.Stop()
}

// natConfig turns port mapping on unless it is off or there is nothing a
// router could forward (no listener, or one on loopback).
func natConfig(mode, listen string, torOnly bool) *nat.Config {
	if mode == "none" || listen == "" || torOnly {
		return nil
	}
	if host, _, err := net.SplitHostPort(listen); err == nil {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return &nat.Config{}
}

type netSettings struct {
	listen      string
	proxy       *anonnet.Proxy
	torPassword string
	cover       p2p.CoverConfig
	err         error
}

// privacyConfig checks the proxy, Tor and cover traffic flags. Tor-only
// keeps the listener on loopback (only Tor's onion service reaches it), or
// drops it when there is no onion service.
func privacyConfig(proxy, torControl, torPwFile string, torOnly, cover bool, coverSize int, coverEvery time.Duration, listen string) netSettings {
	s := netSettings{listen: listen}
	if proxy != "" {
		s.proxy, s.err = anonnet.ParseProxy(proxy)
		if s.err != nil {
			return s
		}
	}
	if torPwFile != "" {
		b, err := os.ReadFile(torPwFile)
		if err != nil {
			s.err = err
			return s
		}
		s.torPassword = strings.TrimRight(string(b), "\r\n")
	}
	if torOnly {
		if s.proxy == nil {
			s.err = fmt.Errorf("-tor-only needs -proxy (Tor's SOCKS port, e.g. socks5://127.0.0.1:9050)")
			return s
		}
		s.listen = ""
		if torControl != "" {
			_, port, err := net.SplitHostPort(listen)
			if err != nil || port == "" {
				port = "7700"
			}
			s.listen = net.JoinHostPort("127.0.0.1", port)
		}
	}
	if cover {
		if coverSize < 256 || coverSize > 65535 || coverEvery < 100*time.Millisecond {
			s.err = fmt.Errorf("-cover-size must be 256 to 65535 bytes and -cover-every at least 100ms")
			return s
		}
		s.cover = p2p.CoverConfig{Size: coverSize, Every: coverEvery}
		log.Printf("cover traffic on: %d bytes every %v (about %.1f GB upload a month)", coverSize, coverEvery,
			s.cover.BytesPerSecond()*30*86400/1e9)
	}
	return s
}
