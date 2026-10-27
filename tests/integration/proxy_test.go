//go:build integration

package integration

import (
	"io"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
)

// tcpProxy forwards TCP connections to the real Home Assistant so a test can
// cut the network between NeptHass and Home Assistant deterministically,
// without touching any container.
type tcpProxy struct {
	ln     net.Listener
	target string
	down   atomic.Bool

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func newTCPProxy(t *testing.T, homeAssistantURL string) *tcpProxy {
	t.Helper()
	u, err := url.Parse(homeAssistantURL)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &tcpProxy{ln: ln, target: u.Host, conns: map[net.Conn]struct{}{}}
	go p.accept()
	t.Cleanup(func() { _ = ln.Close(); p.closeAll() })
	return p
}

// URL is the base URL NeptHass should use instead of Home Assistant's.
func (p *tcpProxy) URL() string { return "http://" + p.ln.Addr().String() }

// SetDown cuts (true) or restores (false) the network: existing connections
// are dropped and new ones are closed immediately.
func (p *tcpProxy) SetDown(down bool) {
	p.down.Store(down)
	if down {
		p.closeAll()
	}
}

func (p *tcpProxy) accept() {
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		if p.down.Load() {
			_ = client.Close()
			continue
		}
		server, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = client.Close()
			continue
		}
		p.track(client, server)
		go func() { _, _ = io.Copy(server, client); _ = server.Close(); _ = client.Close() }()
		go func() { _, _ = io.Copy(client, server); _ = client.Close(); _ = server.Close() }()
	}
}

func (p *tcpProxy) track(conns ...net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range conns {
		p.conns[c] = struct{}{}
	}
}

func (p *tcpProxy) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		_ = c.Close()
		delete(p.conns, c)
	}
}
