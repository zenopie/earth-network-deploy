package filter

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// R6-E-2: a POST that declares a large body and sends none reserves only
// firstChunk x factor, so idle connections cannot hold the budget: with
// 256 of them open, small POSTs on both listeners are still served.
func TestIdleBodiesHoldLittle(t *testing.T) {
	node := newFakeNode(t)
	c := DefaultClasses()
	rpcSrv := httptest.NewServer(NewRPC(node.srv.URL, client(), c))
	defer rpcSrv.Close()
	lcdSrv := httptest.NewServer(NewLCD(node.srv.URL, client(), c))
	defer lcdSrv.Close()

	const idle = 256
	var conns []net.Conn
	for i := 0; i < idle; i++ {
		addr := rpcSrv.Listener.Addr().String()
		path := "/"
		if i%2 == 1 {
			addr, path = lcdSrv.Listener.Addr().String(), "/cosmos/tx/v1beta1/txs"
		}
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
		fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", path, maxBody)
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	time.Sleep(300 * time.Millisecond)
	if used, most := publicBodies.inUse(), int64(idle*firstChunk*factorJSONRPC); used > most {
		t.Fatalf("%d idle bodies hold %d bytes, want at most %d", idle, used, most)
	}
	resp, err := http.Post(rpcSrv.URL+"/", jsonCT, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"abci_query","params":{"path":"/cosmos.bank.v1beta1.Query/SupplyOf","data":"0a057565727468"}}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("abci_query POST behind %d idle bodies: %d", idle, resp.StatusCode)
	}
	resp, err = http.Post(lcdSrv.URL+"/cosmos/tx/v1beta1/txs", jsonCT, strings.NewReader(`{"tx_bytes":"CgQKAggB","mode":"BROADCAST_MODE_SYNC"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("LCD broadcast behind %d idle bodies: %d", idle, resp.StatusCode)
	}
	for _, c := range conns {
		c.Close()
	}
	conns = nil
	deadline := time.Now().Add(3 * time.Second)
	for publicBodies.inUse() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := publicBodies.inUse(); n != 0 {
		t.Fatalf("budget not returned after the idle clients left: %d", n)
	}
}

// A body that arrives in pieces grows its reservation with what arrived.
func TestBodyChunksGrow(t *testing.T) {
	node := newFakeNode(t)
	srv := httptest.NewServer(NewLCD(node.srv.URL, client(), DefaultClasses()))
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST /cosmos/tx/v1beta1/txs HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", maxBody)
	sent := 10 << 10
	_, _ = conn.Write([]byte(strings.Repeat(" ", sent)))
	time.Sleep(200 * time.Millisecond)
	if used := publicBodies.inUse(); used > int64(3*sent*factorLCD) {
		t.Fatalf("%d bytes sent, %d reserved", sent, used)
	}
	conn.Close()
	deadline := time.Now().Add(3 * time.Second)
	for publicBodies.inUse() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}
