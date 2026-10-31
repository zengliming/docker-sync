package registry

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestListTagsFollowsLinkPagination covers the paginated tags/list endpoint: the
// first page advertises rel="next" via Link, the second page ends the walk, and
// duplicates across pages are dropped.
func TestListTagsFollowsLinkPagination(t *testing.T) {
	var requested []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/library/app/tags/list" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		requested = append(requested, r.URL.RawQuery)
		switch r.URL.Query().Get("last") {
		case "":
			w.Header().Set("Link", fmt.Sprintf(`<%s/v2/library/app/tags/list?n=1000&last=b>; rel="next"`, "http://"+r.Host))
			fmt.Fprint(w, `{"name":"library/app","tags":["1.0.0","1.1.0","latest","1.1.0"]}`)
		case "b":
			fmt.Fprint(w, `{"name":"library/app","tags":["2.0.0","2.0.0-rc1"]}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	client := New(Options{Insecure: true})
	ref, err := Parse(strings.TrimPrefix(srv.URL, "http://") + "/library/app:latest")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tags, err := client.ListTags(t.Context(), ref)
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	want := []string{"1.0.0", "1.1.0", "latest", "2.0.0", "2.0.0-rc1"}
	if len(tags) != len(want) {
		t.Fatalf("tags = %v, want %v", tags, want)
	}
	for i := range want {
		if tags[i] != want[i] {
			t.Fatalf("tags = %v, want %v", tags, want)
		}
	}
	if len(requested) != 2 {
		t.Fatalf("requested pages = %v, want two", requested)
	}
}

// TestListTagsIgnoresForeignNextLink makes sure a Link header pointing at another
// host is not followed: that would leak the upstream token to a third party.
func TestListTagsIgnoresForeignNextLink(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Link", `<https://evil.example.com/v2/library/app/tags/list?last=x>; rel="next"`)
		fmt.Fprint(w, `{"name":"library/app","tags":["1.0.0"]}`)
	}))
	defer srv.Close()

	client := New(Options{Insecure: true})
	ref, err := Parse(strings.TrimPrefix(srv.URL, "http://") + "/library/app:latest")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tags, err := client.ListTags(t.Context(), ref)
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != 1 || tags[0] != "1.0.0" {
		t.Fatalf("tags = %v", tags)
	}
	if hits != 1 {
		t.Fatalf("server hits = %d, want 1", hits)
	}
}

// TestListTagsReportsMissingRepository maps 404 to a readable error.
func TestListTagsReportsMissingRepository(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := New(Options{Insecure: true})
	ref, err := Parse(strings.TrimPrefix(srv.URL, "http://") + "/library/gone:latest")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err = client.ListTags(context.Background(), ref)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want a not-found error", err)
	}
}
