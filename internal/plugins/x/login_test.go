package x

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type fakeTokenEndpoint struct{ form url.Values }

func (f *fakeTokenEndpoint) Do(r *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(r.Body)
	f.form, _ = url.ParseQuery(string(b))
	return &http.Response{StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(`{"access_token":"at","refresh_token":"rt","expires_in":60}`))}, nil
}

func freeLoopback(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// A callback without the sign-in's state is refused and changes nothing,
// whether it carries a code or an error: any web page can make the browser
// open the address (REVSEC-6, TEN-440). The real callback then completes the
// sign-in, and a busy port fails at once instead of hanging.
func TestLoginIgnoresForgedCallbacks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	redirect := "http://" + freeLoopback(t) + "/callback"
	tok := &fakeTokenEndpoint{}
	authURL := make(chan string, 1)
	type out struct {
		st  *tokenStore
		err error
	}
	done := make(chan out, 1)
	go func() {
		st, err := loginPKCE(ctx, tok, "cid", redirect, scopeRead, func(u string) { authURL <- u })
		done <- out{st, err}
	}()
	au, err := url.Parse(<-authURL)
	if err != nil {
		t.Fatal(err)
	}
	state := au.Query().Get("state")
	get := func(q string) (int, string) {
		t.Helper()
		resp, err := http.Get(redirect + "?" + q)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for _, forged := range []string{"state=forged&code=evil", "state=forged&error=access_denied", "code=evil"} {
		if code, _ := get(forged); code != http.StatusBadRequest {
			t.Fatalf("forged callback %q answered %d", forged, code)
		}
	}
	select {
	case o := <-done:
		t.Fatalf("a forged callback ended the sign-in: %+v", o)
	case <-time.After(100 * time.Millisecond):
	}
	if code, body := get(fmt.Sprintf("state=%s&code=good", url.QueryEscape(state))); code != http.StatusOK || !strings.Contains(body, "complete") {
		t.Fatalf("real callback = %d %q", code, body)
	}
	o := <-done
	if o.err != nil || o.st == nil || o.st.Access != "at" || tok.form.Get("code") != "good" {
		t.Fatalf("sign-in = %+v, %v; exchanged %v", o.st, o.err, tok.form)
	}

	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	_, err = loginPKCE(ctx, tok, "cid", "http://"+busy.Addr().String()+"/callback", scopeRead, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("busy port: %v", err)
	}
}
