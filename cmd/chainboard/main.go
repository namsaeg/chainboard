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

	"github.com/namsaeg/chainboard/internal/api"
	"github.com/namsaeg/chainboard/internal/node"
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
	flag.Parse()

	if *version {
		fmt.Println("chainboard", params.Version)
		return
	}
	if params.ByName(*network) == nil {
		log.Fatalf("unknown network %q", *network)
	}
	if *delay < 0 {
		*delay = 5 * time.Minute
		if *network == "devnet" {
			*delay = 0
		}
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
	n, err := node.New(node.Config{
		Network:      *network,
		DataDir:      *dataDir,
		Listen:       *listen,
		Seeds:        seedList,
		Mine:         *mine,
		PledgeBytes:  uint64(*pledge * 1e9),
		SendDelayMax: *delay,
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
