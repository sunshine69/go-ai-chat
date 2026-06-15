// mcp_streamable_test.go
package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestStreamableHTTP(t *testing.T) {
	url := "http://localhost:8080/mcp"

	fmt.Printf("Connecting to %s\n", url)
	c, err := ConnectStreamableHTTP(url)
	if err != nil {
		t.Fatalf("❌ Failed: %v", err)
	}
	fmt.Printf("✅ Connected, %d tools:\n", len(c.Tools()))
	for _, tool := range c.Tools() {
		fmt.Printf("  • %s\n", tool.Name)
	}
}

func TestMental(t *testing.T) {
	if aiThoughtBecomeMentalPtn.MatchString(`Let me go!!!!`) {
		println("Caught ")
	}
}
func TestGetSentencesAtPosition(t *testing.T) {
	input := "First sentence. Second sentence. Third sentence. Fourth sentence."

	tests := []struct {
		name      string
		position  string
		wantCount int
		wantError bool
		checkFunc func(t *testing.T, result []string)
	}{
		{
			name:      "single index zero",
			position:  "0",
			wantCount: 1,
			wantError: false,
			checkFunc: func(t *testing.T, result []string) {
				if !strings.Contains(result[0], "First sentence") {
					t.Errorf("expected first sentence, got %q", result[0])
				}
			},
		},
		{
			name:      "single index middle",
			position:  "2",
			wantCount: 1,
			wantError: false,
			checkFunc: func(t *testing.T, result []string) {
				if !strings.Contains(result[0], "Third sentence") {
					t.Errorf("expected third sentence, got %q", result[0])
				}
			},
		},
		{
			name:      "single index last",
			position:  "3",
			wantCount: 1,
			wantError: false,
			checkFunc: func(t *testing.T, result []string) {
				if !strings.Contains(result[0], "Fourth sentence") {
					t.Errorf("expected fourth sentence, got %q", result[0])
				}
			},
		},
		{
			name:      "multiple indices",
			position:  "1,3",
			wantCount: 2,
			wantError: false,
			checkFunc: func(t *testing.T, result []string) {
				if !strings.Contains(result[0], "Second sentence") {
					t.Errorf("expected second sentence at index 0, got %q", result[0])
				}
				if !strings.Contains(result[1], "Fourth sentence") {
					t.Errorf("expected fourth sentence at index 1, got %q", result[1])
				}
			},
		},
		{
			name:      "multiple indices unsorted",
			position:  "2,0",
			wantCount: 2,
			wantError: false,
			checkFunc: func(t *testing.T, result []string) {
				if !strings.Contains(result[0], "Third sentence") {
					t.Errorf("expected third sentence at index 0, got %q", result[0])
				}
				if !strings.Contains(result[1], "First sentence") {
					t.Errorf("expected first sentence at index 1, got %q", result[1])
				}
			},
		},
		{
			name:      "negative index -1 (last sentence)",
			position:  "-1",
			wantCount: 1,
			wantError: false,
			checkFunc: func(t *testing.T, result []string) {
				if !strings.Contains(result[0], "Fourth sentence") {
					t.Errorf("expected last sentence (index -1), got %q", result[0])
				}
			},
		},
		{
			name:      "negative index -2 (second-to-last)",
			position:  "-2",
			wantCount: 1,
			wantError: false,
			checkFunc: func(t *testing.T, result []string) {
				if !strings.Contains(result[0], "Third sentence") {
					t.Errorf("expected second-to-last sentence (index -2), got %q", result[0])
				}
			},
		},
		{
			name:      "negative indices comma-separated -1,-2",
			position:  "-1,-2",
			wantCount: 2,
			wantError: false,
			checkFunc: func(t *testing.T, result []string) {
				if !strings.Contains(result[0], "Fourth sentence") {
					t.Errorf("expected last sentence at index 0, got %q", result[0])
				}
				if !strings.Contains(result[1], "Third sentence") {
					t.Errorf("expected second-to-last sentence at index 1, got %q", result[1])
				}
			},
		},
		{
			name:      "random selection rand",
			position:  "rand",
			wantCount: 1,
			wantError: false,
			checkFunc: func(t *testing.T, result []string) {
				if len(result[0]) == 0 {
					t.Error("expected non-empty sentence")
				}
				found := false
				for _, s := range []string{"First", "Second", "Third", "Fourth"} {
					if strings.Contains(result[0], s) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("random sentence doesn't match any expected sentence: %q", result[0])
				}
			},
		},
		{
			name:      "out of range high",
			position:  "5",
			wantCount: 0,
			wantError: true,
		},
		{
			name:      "out of range negative",
			position:  "-5",
			wantCount: 0,
			wantError: true,
		},
		{
			name:      "invalid position string",
			position:  "abc",
			wantCount: 0,
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := getSentencesAtPosition(input, tt.position)

			if (err != nil) != tt.wantError {
				t.Errorf("getSentencesAtPosition() error = %v, wantError %v", err, tt.wantError)
				return
			}

			if !tt.wantError && len(result) != tt.wantCount {
				t.Errorf("expected %d sentences, got %d: %v", tt.wantCount, len(result), result)
				return
			}

			if tt.checkFunc != nil && !tt.wantError {
				tt.checkFunc(t, result)
			}
		})
	}
}

func TestGetSentencesAtPosition_EmptyText(t *testing.T) {
	result, err := getSentencesAtPosition("", "0")
	if err == nil {
		t.Error("expected error for empty text, got nil")
	}
	if len(result) > 0 {
		t.Errorf("expected no sentences for empty text, got %v", result)
	}
}

func TestGetSentencesAtPosition_NoPunctuation(t *testing.T) {
	result, err := getSentencesAtPosition("No punctuation here", "0")
	if err == nil {
		t.Errorf("expected error for text without sentence-ending punctuation, got nil")
	}
	if len(result) > 0 {
		t.Errorf("expected no sentences for text without explicit punctuation, got %v", result)
	}
}

func TestGetSentencesAtPosition_MultipleCommas(t *testing.T) {
	input := "A. B. C."
	result, err := getSentencesAtPosition(input, "0,1,2")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if len(result) != 3 {
		t.Errorf("expected 3 sentences, got %d", len(result))
	}
}
