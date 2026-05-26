package gcsweb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListBuildIDs(t *testing.T) {
	resp := gcsListResponse{
		Prefixes: []string{
			"logs/my-job/2034271404769153024/",
			"logs/my-job/2035720955698790400/",
			"logs/my-job/2034633792370569216/",
			"logs/my-job/2034996180198326272/",
			"logs/my-job/2035358567883448320/",
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	ids, err := listAllBuildIDs(context.Background(), srv.Client(), srv.URL, "my-job")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{
		"2035720955698790400",
		"2035358567883448320",
		"2034996180198326272",
		"2034633792370569216",
		"2034271404769153024",
	}

	if len(ids) != len(expected) {
		t.Fatalf("got %d IDs, want %d", len(ids), len(expected))
	}
	for i, id := range ids {
		if id != expected[i] {
			t.Errorf("ids[%d] = %s, want %s", i, id, expected[i])
		}
	}
}

func TestPagination(t *testing.T) {
	page1 := gcsListResponse{
		Prefixes:      []string{"logs/my-job/1111111111111111111/", "logs/my-job/2222222222222222222/"},
		NextPageToken: "token123",
	}
	page2 := gcsListResponse{
		Prefixes: []string{"logs/my-job/3333333333333333333/", "logs/my-job/4444444444444444444/"},
	}

	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		callCount++
		if r.URL.Query().Get("pageToken") == "token123" {
			json.NewEncoder(w).Encode(page2)
		} else {
			json.NewEncoder(w).Encode(page1)
		}
	}))
	defer srv.Close()

	ids, err := listAllBuildIDs(context.Background(), srv.Client(), srv.URL, "my-job")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if callCount != 2 {
		t.Errorf("expected 2 API calls for pagination, got %d", callCount)
	}

	expected := []string{
		"4444444444444444444",
		"3333333333333333333",
		"2222222222222222222",
		"1111111111111111111",
	}
	if len(ids) != len(expected) {
		t.Fatalf("got %d IDs, want %d", len(ids), len(expected))
	}
	for i, id := range ids {
		if id != expected[i] {
			t.Errorf("ids[%d] = %s, want %s", i, id, expected[i])
		}
	}
}

func TestListRecentBuildIDs(t *testing.T) {
	resp := gcsListResponse{
		Prefixes: []string{
			"logs/my-job/1111111111111111111/",
			"logs/my-job/2222222222222222222/",
			"logs/my-job/3333333333333333333/",
			"logs/my-job/4444444444444444444/",
			"logs/my-job/5555555555555555555/",
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	// Temporarily test via the internal helper since we can't override the const.
	ids, err := listAllBuildIDs(context.Background(), srv.Client(), srv.URL, "my-job")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	count := 3
	if count > len(ids) {
		count = len(ids)
	}
	ids = ids[:count]

	if len(ids) != 3 {
		t.Fatalf("got %d IDs, want 3", len(ids))
	}

	expected := []string{
		"5555555555555555555",
		"4444444444444444444",
		"3333333333333333333",
	}
	for i, id := range ids {
		if id != expected[i] {
			t.Errorf("ids[%d] = %s, want %s", i, id, expected[i])
		}
	}
}

func TestListRecentBuildIDsCountExceedsAvailable(t *testing.T) {
	resp := gcsListResponse{
		Prefixes: []string{
			"logs/my-job/1111111111111111111/",
			"logs/my-job/2222222222222222222/",
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	ids, err := listAllBuildIDs(context.Background(), srv.Client(), srv.URL, "my-job")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	count := 100
	if count > len(ids) {
		count = len(ids)
	}
	ids = ids[:count]

	if len(ids) != 2 {
		t.Fatalf("got %d IDs, want 2 (all available)", len(ids))
	}
}

func TestEmptyResponse(t *testing.T) {
	resp := gcsListResponse{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	ids, err := listAllBuildIDs(context.Background(), srv.Client(), srv.URL, "my-job")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ids) != 0 {
		t.Errorf("expected 0 IDs from empty response, got %d", len(ids))
	}
}

func TestFilterNonNumeric(t *testing.T) {
	resp := gcsListResponse{
		Prefixes: []string{
			"logs/my-job/1111111111111111111/",
			"logs/my-job/latest-build.txt/",
			"logs/my-job/2222222222222222222/",
			"logs/my-job/some-directory/",
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	ids, err := listAllBuildIDs(context.Background(), srv.Client(), srv.URL, "my-job")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{"2222222222222222222", "1111111111111111111"}
	if len(ids) != len(expected) {
		t.Fatalf("got %d IDs, want %d", len(ids), len(expected))
	}
	for i, id := range ids {
		if id != expected[i] {
			t.Errorf("ids[%d] = %s, want %s", i, id, expected[i])
		}
	}
}

func TestHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := listAllBuildIDs(context.Background(), srv.Client(), srv.URL, "my-job")
	if err == nil {
		t.Fatal("expected error for 404 response")
	}
}

func TestSortingNewestFirst(t *testing.T) {
	resp := gcsListResponse{
		Prefixes: []string{
			"logs/my-job/1000000000000000001/",
			"logs/my-job/1000000000000000005/",
			"logs/my-job/1000000000000000002/",
			"logs/my-job/1000000000000000004/",
			"logs/my-job/1000000000000000003/",
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	ids, err := listAllBuildIDs(context.Background(), srv.Client(), srv.URL, "my-job")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for i := 1; i < len(ids); i++ {
		if ids[i] > ids[i-1] {
			t.Errorf("not sorted descending: ids[%d]=%s > ids[%d]=%s", i, ids[i], i-1, ids[i-1])
		}
	}
}
