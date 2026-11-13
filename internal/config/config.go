// Package config — настройки из переменных окружения и файла .env.
//
// Порядок приоритета (от высшего к низшему):
//  1. переменная окружения, установленная в системе (или в шелле);
//  2. значение из файла .env;
//  3. встроенный дефолт.
//
// Так можно переопределять одну настройку для отладки, не трогая .env:
//
//	$env:RC_CHAT_MODEL = "qwen2.5:7b"; go run ./cmd/recall
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	// Внимание: смена EmbedModel требует реиндекса — размерность векторов
	// у моделей разная (nomic: 768, bge-m3: 1024), старые векторы
	// в пространстве новой модели бессмысленны.
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

// Load собирает конфигурацию. Сначала загружает .env в окружение,
// затем читает переменные.
func Load() (Config, error) {
	if err := loadDotEnv(".env"); err != nil {
		return Config{}, err
	}

	return Config{
		Listen:        getEnv("RC_LISTEN", ":8080"),
		DBPath:        getEnv("RC_DB_PATH", "./data/recall.db"),
		LLMURL:        getEnv("RC_LLM_URL", "http://localhost:11434"),
		EmbedModel:    getEnv("RC_EMBED_MODEL", "bge-m3"),
		ChatModel:     getEnv("RC_CHAT_MODEL", "qwen2.5-coder:7b"),
		ChunkSize:     getInt("RC_CHUNK_SIZE", 800),
		ChunkOverlap:  getInt("RC_CHUNK_OVERLAP", 100),
		TopK:          getInt("RC_TOP_K", 5),
		IngestWorkers: getInt("RC_INGEST_WORKERS", 4),
		QueueSize:     getInt("RC_QUEUE_SIZE", 256),
		HTTPTimeout:   getDuration("RC_HTTP_TIMEOUT", 5*time.Minute),
	}, nil
}

// loadDotEnv — минимальный парсер .env без внешних зависимостей.
// Формат: KEY=VALUE (или KEY: VALUE), пустые строки и # — комментарии,
// кавычки вокруг значения снимаются. Важно: уже установленные
// переменные окружения НЕ перезаписываем — приоритет у реального
// окружения, а .env лишь «подливает» недостающее.
// Отсутствие файла — не ошибка: конфигурирование через env остаётся
// рабочим и без него.
func loadDotEnv(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// Пробуем ещё и рядом с бинарником — удобно для готовых релизов:
		// запустил recall.exe в папке с .env — конфиг подхватился.
		if exe, exeErr := os.Executable(); exeErr == nil {
			data, err = os.ReadFile(filepath.Join(filepath.Dir(exe), ".env"))
		}
		if os.IsNotExist(err) {
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("read .env: %w", err)
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Разделитель — знак равенства; срезаем и «ключ: значение»,
		// который тоже встречается в экосистеме .env.
		var key, val string
		if i := strings.Index(line, "="); i >= 0 {
			key, val = line[:i], line[i+1:]
		} else if i := strings.Index(line, ": "); i >= 0 {
			key, val = line[:i], line[i+2:]
		} else {
			continue // не присваивание — пропускаем
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
	return nil
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
