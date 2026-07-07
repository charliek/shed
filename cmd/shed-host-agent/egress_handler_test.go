package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestEgressAuditEntry(t *testing.T) {
	d := egressDecision{
		Time: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		Shed: "web", Host: "evil.com", Port: 443, ResolvedIP: "1.2.3.4",
		Protocol: "https", Verdict: "deny", Reason: "default-deny",
	}
	e := egressAuditEntry("srv", d)
	if e.Server != "srv" || e.Shed != "web" || e.Namespace != "egress" {
		t.Errorf("server/shed/ns = %q/%q/%q", e.Server, e.Shed, e.Namespace)
	}
	if e.Operation != "https" || e.Result != "deny" || e.Reason != "default-deny" {
		t.Errorf("op/result/reason = %q/%q/%q", e.Operation, e.Result, e.Reason)
	}
	if e.Detail != "evil.com:443 (1.2.3.4)" {
		t.Errorf("detail = %q, want evil.com:443 (1.2.3.4)", e.Detail)
	}
	if e.Timestamp != "2020-01-01T00:00:00Z" {
		t.Errorf("timestamp = %q", e.Timestamp)
	}
}

func TestEgressAuditEntry_NoResolvedIP(t *testing.T) {
	e := egressAuditEntry("srv", egressDecision{Shed: "web", Host: "x.com", Port: 80, Protocol: "http", Verdict: "allow"})
	if e.Detail != "x.com:80" {
		t.Errorf("detail = %q, want x.com:80 (no resolved IP)", e.Detail)
	}
}

func TestEgressSubscriber_Stream(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		// Emit decisions repeatedly so the test can't lose a race with setup.
		for {
			select {
			case <-r.Context().Done():
				return
			default:
				fmt.Fprint(w, "data: {\"ts\":\"2020-01-01T00:00:00Z\",\"shed\":\"web\",\"host\":\"evil.com\",\"port\":443,\"protocol\":\"https\",\"verdict\":\"deny\",\"reason\":\"default-deny\"}\n\n")
				if fl != nil {
					fl.Flush()
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
	}))
	defer ts.Close()

	audit := NewAuditLogger(LogConfig{Enabled: false}, discardLogger())
	ch, unsub := audit.Subscribe(8)
	defer unsub()

	sub := NewEgressSubscriber(ServerTarget{Name: "srv", URL: ts.URL, Token: "tok"}, nil, audit, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sub.Run(ctx)

	select {
	case e := <-ch:
		if e.Namespace != "egress" || e.Shed != "web" || e.Result != "deny" {
			t.Errorf("audit entry = %+v", e)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("no egress audit entry received over the stream")
	}
}

// fakeTokenSource is a tokenSource returning a fixed token and counting Invalidate.
type fakeTokenSource struct {
	token       string
	err         error
	invalidated int
}

func (f *fakeTokenSource) Token() (string, error) { return f.token, f.err }
func (f *fakeTokenSource) Invalidate()            { f.invalidated++ }

func TestEgressSubscriber_SendsControlToken(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK) // empty body → stream returns promptly
	}))
	defer ts.Close()

	audit := NewAuditLogger(LogConfig{Enabled: false}, discardLogger())
	sub := NewEgressSubscriber(
		ServerTarget{Name: "srv", URL: ts.URL, Token: "creds-tok"},
		&fakeTokenSource{token: "ctl-tok"}, audit, discardLogger())
	_ = sub.stream(context.Background())

	if gotAuth != "Bearer ctl-tok" {
		t.Errorf("Authorization = %q, want the control token (not the credentials token)", gotAuth)
	}
}

func TestEgressSubscriber_401Invalidates(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()

	audit := NewAuditLogger(LogConfig{Enabled: false}, discardLogger())
	fake := &fakeTokenSource{token: "ctl-tok"}
	sub := NewEgressSubscriber(ServerTarget{Name: "srv", URL: ts.URL}, fake, audit, discardLogger())
	if err := sub.stream(context.Background()); err == nil {
		t.Fatal("expected an error on 401")
	}
	if fake.invalidated != 1 {
		t.Errorf("Invalidate called %d times, want 1 (so the next reconnect re-mints)", fake.invalidated)
	}
}

func TestEgressSubscriber_DisabledReturnsUnavailable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotImplemented) // egress disabled on the server
	}))
	defer ts.Close()

	audit := NewAuditLogger(LogConfig{Enabled: false}, discardLogger())
	sub := NewEgressSubscriber(ServerTarget{Name: "srv", URL: ts.URL}, nil, audit, discardLogger())
	if err := sub.stream(context.Background()); !errors.Is(err, errEgressUnavailable) {
		t.Errorf("stream err = %v, want errEgressUnavailable", err)
	}
}

func TestEgressHTTPClient_PinOnPlainURLFailsClosed(t *testing.T) {
	c := egressHTTPClient("http://localhost:8080", "sha256:deadbeef")
	req, _ := http.NewRequest(http.MethodGet, "http://localhost:8080/api/egress/stream", nil)
	if _, err := c.Transport.RoundTrip(req); err == nil {
		t.Error("expected fail-closed error when a TLS pin is set on a non-https URL")
	}
}
