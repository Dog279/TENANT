package s

import "net/http"

var _ = http.Server{} // outside cmd and internal: not checked
