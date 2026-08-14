package web

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// egressProxy forces Chrome's HTTP, HTTPS, redirect, and subresource traffic
// through the Go egress guard. Chrome receives only a loopback proxy address;
// destination DNS resolution and dialing happen here.
type egressProxy struct {
	listener  net.Listener
	server    *http.Server
	transport *http.Transport
}

func startEgressProxy(guard *egressGuard) (*egressProxy, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("web: start egress proxy: %w", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = guard.DialContext
	transport.DialTLSContext = nil
	p := &egressProxy{listener: listener, transport: transport}
	p.server = &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 15 * time.Second,
	}
	go func() { _ = p.server.Serve(listener) }()
	return p, nil
}

func (p *egressProxy) Address() string { return p.listener.Addr().String() }

func (p *egressProxy) Close() error {
	p.transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return p.server.Shutdown(ctx)
}

func (p *egressProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	if r.URL == nil || !r.URL.IsAbs() {
		http.Error(w, "web proxy requires an absolute URL", http.StatusBadRequest)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	removeHopHeaders(out.Header)
	resp, err := p.transport.RoundTrip(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	removeHopHeaders(resp.Header)
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *egressProxy) connect(w http.ResponseWriter, r *http.Request) {
	dst, err := p.transport.DialContext(r.Context(), "tcp", r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		dst.Close()
		http.Error(w, "web proxy cannot tunnel this connection", http.StatusInternalServerError)
		return
	}
	client, rw, err := hijacker.Hijack()
	if err != nil {
		dst.Close()
		return
	}
	defer client.Close()
	defer dst.Close()
	if _, err := rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := rw.Flush(); err != nil {
		return
	}
	copyBuffered(dst, rw)
	errCh := make(chan error, 2)
	go func() { _, err := io.Copy(dst, client); errCh <- err }()
	go func() { _, err := io.Copy(client, dst); errCh <- err }()
	<-errCh
}

func copyBuffered(dst net.Conn, rw *bufio.ReadWriter) {
	if rw.Reader.Buffered() > 0 {
		_, _ = io.CopyN(dst, rw, int64(rw.Reader.Buffered()))
	}
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func removeHopHeaders(header http.Header) {
	for _, token := range strings.Split(header.Get("Connection"), ",") {
		header.Del(strings.TrimSpace(token))
	}
	for _, key := range hopHeaders {
		header.Del(key)
	}
}
