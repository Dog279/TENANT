package mcpremote

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// codeDebounce is how long awaitCode waits, after the first matching callback,
// for a fresher one. Atlassian's consent is a TWO-STEP flow (grant access, then
// select site); a re-authorize mints a NEW code that SUPERSEDES the earlier one,
// so redeeming the first yields invalid_grant "grant not found". Keeping the
// LAST code within this window defeats that race. Sub-second — negligible UX cost.
const codeDebounce = 800 * time.Millisecond

type authResult struct{ code, state string }

// newCodeFetcher starts a localhost callback server and returns an
// AuthorizationCodeFetcher that opens the consent URL in a browser and returns
// the FRESHEST authorization code matching the request's state (see awaitCode).
// The returned stop() shuts the server down. This is the only hand-rolled OAuth
// piece — the SDK handles DCR/PKCE/exchange/refresh.
func newCodeFetcher(addr string, open func(string) error) (auth.AuthorizationCodeFetcher, func(), error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("mcpremote: bind callback %s: %w (is the port free?)", addr, err)
	}
	results := make(chan authResult, 8) // hold several hits; awaitCode picks the freshest
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			http.Error(w, "authorization denied: "+e, http.StatusBadRequest)
			return
		}
		if code := q.Get("code"); code != "" {
			select {
			case results <- authResult{code: code, state: q.Get("state")}:
			default: // buffer full — extremely unlikely; the freshest still wins
			}
		}
		_, _ = w.Write([]byte("Connected. You can close this tab and return to Tenant."))
	})
	srv := &http.Server{Handler: mux}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = err // surfaces as a fetch timeout / ctx cancel
		}
	}()

	fetcher := func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		want := stateFromURL(args.URL)
		fmt.Fprintf(os.Stderr, "\nAuthorize Tenant's access in your browser:\n\n  %s\n\n"+
			"(Atlassian shows TWO screens — approve access, then pick a site. Click through once each; don't re-click.)\n", args.URL)
		if open != nil {
			_ = open(args.URL) // best-effort auto-open; the printed URL is the fallback
		}
		res, err := awaitCode(ctx, results, want, codeDebounce)
		if err != nil {
			return nil, err
		}
		return &auth.AuthorizationResult{Code: res.code, State: res.state}, nil
	}
	return fetcher, func() { _ = srv.Close() }, nil
}

// awaitCode blocks for the first callback whose state matches want (the state
// the SDK embedded in the authorize URL), then debounces: it keeps the LATEST
// matching code arriving within `debounce` of the previous one and returns that.
// This redeems the code the server still considers live after a two-step /
// double-authorize, instead of a superseded earlier one. A blank want accepts
// any state (the SDK still validates it downstream).
func awaitCode(ctx context.Context, results <-chan authResult, want string, debounce time.Duration) (authResult, error) {
	matches := func(r authResult) bool { return want == "" || r.state == want }

	var chosen authResult
	for {
		select {
		case r := <-results:
			if matches(r) {
				chosen = r
				goto debounceLoop
			}
		case <-ctx.Done():
			return authResult{}, ctx.Err()
		}
	}

debounceLoop:
	timer := time.NewTimer(debounce)
	defer timer.Stop()
	for {
		select {
		case r := <-results:
			if matches(r) {
				chosen = r // last-wins: the freshest code supersedes earlier ones
			}
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(debounce)
		case <-timer.C:
			return chosen, nil
		case <-ctx.Done():
			return chosen, nil // return the freshest we have rather than abandoning
		}
	}
}

// stateFromURL extracts the OAuth `state` query parameter from an authorize URL.
func stateFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Query().Get("state")
}
