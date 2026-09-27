// Package httplimits provides the defensive limits shared by Tenant's
// long-running HTTP servers. Streaming routes deliberately use no server-wide
// WriteTimeout; callers can instead apply a deadline to finite responses with
// Protect.
package httplimits

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const (
	// ReadHeaderTimeout bounds the time a client may spend sending request
	// headers, protecting listeners from slowloris-style connections.
	ReadHeaderTimeout = 5 * time.Second
	// IdleTimeout bounds keep-alive connections between requests. It does not
	// interrupt an active SSE or other streaming response.
	IdleTimeout = 90 * time.Second
	// ResponseWriteTimeout bounds finite response writes. Streaming routes must
	// be identified to Protect so they remain deadline-free.
	ResponseWriteTimeout = 30 * time.Second
	// MaxHeaderBytes is generous enough for normal auth/cookie headers while
	// preventing unbounded header allocation.
	MaxHeaderBytes = 64 << 10
)

// Configure applies the shared connection-level limits to srv. WriteTimeout is
// intentionally left at zero because several callers serve long-lived streams.
// Its error log (TLS handshake failures, handler panics) goes to the process
// logger at WARN, which serve and the TUI point at the log book, instead of
// net/http's default of stderr. Every server is configured after that.
func Configure(srv *http.Server) {
	srv.ReadHeaderTimeout = ReadHeaderTimeout
	srv.IdleTimeout = IdleTimeout
	srv.MaxHeaderBytes = MaxHeaderBytes
	srv.ErrorLog = slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn)
}

// Protect caps POST/PUT request bodies and gives non-streaming responses a
// per-request write deadline. Requests whose Content-Length already exceeds
// maxBodyBytes are rejected before dispatch; MaxBytesReader also bounds bodies
// whose size is not known up front.
func Protect(next http.Handler, maxBodyBytes int64, writeTimeout time.Duration, isStreaming func(*http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if maxBodyBytes > 0 && (r.Method == http.MethodPost || r.Method == http.MethodPut) {
			if r.ContentLength > maxBodyBytes {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
			body, err := io.ReadAll(r.Body)
			if err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
					return
				}
				http.Error(w, "could not read request body", http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}

		streaming := isStreaming != nil && isStreaming(r)
		if writeTimeout > 0 && !streaming {
			controller := http.NewResponseController(w)
			if err := controller.SetWriteDeadline(time.Now().Add(writeTimeout)); err == nil {
				defer controller.SetWriteDeadline(time.Time{})
			}
		}
		next.ServeHTTP(w, r)
	})
}
