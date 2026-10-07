// earth-edge: the request filter in front of the validator's CometBFT RPC
// and LCD. It runs as its own service in the validator's Akash lease, from
// its own image (edge/Dockerfile: one static binary on scratch, as uid
// 65532); cloudflared's public hostnames point at it, and only it reaches
// node:26657 and node:1317.
//
//	rpc.erth.network -> cloudflared -> edge:26657 -> node:26657
//	lcd.erth.network -> cloudflared -> edge:1317  -> node:1317
//
// It serves an allowlist (filter/rpcpolicy.go, filter/lcdpolicy.go), decodes every request
// once the way the node would, and forwards a request it wrote itself, so
// no encoding trick reaches the node. Expensive classes of call are bounded
// in how many run at once (filter/limit.go). It keeps no per-client state and logs
// no request: not an address, a path, a query or a body (NO_LOGS.md
// at the repo root). Its only log lines are its start and a fatal error.
//
// Configuration (env): EDGE_RPC_LISTEN (:26657), EDGE_LCD_LISTEN (:1317),
// EDGE_RPC_UPSTREAM (http://node:26657), EDGE_LCD_UPSTREAM
// (http://node:1317), EDGE_MAX_CONNS (256 per listener),
// EDGE_BACKEND_AUTH_SHA256 (optional: hex SHA-256 of the backend's exact
// Authorization header value; requests carrying it may use the slots
// reserved for the backend, filter/limit.go). Go's own GOMAXPROCS and
// GOMEMLIMIT bound the runtime; the SDL sets both. Tests:
// filter/ (allowlists, every reported bypass, fuzzing) and conformance/
// (against CometBFT's and grpc-gateway's own code).
package main

import (
	"context"
	"encoding/hex"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/zenopie/earth-network-deploy/edge/filter"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func mustUpstream(k, def string) string {
	v := env(k, def)
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "http" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		log.Fatalf("earth-edge: %s must be http://host:port", k)
	}
	return "http://" + u.Host
}

// limitListener caps open client connections (golang.org/x/net/netutil's
// LimitListener, without the dependency).
type limitListener struct {
	net.Listener
	sem chan struct{}
}

func (l *limitListener) Accept() (net.Conn, error) {
	l.sem <- struct{}{}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	return &limitConn{Conn: c, release: func() { <-l.sem }}, nil
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

func newServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		// The outer bound; a body's own deadline is set from its size
		// (filter/body.go: 2 s + 128 KiB/s, so 10 s for 1 MiB).
		ReadTimeout:    20 * time.Second,
		WriteTimeout:   75 * time.Second,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 16 << 10,
		// net/http logs some connection errors with the remote address
		// ("http: TLS handshake error from ..."). Nothing here may log one.
		ErrorLog: log.New(io.Discard, "", 0),
	}
}

func main() {
	log.SetFlags(0)
	// It parses what strangers send; as root a bug would be root in the
	// container. The image's USER is 65532; this catches an override.
	if os.Getuid() == 0 {
		log.Fatal("earth-edge: refusing to run as root (run the image as its USER, 65532)")
	}
	maxConns, err := strconv.Atoi(env("EDGE_MAX_CONNS", "256"))
	if err != nil || maxConns < 1 {
		log.Fatal("earth-edge: EDGE_MAX_CONNS must be a positive integer")
	}
	cls := filter.DefaultClasses()
	if v := os.Getenv("EDGE_BACKEND_AUTH_SHA256"); v != "" {
		h, err := hex.DecodeString(v)
		if err != nil || len(h) != 32 {
			log.Fatal("earth-edge: EDGE_BACKEND_AUTH_SHA256 must be 64 hex digits")
		}
		cls.SetBackendAuthSHA256(h)
	}
	client := &http.Client{
		Transport: filter.NewTransport(),
		// The node never redirects a request this proxy builds (paths are
		// clean); if it did, the client gets the 3xx, not a second fetch.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	rpcUp := mustUpstream("EDGE_RPC_UPSTREAM", "http://node:26657")
	lcdUp := mustUpstream("EDGE_LCD_UPSTREAM", "http://node:1317")

	servers := map[string]*http.Server{
		env("EDGE_RPC_LISTEN", ":26657"): newServer(filter.NewRPC(rpcUp, client, cls)),
		env("EDGE_LCD_LISTEN", ":1317"):  newServer(filter.NewLCD(lcdUp, client, cls)),
	}
	errc := make(chan error, len(servers))
	for addr, srv := range servers {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			log.Fatalf("earth-edge: listen %s: %v", addr, err)
		}
		go func(srv *http.Server, ln net.Listener) {
			errc <- srv.Serve(&limitListener{Listener: ln, sem: make(chan struct{}, maxConns)})
		}(srv, ln)
	}
	nr, ng := filter.Counts()
	log.Printf("earth-edge: rpc %s -> %s, lcd %s -> %s, %d LCD routes, %d gRPC paths over abci_query, backend reserve %v",
		env("EDGE_RPC_LISTEN", ":26657"), rpcUp, env("EDGE_LCD_LISTEN", ":1317"), lcdUp, nr, ng,
		os.Getenv("EDGE_BACKEND_AUTH_SHA256") != "")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	select {
	case <-sig:
	case err := <-errc:
		log.Fatalf("earth-edge: server stopped: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(ctx)
	}
}
