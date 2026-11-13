// Package llm — слой доступа к языковым моделям.
// Весь остальной код зависит только от интерфейса Provider, а не от
// конкретного сервиса: поменять Ollama на OpenAI — одна env-переменная.
package llm

import "context"

// Message — сообщение в диалоге с моделью.
// Role: "system" (инструкции), "user" (человек), "assistant" (ответ модели).
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Provider — два умения, которые нужны RAG-системе.
type Provider interface {
	// Embed превращает тексты в векторы. Батчевый (срез на входе):
	// один HTTP-запрос на пачку текстов эффективнее сотни одиночных —
	// это первое место, где рождается «высоконагруженность».
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	// ChatStream начинает диалог и возвращает канал токенов: модель
	// генерирует ответ по частям, и мы отдаём их пользователю по мере
	// появления (стриминг). Канал закрывается по завершении ответа
	// или отмене ctx — тогда генерация прерывается физически.
	ChatStream(ctx context.Context, messages []Message) (<-chan string, error)
}
