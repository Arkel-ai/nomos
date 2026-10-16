package services

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	u "github.com/Arkel-ai/nomos/api/utils"
)

func TestParsePageBlocks(t *testing.T) {
	cases := []struct {
		name string
		text string
		n    int
		want map[int]string
	}{
		{"all pages, blank in the middle", "<page n=\"1\">\nAlpha\n</page>\n<page n=\"2\"></page>\n<page n=\"3\">Gamma</page>", 3,
			map[int]string{1: "Alpha", 2: "", 3: "Gamma"}},
		{"cut-off answer loses only the unclosed page", "<page n=\"1\">Alpha</page><page n=\"2\">Bet", 2,
			map[int]string{1: "Alpha"}},
		{"out of range and repeated blocks are ignored", "<page n=\"1\">A</page><page n=\"1\">B</page><page n=\"4\">D</page>", 2,
			map[int]string{1: "A"}},
		{"an untagged one-page answer is taken whole", "  Alpha\nBeta  ", 1,
			map[int]string{1: "Alpha\nBeta"}},
		{"an untagged answer is not guessed across pages", "Alpha", 2,
			map[int]string{}},
		{"an empty answer reads nothing", "", 1,
			map[int]string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePageBlocks(tc.text, tc.n)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d pages %v, want %v", len(got), got, tc.want)
			}
			for k, v := range tc.want {
				if g, ok := got[k]; !ok || g != v {
					t.Fatalf("page %d = %q (present=%t), want %q", k, g, ok, v)
				}
			}
		})
	}
}

// TestVisionReadPDFRequest pins what a range read sends — the PDF as a file
// part, low reasoning, an output budget sized to the range — and what an empty
// answer becomes: ErrEmptyCompletion, reported as "empty", never a refusal.
func TestVisionReadPDFRequest(t *testing.T) {
	var answer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model     string         `json:"model"`
			MaxTokens int            `json:"max_tokens"`
			Reasoning map[string]any `json:"reasoning"`
			Messages  []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Model != "vision-model" || req.MaxTokens != 3*ocrMaxTokensPerPage || req.Reasoning["effort"] != "low" {
			t.Errorf("model=%s max_tokens=%d reasoning=%v", req.Model, req.MaxTokens, req.Reasoning)
		}
		var system string
		if len(req.Messages) != 2 || json.Unmarshal(req.Messages[0].Content, &system) != nil ||
			!strings.Contains(system, "<page n=") {
			t.Errorf("the system prompt must carry the page-block contract")
		}
		user := string(req.Messages[1].Content)
		if !strings.Contains(user, `"type":"file"`) || !strings.Contains(user, "data:application/pdf;base64,") ||
			!strings.Contains(user, `pages=\"3\"`) {
			t.Errorf("the user turn must carry the PDF and its page count: %.200s", user)
		}
		w.Header().Set("Content-Type", "application/json")
		resp, _ := json.Marshal(map[string]any{
			"id": "r1", "model": "vision-model",
			"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": answer}}},
			"usage":   map[string]any{"prompt_tokens": 4000, "completion_tokens": 900},
		})
		w.Write(resp)
	}))
	defer srv.Close()
	defer u.UseGateway(srv.URL, "test")()

	opts := u.ChatOpts{Model: "vision-model", TenantID: ingestTestTenant}

	answer = "<page n=\"1\">Alpha</page><page n=\"2\"></page><page n=\"3\">Gamma</page>"
	read, _, err := visionReadPDF(context.Background(), opts, []byte("%PDF-1.4"), 3)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(read) != 3 || read[2] != "" || read[3] != "Gamma" {
		t.Fatalf("read = %v", read)
	}

	answer = ""
	_, _, err = visionReadPDF(context.Background(), opts, []byte("%PDF-1.4"), 3)
	if !errors.Is(err, u.ErrEmptyCompletion) || visionFailure(err) != "empty" {
		t.Fatalf("an empty answer must be ErrEmptyCompletion/\"empty\", got %v (%s)", err, visionFailure(err))
	}
}
