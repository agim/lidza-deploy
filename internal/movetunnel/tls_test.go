package movetunnel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The tunnel takes over the connection after an HTTP/1.1 upgrade. The
// control panel reaches agents through a transport that also speaks HTTP/2,
// and a web server in front of an agent accepts HTTP/2 when it is offered.
// The tunnel's client must not offer it: against a real TLS server with
// HTTP/2 enabled, the upgrade has to go through.
func TestDialUpgradesThroughATLSServerThatAlsoSpeaksHTTP2(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/plain" {
			w.Header().Set("X-Proto", r.Proto)
			return
		}
		if !Requested(r) {
			http.Error(w, "not an upgrade over "+r.Proto, http.StatusBadRequest)
			return
		}
		conn, err := Upgrade(w, r)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn) // echo
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	// The shared transport has been used, as in a running control panel:
	// its ordinary requests are HTTP/2.
	base := server.Client().Transport
	res, err := (&http.Client{Transport: base}).Get(server.URL + "/plain")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.Header.Get("X-Proto") != "HTTP/2.0" {
		t.Fatalf("the ordinary request was expected over HTTP/2, got %s", res.Header.Get("X-Proto"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := Dial(ctx, Client(base), server.URL+"/tunnel", "bearer", "token")
	if err != nil {
		t.Fatalf("the tunnel could not be opened: %v", err)
	}
	defer stream.Close()
	if _, err := stream.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(stream, got); err != nil || string(got) != "ping" {
		t.Fatalf("the stream does not carry data: %q %v", got, err)
	}
	// And the shared transport still speaks HTTP/2 afterwards: the tunnel's
	// client is a copy.
	res, err = (&http.Client{Transport: base}).Get(server.URL + "/plain")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.Header.Get("X-Proto") != "HTTP/2.0" {
		t.Fatalf("the shared transport lost HTTP/2: %s", res.Header.Get("X-Proto"))
	}
}
