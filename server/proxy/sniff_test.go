package proxy

import (
	"crypto/tls"
	"net"
	"testing"
)

func TestSniffDomainHTTP(t *testing.T) {
	server, client := net.Pipe()
	go func() {
		client.Write([]byte("GET / HTTP/1.1\r\nHost: example.com\r\nUser-Agent: test\r\n\r\n"))
	}()
	domain, peeked := sniffDomain(server)
	if domain != "example.com" {
		t.Fatalf("expected example.com, got %q", domain)
	}
	if len(peeked) == 0 {
		t.Fatal("expected non-empty peeked bytes")
	}
	t.Logf("domain=%s peeked_len=%d", domain, len(peeked))
}

func TestSniffDomainTLS(t *testing.T) {
	server, client := net.Pipe()
	go func() {
		tlsClient := tls.Client(client, &tls.Config{ServerName: "example.org", InsecureSkipVerify: true})
		tlsClient.Handshake() // will fail (no real server), that's fine, ClientHello is already sent
	}()
	domain, peeked := sniffDomain(server)
	if domain != "example.org" {
		t.Fatalf("expected example.org, got %q", domain)
	}
	if len(peeked) == 0 || peeked[0] != 0x16 {
		t.Fatalf("expected peeked TLS record starting with 0x16, got %v", peeked[:1])
	}
	t.Logf("domain=%s peeked_len=%d", domain, len(peeked))
}

func TestSniffDomainNoData(t *testing.T) {
	server, client := net.Pipe()
	client.Close()
	domain, peeked := sniffDomain(server)
	if domain != "" || peeked != nil {
		t.Fatalf("expected empty result on closed conn, got domain=%q peeked=%v", domain, peeked)
	}
}
