package atlassian

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

// REVSEC-6: any web page can make the browser open the loopback callback. A
// hit without the sign-in's state, error or code, is ignored and the
// sign-in keeps waiting; the real one still ends it, and a second hit after
// it never blocks.
func TestAwaitCallback_IgnoresForeignState(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type res struct {
		code string
		err  error
	}
	out := make(chan res, 1)
	go func() {
		code, err := awaitCallbackCode(ctx, addr, "the-real-state", "https://auth.example/authorize", func(string) error { return nil })
		out <- res{code, err}
	}()
	hit := func(q string) int {
		for i := 0; i < 50; i++ {
			resp, err := http.Get("http://" + addr + "/callback?" + q)
			if err == nil {
				resp.Body.Close()
				return resp.StatusCode
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("the callback never listened")
		return 0
	}
	for _, q := range []string{"state=forged&code=x", "error=access_denied", "state=forged&error=access_denied", "code=x"} {
		if code := hit(q); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", q, code)
		}
	}
	select {
	case r := <-out:
		t.Fatalf("a forged callback ended the sign-in: %+v", r)
	case <-time.After(200 * time.Millisecond):
	}
	hit("state=the-real-state&code=the-code")
	if r := <-out; r.err != nil || r.code != "the-code" {
		t.Fatalf("the real callback: %+v", r)
	}
}
