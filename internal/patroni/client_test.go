package patroni

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClusterParsesMembers(t *testing.T) {
	payload := `{"members":[
		{"name":"pg-0","role":"leader","state":"running","lag":null},
		{"name":"pg-1","role":"replica","state":"running","lag":0},
		{"name":"pg-2","role":"replica","state":"streaming","lag":1024}
	]}`
	var gotUser, gotPass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cluster" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		gotUser, gotPass, _ = r.BasicAuth()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	c := New(srv.URL, "patroni", "secret", time.Second)
	cluster, err := c.Cluster(context.Background())
	if err != nil {
		t.Fatalf("Cluster() error: %v", err)
	}
	if gotUser != "patroni" || gotPass != "secret" {
		t.Fatalf("unexpected basic auth: %q/%q", gotUser, gotPass)
	}
	if len(cluster.Members) != 3 {
		t.Fatalf("expected 3 members, got %d", len(cluster.Members))
	}
	leader, ok := cluster.Leader()
	if !ok || leader != "pg-0" {
		t.Fatalf("expected leader pg-0, got %q ok=%v", leader, ok)
	}
	if cluster.Members[2].Lag == nil || *cluster.Members[2].Lag != 1024 {
		t.Fatalf("expected lag 1024 for pg-2")
	}
}

func TestClusterNoLeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"members":[{"name":"pg-0","role":"replica","state":"running"}]}`))
	}))
	defer srv.Close()

	cluster, err := New(srv.URL, "", "", time.Second).Cluster(context.Background())
	if err != nil {
		t.Fatalf("Cluster() error: %v", err)
	}
	if _, ok := cluster.Leader(); ok {
		t.Fatal("expected no leader")
	}
}

func TestClusterNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "", "", time.Second).Cluster(context.Background()); err == nil {
		t.Fatal("expected error on non-200 response")
	}
}

func TestClusterBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "", "", time.Second).Cluster(context.Background()); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestClusterUnreachable(t *testing.T) {
	// Port 1 on localhost is never a Patroni endpoint.
	if _, err := New("http://127.0.0.1:1", "", "", 200*time.Millisecond).Cluster(context.Background()); err == nil {
		t.Fatal("expected connection error")
	}
}
