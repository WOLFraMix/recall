// Package config — настройки из переменных окружения.
package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	// Listen — адрес HTTP-сервера: REST, SSE и встроенный веб-интерфейс.
	Listen string

	// DBPath — файл SQLite с документами, чанками и векторами.
	DBPath string

	// LLMURL — базовый URL провайдера моделей.
	// Для Ollama по умолчанию это http://localhost:11434.
	LLMURL string

	// EmbedModel — модель для векторов смысла, ChatModel — для ответов.
	// Разные модели потому, что у них разные задачи: эмбеддинг-модель
	// не генерирует текст, а чат-модель не оптимизирована под векторы.
	EmbedModel string
	ChatModel  string

	// ChunkSize — размер чанка в символах. ~800 символов ≈ 200 токенов:
	// достаточно контекста, чтобы чанк был осмысленным сам по себе.
	ChunkSize int

	// ChunkOverlap — перекрытие соседних чанков. Нужно потому, что мысль
	// может пересечь границу: перекрытие не даёт «разорвать» её.
	ChunkOverlap int

	// TopK — сколько ближайших чанков отдавать модели в контекст.
	TopK int

	// IngestWorkers — сколько документов обрабатываем параллельно.
	IngestWorkers int

	// QueueSize — ёмкость очереди загрузок (backpressure).
	QueueSize int

	// HTTPTimeout — таймаут запросов к LLM (генерация бывает долгой).
	HTTPTimeout time.Duration
}

func Load() Config {
	return Config{
		Listen:        getEnv("RC_LISTEN", ":8080"),
		DBPath:        getEnv("RC_DB_PATH", "./data/recall.db"),
		LLMURL:        getEnv("RC_LLM_URL", "http://localhost:11434"),
		EmbedModel:    getEnv("RC_EMBED_MODEL", "nomic-embed-text"),
		ChatModel:     getEnv("RC_CHAT_MODEL", "qwen2.5:7b"),
		ChunkSize:     getInt("RC_CHUNK_SIZE", 800),
		ChunkOverlap:  getInt("RC_CHUNK_OVERLAP", 100),
		TopK:          getInt("RC_TOP_K", 5),
		IngestWorkers: getInt("RC_INGEST_WORKERS", 4),
		QueueSize:     getInt("RC_QUEUE_SIZE", 256),
		HTTPTimeout:   getDuration("RC_HTTP_TIMEOUT", 5*time.Minute),
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

func getDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
