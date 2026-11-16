package netpolicy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingUpstream is a TCP server on loopback that keeps what every
// connection sent; with answer set it answers each HTTP request on a
// connection with 200 and "ok".
type recordingUpstream struct {
	ap   netip.AddrPort
	mu   sync.Mutex
	got  []byte
	conn atomic.Int64
}

func newRecordingUpstream(t *testing.T, answer bool) *recordingUpstream {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	u := &recordingUpstream{ap: netip.MustParseAddrPort(ln.Addr().String())}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			u.conn.Add(1)
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(20 * time.Second))
				r := io.TeeReader(c, writerFunc(func(b []byte) (int, error) {
					u.mu.Lock()
					u.got = append(u.got, b...)
					u.mu.Unlock()
					return len(b), nil
				}))
				if !answer {
					_, _ = io.Copy(io.Discard, r)
					return
				}
				br := bufio.NewReader(r)
				for {
					req, err := http.ReadRequest(br)
					if err != nil {
						return
					}
					_, _ = io.Copy(io.Discard, req.Body)
					if _, err := io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"); err != nil {
						return
					}
				}
			}()
		}
	}()
	return u
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

func (u *recordingUpstream) received() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return string(u.got)
}

func (u *recordingUpstream) proxy(policy func() (ProxyPolicy, error), resolve func(context.Context, string) ([]netip.Addr, error)) *Proxy {
	if resolve == nil {
		resolve = func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{u.ap.Addr()}, nil }
	}
	return &Proxy{Policy: policy, Resolve: resolve, Exempt: []netip.AddrPort{u.ap}, Tunnels: &Tunnels{}, RecheckEvery: time.Hour}
}

// TestProxyTunnelStart: an exception that ends while the proxy resolves
// or dials forwards nothing: the policy is checked again once the
// connection exists, before the tunnel is acknowledged or a byte is
// forwarded (finding E1-12).
func TestProxyTunnelStart(t *testing.T) {
	early := "GET /early HTTP/1.1\r\nHost: raw.test\r\n\r\n"
	for _, tc := range []struct {
		name    string
		connect bool
		// slow: the exception ends while the name is resolved; otherwise
		// it is revoked between the first check and the second (while
		// dialling).
		slow bool
		keep bool // the positive control: it stays in force
	}{
		{"CONNECT, expiry during resolution", true, true, false},
		{"CONNECT, revocation during dialling", true, false, false},
		{"plain request, expiry during resolution", false, true, false},
		{"plain request, revocation during dialling", false, false, false},
		{"CONNECT, still allowed", true, false, true},
		{"plain request, still allowed", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newRecordingUpstream(t, !tc.connect)
			exc, err := Expand([]string{"raw.test:" + strconv.Itoa(int(up.ap.Port()))}, nil)
			if err != nil {
				t.Fatal(err)
			}
			end := time.Now().Add(time.Hour)
			if tc.slow {
				end = time.Now().Add(150 * time.Millisecond)
			}
			var calls atomic.Int64
			policy := func() (ProxyPolicy, error) {
				n := calls.Add(1)
				if time.Now().After(end) || (!tc.slow && !tc.keep && n > 1) {
					return ProxyPolicy{}, nil
				}
				return ProxyPolicy{Exceptions: exc, Ends: end}, nil
			}
			var resolve func(context.Context, string) ([]netip.Addr, error)
			if tc.slow {
				resolve = func(context.Context, string) ([]netip.Addr, error) {
					time.Sleep(400 * time.Millisecond)
					return []netip.Addr{up.ap.Addr()}, nil
				}
			}
			p := up.proxy(policy, resolve)
			c := serveOnce(t, p)
			_ = c.SetDeadline(time.Now().Add(15 * time.Second))
			target := fmt.Sprintf("raw.test:%d", up.ap.Port())
			if tc.connect {
				fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n%s", target, target, early)
			} else {
				fmt.Fprintf(c, "GET http://%s/early HTTP/1.1\r\nHost: raw.test\r\n\r\n", target)
			}
			br := bufio.NewReader(c)
			resp, err := http.ReadResponse(br, &http.Request{Method: map[bool]string{true: "CONNECT", false: "GET"}[tc.connect]})
			if err != nil {
				t.Fatal(err)
			}
			if tc.keep {
				if resp.StatusCode != 200 {
					t.Fatalf("answer %d", resp.StatusCode)
				}
				if tc.connect {
					// The early request reaches the upstream through the tunnel.
					deadline := time.Now().Add(5 * time.Second)
					for !strings.Contains(up.received(), "GET /early") && time.Now().Before(deadline) {
						time.Sleep(10 * time.Millisecond)
					}
				}
				if !strings.Contains(up.received(), "GET /early") {
					t.Errorf("the upstream received %q", up.received())
				}
				return
			}
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 403 || !strings.Contains(string(body), "not on this box's allowlist") {
				t.Errorf("answer %d %q", resp.StatusCode, body)
			}
			time.Sleep(200 * time.Millisecond)
			if got := up.received(); got != "" {
				t.Errorf("the upstream received %q", got)
			}
			if n := p.Tunnels.Len(); n != 0 {
				t.Errorf("%d connections registered", n)
			}
			if up.conn.Load() == 0 {
				t.Error("the proxy never connected: the check came too early to test this")
			}
		})
	}
}

