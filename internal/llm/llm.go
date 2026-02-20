package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type ollamaRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

type ollamaResponse struct {
	Response string `json:"response"`
}

// InferDate asks an LLM to extract a document date from the given text.
// Returns a date string in YYYY-MM-DD format, or empty string if no date found.
func InferDate(ollamaURL, model, text string) (string, error) {
	if len(text) > 2000 {
		text = text[:2000]
	}

	prompt := fmt.Sprintf(`Extract the document date from the following text. The document date is the date the document was created, issued, or refers to (e.g. invoice date, letter date, statement date). Return ONLY the date in YYYY-MM-DD format. If no date can be determined, return "NONE".

Text:
%s

Date:`, text)

	body, err := json.Marshal(ollamaRequest{
		Model:  model,
		Prompt: prompt,
		Stream: false,
	})
	if err != nil {
		return "", err
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Post(ollamaURL+"/api/generate", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("ollama request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}

	var result ollamaResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decoding ollama response: %w", err)
	}

	dateStr := strings.TrimSpace(result.Response)
	if dateStr == "" || strings.EqualFold(dateStr, "NONE") {
		return "", nil
	}

	// Validate it parses as a date
	if _, err := time.Parse("2006-01-02", dateStr); err != nil {
		return "", nil
	}

	return dateStr, nil
}
