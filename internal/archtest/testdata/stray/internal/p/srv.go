package p

import (
	"net/http"

	"tenant/internal/httplimits"
)

func servers() {
	ok := &http.Server{}
	httplimits.Configure(ok)
	_ = &http.Server{} // not configured
}