// TestProxyHeadBudget: each head may consume at most 64 KiB, counting
// what was buffered after the head before it: an interim answer followed
// at once by a larger final head, and a pipelined request with a larger
// head, are refused; heads within the limit still pass (finding E1-15).
func TestProxyHeadBudget(t *testing.T) {
	big := func(n int) string { return "X-Big: " + strings.Repeat("h", n) + "\r\n" }
	t.Run("response head after an interim answer", func(t *testing.T) {
		for _, tc := range []struct {
			size int
			want int
		}{{72 << 10, 502}, {60 << 10, 200}} {
			p, ap := rawUpstream(t, func(c net.Conn) {
				_, _ = io.WriteString(c, "HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 200 OK\r\n"+big(tc.size)+"Content-Length: 2\r\n\r\nok")
				time.Sleep(time.Second)
			})
			c := serveOnce(t, p)
			_ = c.SetDeadline(time.Now().Add(10 * time.Second))
			fmt.Fprintf(c, "GET http://raw.test:%d/ HTTP/1.1\r\nHost: raw.test\r\n\r\n", ap.Port())
			resp, err := http.ReadResponse(bufio.NewReader(c), nil)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.want || (tc.want == 502 && !strings.Contains(string(b), "too large")) {
				t.Errorf("a head of %d bytes: %d %q", tc.size, resp.StatusCode, b)
			}
		}
	})
	t.Run("pipelined request head", func(t *testing.T) {
		for _, tc := range []struct {
			size int
			want int
		}{{72 << 10, 431}, {60 << 10, 200}} {
			up := newRecordingUpstream(t, true)
			allow, _ := Expand([]string{"raw.test:" + strconv.Itoa(int(up.ap.Port()))}, nil)
			p := up.proxy(func() (ProxyPolicy, error) { return ProxyPolicy{Allow: allow}, nil }, nil)
			c := serveOnce(t, p)
			_ = c.SetDeadline(time.Now().Add(10 * time.Second))
			url := fmt.Sprintf("http://raw.test:%d/", up.ap.Port())
			// Both requests in one write: the second's head is buffered
			// while the first is served.
			fmt.Fprintf(c, "GET %sone HTTP/1.1\r\nHost: raw.test\r\n\r\nGET %stwo HTTP/1.1\r\nHost: raw.test\r\n%s\r\n", url, url, big(tc.size))
			br := bufio.NewReader(c)
			first, err := http.ReadResponse(br, nil)
			if err != nil || first.StatusCode != 200 {
				t.Fatalf("the first request: %v %v", first, err)
			}
			io.ReadAll(first.Body)
			second, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Fatal(err)
			}
			io.ReadAll(second.Body)
			if second.StatusCode != tc.want {
				t.Errorf("a pipelined head of %d bytes: %d", tc.size, second.StatusCode)
			}
		}
	})
}
