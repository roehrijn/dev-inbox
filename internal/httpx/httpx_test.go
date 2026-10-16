package httpx

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blackhole forwards TCP connections to a backend until kill, after which the connections it
// already forwards silently drop every byte, like a link that died while the machine slept. New
// connections work again.
type blackhole struct {
	ln      net.Listener
	backend string
	mu      sync.Mutex
	dead    []*atomic.Bool
}

func newBlackhole(t *testing.T, backend string) *blackhole {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &blackhole{ln: ln, backend: backend}
	t.Cleanup(func() { _ = ln.Close() })
	go b.serve()
	return b
}

func (b *blackhole) serve() {
	for {
		c, err := b.ln.Accept()
		if err != nil {
			return
		}
		s, err := net.Dial("tcp", b.backend)
		if err != nil {
			_ = c.Close()
			continue
		}
		dead := new(atomic.Bool)
		b.mu.Lock()
		b.dead = append(b.dead, dead)
		b.mu.Unlock()
		go pump(s, c, dead)
		go pump(c, s, dead)
	}
}

func pump(dst, src net.Conn, dead *atomic.Bool) {
	defer func() { _ = dst.Close() }()
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if err != nil {
			return
		}
		if dead.Load() {
			continue
		}
		if _, err := dst.Write(buf[:n]); err != nil {
			return
		}
	}
}

func (b *blackhole) kill() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, d := range b.dead {
		d.Store(true)
	}
}

func TestTransportReplacesDeadConnection(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	proxy := newBlackhole(t, srv.Listener.Addr().String())

	tr := Transport()
	tr.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
	tr.HTTP2.SendPingTimeout, tr.HTTP2.PingTimeout = 100*time.Millisecond, 100*time.Millisecond
	c := &http.Client{Transport: tr, Timeout: time.Second}
	url := "https://" + proxy.ln.Addr().String()

	get := func() error {
		resp, err := c.Get(url)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.ProtoMajor != 2 {
			return errors.New("not HTTP/2: " + resp.Proto)
		}
		_, err = io.ReadAll(resp.Body)
		return err
	}
	if err := get(); err != nil {
		t.Fatalf("first request: %v", err)
	}
	proxy.kill()
	_ = get() // may fail on the dead connection
	for i := range 3 {
		if err := get(); err != nil {
			t.Fatalf("request %d after the connection died: %v", i, err)
		}
	}
}
