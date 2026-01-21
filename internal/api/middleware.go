package api

import (
	"net/http"
)

// ContentTypeJSON is middleware that sets the Content-Type header to application/json
// for all responses.
func ContentTypeJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		next.ServeHTTP(w, r)
	})
}
