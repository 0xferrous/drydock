// Command drydock-portal serves the drydock Driver over HTTP so a guest VM can
// drive preview VMs running on the physical host.
package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/0xferrous/drydock/internal/drydock"
)

// mapList collects repeatable --map FROM=TO flags.
type mapList [][2]string

func (m *mapList) String() string {
	parts := make([]string, 0, len(*m))
	for _, pair := range *m {
		parts = append(parts, pair[0]+"="+pair[1])
	}
	return strings.Join(parts, ",")
}

func (m *mapList) Set(value string) error {
	from, to, ok := strings.Cut(value, "=")
	if !ok || from == "" {
		return &net.AddrError{Err: "expected FROM=TO", Addr: value}
	}
	*m = append(*m, [2]string{strings.TrimRight(from, "/"), strings.TrimRight(to, "/")})
	return nil
}

func main() {
	bind := flag.String("bind", "127.0.0.1", "address to listen on")
	port := flag.Int("port", 8787, "port to listen on")
	tokenFile := flag.String("token-file", "", "file containing a bearer token to require")
	defaultFlake := flag.String("default-flake", "", "flake used when the client omits one")
	defaultHost := flag.String("default-host", "", "nixosConfiguration used when omitted")
	cwd := flag.String("cwd", "", "working directory for drydock invocations")
	var maps mapList
	flag.Var(&maps, "map", "rewrite guest-visible path prefixes to host paths (FROM=TO)")
	flag.Parse()

	token := ""
	if *tokenFile != "" {
		data, err := os.ReadFile(*tokenFile)
		if err != nil {
			log.Fatalf("drydock-portal: cannot read token file: %v", err)
		}
		token = strings.TrimSpace(string(data))
	}
	if *cwd != "" {
		if err := os.Chdir(*cwd); err != nil {
			log.Fatalf("drydock-portal: cannot chdir to %s: %v", *cwd, err)
		}
	}

	server := &drydock.Server{
		Driver:       drydock.NewEngine(),
		Token:        token,
		DefaultFlake: *defaultFlake,
		DefaultHost:  *defaultHost,
		PathMap:      maps,
	}
	addr := net.JoinHostPort(*bind, strconv.Itoa(*port))
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
		<-signals
		_ = httpServer.Close()
	}()

	log.Printf("drydock-portal %s on http://%s (flake=%s, host=%s)",
		drydock.Version, addr, orDash(*defaultFlake), orDash(*defaultHost))
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("drydock-portal: %v", err)
	}
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
