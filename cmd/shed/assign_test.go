package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
)

// TestAssignShedClient: the client posts {"user":...} to the shed's assign
// route and returns the shed; a refusal carries the server's code and
// message.
func TestAssignShedClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if r.Method != http.MethodPost || len(req) != 1 {
			http.Error(w, "bad request "+r.Method, http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/api/sheds/legacy/assign":
			_ = json.NewEncoder(w).Encode(config.Shed{Name: "legacy", User: req["user"].(string)})
		case "/api/sheds/hosty/assign":
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(config.NewAPIError(config.ErrShedHostResources, "shed carries host resources: recreate it without them"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	c := NewAPIClient(u.Hostname(), port, time.Second)

	shed, err := c.AssignShed("legacy", "bob")
	if err != nil || shed.Name != "legacy" || shed.User != "bob" {
		t.Fatalf("AssignShed = %+v, %v", shed, err)
	}
	_, err = c.AssignShed("hosty", "bob")
	if err == nil || !strings.Contains(err.Error(), config.ErrShedHostResources) || !strings.Contains(err.Error(), "recreate it without them") {
		t.Fatalf("refusal = %v", err)
	}
}
