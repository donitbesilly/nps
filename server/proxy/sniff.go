package proxy

import (
	"bytes"
	"crypto/tls"
	"net"
	"strings"
	"time"
)

// sniffTimeout bounds how long ProcessTunnel waits for the client to send
// its first bytes before giving up on domain sniffing and forwarding
// normally. Client-first protocols (HTTP, TLS, RDP) reply almost
// instantly; server-first protocols simply pay this once as extra latency
// before the target connection is dialed.
const sniffTimeout = 500 * time.Millisecond

// sniffDomain peeks at the first bytes of an unread connection, looking for
// an HTTP Host header or a TLS ClientHello SNI hostname, WITHOUT consuming
// data that isn't handed back: the caller must write peeked to the target
// connection before resuming the normal bidirectional copy (see
// conn.CopyWaitGroup's rb parameter), exactly as ProcessHttp already does
// for host-based routing. domain is "" if nothing recognizable was found or
// the client didn't send anything within sniffTimeout.
func sniffDomain(c net.Conn) (domain string, peeked []byte) {
	_ = c.SetReadDeadline(time.Now().Add(sniffTimeout))
	defer c.SetReadDeadline(time.Time{})

	buf := make([]byte, 4096)
	n, err := c.Read(buf)
	if err != nil || n == 0 {
		return "", nil
	}
	peeked = buf[:n]

	if peeked[0] == 0x16 { // TLS handshake record
		return parseSNI(peeked), peeked
	}
	return parseHTTPHost(peeked), peeked
}

// parseHTTPHost scans for a "Host:" header line. It works on a possibly
// incomplete request (only the bytes read so far), which is fine since the
// Host header is almost always within the first packet of an HTTP request.
func parseHTTPHost(b []byte) string {
	for _, line := range bytes.Split(b, []byte("\r\n")) {
		if len(line) > 5 && strings.EqualFold(string(line[:5]), "Host:") {
			return strings.TrimSpace(string(line[5:]))
		}
	}
	return ""
}

// parseSNI extracts the SNI server name from a TLS ClientHello using the
// standard library's own parser (via tls.Server + GetConfigForClient),
// rather than hand-rolling a fragile record/extension parser. The fake
// connection only ever sees the already-peeked bytes and discards any
// writes, so this can't touch the real network connection.
func parseSNI(peeked []byte) string {
	var sni string
	config := &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			sni = hello.ServerName
			return nil, errBreakAfterSniff
		},
	}
	_ = tls.Server(&sniffPeekConn{r: bytes.NewReader(peeked)}, config).Handshake()
	return sni
}

var errBreakAfterSniff = errBreak{}

type errBreak struct{}

func (errBreak) Error() string { return "stop after sniffing SNI" }

// sniffPeekConn is a throwaway net.Conn backed by an in-memory buffer, used
// only to feed the peeked bytes through tls.Server's ClientHello parser.
// Reads beyond the buffer and all writes are no-ops.
type sniffPeekConn struct {
	r *bytes.Reader
}

func (p *sniffPeekConn) Read(b []byte) (int, error)         { return p.r.Read(b) }
func (p *sniffPeekConn) Write(b []byte) (int, error)        { return len(b), nil }
func (p *sniffPeekConn) Close() error                       { return nil }
func (p *sniffPeekConn) LocalAddr() net.Addr                { return nil }
func (p *sniffPeekConn) RemoteAddr() net.Addr                { return nil }
func (p *sniffPeekConn) SetDeadline(t time.Time) error      { return nil }
func (p *sniffPeekConn) SetReadDeadline(t time.Time) error  { return nil }
func (p *sniffPeekConn) SetWriteDeadline(t time.Time) error { return nil }
