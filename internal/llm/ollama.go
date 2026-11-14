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

type Ollama struct {
	base       string       // например http://localhost:11434
	embedModel string       // модель для векторов (nomic-embed-text:latest)
	chatModel  string       // модель для ответов (qwen2.5:7b)
	hc         *http.Client // долгий таймаут: генерация 7B-модели может занять минуты
}

func NewOllama(base, embedModel, chatModel string, timeout time.Duration) *Ollama {
	return &Ollama{
		base:       strings.TrimRight(base, "/"),
		embedModel: embedModel,
		chatModel:  chatModel,
		hc:         &http.Client{Timeout: timeout},
	}
}

// --- Эмбеддинги ---

// embedRequest/embedResponse — контракт Ollama /api/embed:
// на вход пачка текстов, на выход — пачка векторов того же размера.
type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

func (o *Ollama) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(embedRequest{Model: o.embedModel, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("marshal embed request: %w", err)
	}
	// Запрос тоже с контекстом: если приложение выключается,
	// недоделанный HTTP-вызов прервётся.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		o.base+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Читаем тело ошибки целиком — Ollama кладёт туда осмысленный текст.
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return nil, fmt.Errorf("embed: HTTP %d: %s", resp.StatusCode, string(b))
	}

	var er embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, fmt.Errorf("decode embed response: %w", err)
	}
	if len(er.Embeddings) != len(texts) {
		// Соглашение API: сколько текстов просили — столько векторов ждём.
		// Нарушение — сигнал, что модель/версия не та: падаем честно.
		return nil, fmt.Errorf("embed: got %d vectors for %d texts", len(er.Embeddings), len(texts))
	}
	return er.Embeddings, nil
}

// --- Чат со стримингом ---

// chatRequest — на вход диалог; stream:true включает NDJSON-поток.
type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
}

// chatChunk — одна строка потока. Приходят только инкременты:
// message.content — это ОЧЕРЕДНОЙ кусочек, а не весь ответ.
type chatChunk struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	Done bool `json:"done"`
}

func (o *Ollama) ChatStream(ctx context.Context, messages []Message) (<-chan string, error) {
	body, err := json.Marshal(chatRequest{Model: o.chatModel, Messages: messages, Stream: true})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		o.base+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("chat: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("chat: HTTP %d: %s", resp.StatusCode, string(b))
	}

	// Возвращаем канал токенов. ЗАЧЕМ канал, а не writer: разделение ролей.
	// Эта горутина только читает сеть; потребитель (SSE-обработчик) —
	// только рассылает в браузер. Ничего не знает друг о друге —
	// классический producer/consumer на канале.
	tokens := make(chan string)

	go func() {
		defer func() { _ = resp.Body.Close() }()
		defer close(tokens) // потребитель увидит закрытие — «ответ закончился»

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // строки бывают длинные
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var chunk chatChunk
			if err := json.Unmarshal(line, &chunk); err != nil {
				continue // мусорная строка — пропускаем, не роняем поток
			}
			if chunk.Message.Content != "" {
				select {
				case tokens <- chunk.Message.Content:
				case <-ctx.Done():
					return // пользователь нажал «стоп» — прерываем чтение
				}
			}
			if chunk.Done {
				return
			}
		}
	}()
	return tokens, nil
}
