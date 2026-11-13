package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type OpenAI struct {
	base  string // например https://api.openai.com/v1
	key   string
	model string
	hc    *http.Client
}

func NewOpenAI(base, key, model string, timeout time.Duration) *OpenAI {
	return &OpenAI{base: strings.TrimRight(base, "/"), key: key, model: model, hc: &http.Client{Timeout: timeout}}
}

// OpenAI-формат эмбеддингов: data — массив объектов с полем embedding.
type oaEmbedResp struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
}

func (o *OpenAI) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": o.model, "input": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.base+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.key)

	resp, err := o.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return nil, fmt.Errorf("embed: HTTP %d: %s", resp.StatusCode, string(b))
	}

	var er oaEmbedResp
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, err
	}
	// Восстанавливаем порядок: API может вернуть не по порядку, сортируем по index.
	out := make([][]float32, len(texts))
	for _, d := range er.Data {
		if d.Index >= 0 && d.Index < len(out) {
			out[d.Index] = d.Embedding
		}
	}
	return out, nil
}

func (o *OpenAI) ChatStream(ctx context.Context, messages []Message) (<-chan string, error) {
	body, _ := json.Marshal(map[string]any{
		"model": o.model, "messages": messages, "stream": true,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.key)

	resp, err := o.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("chat: HTTP %d: %s", resp.StatusCode, string(b))
	}

	tokens := make(chan string)
	go func() {
		defer func() { _ = resp.Body.Close() }()
		defer close(tokens)

		// OpenAI-стриминг — это SSE: строки вида "data: {json}", поток
		// завершается строкой "data: [DONE]".
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				return
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if json.Unmarshal([]byte(payload), &chunk) != nil {
				continue
			}
			if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
				select {
				case tokens <- chunk.Choices[0].Delta.Content:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return tokens, nil
}
