package filter

import (
	"hash/maphash"
	"net/http"
	"net/netip"
	"strings"
)

// Who a request is from, for the per-client limits of the classes
// (limit.go). The only use of a client's address in this process: it is
// turned into a key here, the key is held only while that client has a
// request in flight or queued, and neither is ever logged or forwarded
// (NO_LOGS.md).
//
// Where the address comes from. The edge's two ports are published to the
// lease's cloudflared and nothing else, never on a provider port
// (akash/deploy.yaml; bin/build-sdl.py refuses anything else). So the only
// connections it accepts are from inside the lease, and the one service
// there that carries outside traffic is cloudflared, with requests that came
// through Cloudflare (the relayer dials the node directly; nothing else in
// the lease speaks HTTP to the edge). Cloudflare sets CF-Connecting-IP on every
// request it proxies to the visitor's address, replacing any value the
// visitor sent, so behind the tunnel the header is Cloudflare's word, not the
// client's. It is read strictly: exactly one value, a bare IP address.
// Anything else (no header, two, garbage) falls back to the TCP peer, which
// is cloudflared's own pod address: every such request shares one key, so a
// missing or malformed header can only make a client poorer, never give it
// a fresh allowance.
//
// IPv6 addresses are grouped by /64: one subscriber is usually handed a
// whole /64 (or more), so keying a single address would give one client
// 2^64 allowances. IPv4 is keyed by address. An IPv4-mapped IPv6 address
// is its IPv4 address.
//
// The key is a keyed hash (a random seed per process) of the grouped
// address, so the maps in limit.go hold no addresses at all, and the hash
// means nothing outside this process's lifetime.
const clientIPHeader = "CF-Connecting-IP"

var clientSeed = maphash.MakeSeed()

func clientKey(r *http.Request) uint64 {
	if vs := r.Header.Values(clientIPHeader); len(vs) == 1 {
		if a, err := netip.ParseAddr(strings.TrimSpace(vs[0])); err == nil && a.Zone() == "" {
			return keyOf(a, 'h')
		}
	}
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		return keyOf(ap.Addr(), 'p')
	}
	return maphash.String(clientSeed, "unknown")
}

// keyOf hashes the client's group: its IPv4 address, or its IPv6 /64.
// src keeps a header address and a TCP peer apart.
func keyOf(a netip.Addr, src byte) uint64 {
	a = a.Unmap().WithZone("")
	if a.Is6() {
		a = netip.PrefixFrom(a, 64).Masked().Addr()
	}
	b := a.As16()
	var h maphash.Hash
	h.SetSeed(clientSeed)
	_ = h.WriteByte(src)
	_, _ = h.Write(b[:])
	return h.Sum64()
}
