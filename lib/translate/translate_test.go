package translate

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTranslateEndToEndWithFakeAPI(t *testing.T) {
	var gotPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Errorf("expected Authorization header, got %q", auth)
		}
		var req chatRequest
		json.NewDecoder(r.Body).Decode(&req)
		gotPrompt = req.Messages[0].Content
		resp := chatResponse{}
		resp.Choices = []struct {
			Message chatMessage `json:"message"`
		}{{Message: chatMessage{Content: `{"province":"马佐夫舍","city":"华沙"}`}}}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	dbPath := filepath.Join(os.TempDir(), "translate_test.db")
	os.Remove(dbPath)
	defer os.Remove(dbPath)

	if err := Init(srv.URL, "test-key", "test-model", dbPath); err != nil {
		t.Fatalf("Init error: %v", err)
	}
	if !enabled {
		t.Fatal("expected enabled=true when url+key set")
	}

	if _, _, ok := Lookup("Poland", "Mazovia", "Warsaw"); ok {
		t.Fatal("expected no cached translation before Enqueue")
	}

	Enqueue("Poland", "Mazovia", "Warsaw")

	deadline := time.Now().Add(3 * time.Second)
	var provinceZH, cityZH string
	var ok bool
	for time.Now().Before(deadline) {
		provinceZH, cityZH, ok = Lookup("Poland", "Mazovia", "Warsaw")
		if ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ok {
		t.Fatal("translation never appeared in cache")
	}
	if provinceZH != "马佐夫舍" || cityZH != "华沙" {
		t.Fatalf("unexpected translation: province=%q city=%q", provinceZH, cityZH)
	}
	if gotPrompt == "" {
		t.Fatal("expected a non-empty prompt to have been sent")
	}
	t.Logf("prompt sent: %s", gotPrompt)

	// second Enqueue for the same place should not re-call the API (already cached)
	Enqueue("Poland", "Mazovia", "Warsaw")
}

func TestTranslateDisabledByDefault(t *testing.T) {
	apiURL, apiKey, model, enabled = "", "", "", false
	Enqueue("Germany", "Berlin", "Berlin") // must not panic or block
	if _, _, ok := Lookup("Germany", "Berlin", "Berlin"); ok {
		t.Fatal("expected no result when disabled")
	}
}
