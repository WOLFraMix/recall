// Package ingest — конвейер обработки документов.
//
// Пайплайн: файл → парсинг текста → чанкинг → пул эмбеддингов → БД.
// Построен как producer/consumer: продюсеры (API) только кладут задачи
// в канал, консюмеры (worker pool) выполняют тяжёлую работу. Разделение
// позволяет API отвечать мгновенно, а обработку — распараллеливать
// и дозировать.
package ingest

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/WOLFraMix/recall/internal/llm"
	"github.com/WOLFraMix/recall/internal/store"
)

// Task — единица работы: документ, ждущий обработки.
type Task struct {
	DocID int64
	Name  string
	Body  []byte // содержимое файла уже в памяти — API прочитал его при загрузке
}

// Ingester владеет очередью задач и пулом воркеров.
type Ingester struct {
	store   *store.Store
	llm     llm.Provider
	embed   string // имя эмбеддинг-модели для прогресс-сообщений
	jobs    chan Task
	workers int
	wg      sync.WaitGroup

	// Атомарные счётчики для дашборда: сколько чанков проиндексировано
	// суммарно. atomic — потому что инкременты приходят из нескольких
	// горутин одновременно; мьютекс здесь лишний, одна атомарная
	// инструкция дешевле и без гонок.
	ChunksTotal atomic.Int64

	log *log.Logger
}

// New создаёт Ingester. Канал задач буферизованный — это и есть
// очередь backpressure: пока воркеры заняты, новые задачи ждут в ней.
func New(st *store.Store, provider llm.Provider, embedModel string, workers, queueSize int) *Ingester {
	return &Ingester{
		store:   st,
		llm:     provider,
		embed:   embedModel,
		jobs:    make(chan Task, queueSize),
		workers: workers,
		log:     log.New(log.Writer(), "ingest: ", log.LstdFlags),
	}
}

// Submit ставит документ в очередь. Неблокирующая: если очередь полна,
// возвращает false — API честно ответит 503 «перегружено», а не зависнет.
// Сравните с блокирующей отправкой: канал заполнен → горутина API спит
// до освобождения места → HTTP-запрос висит. Для загрузки файлов это
// хуже, чем мгновенный отказ: пользователь может повторить позже.
func (ing *Ingester) Submit(t Task) bool {
	select {
	case ing.jobs <- t:
		return true
	default:
		return false
	}
}

// Run запускает пул воркеров. Блокируется до отмены ctx — вызывается
// из main в отдельной горутине.
func (ing *Ingester) Run(ctx context.Context) {
	ing.wg.Add(ing.workers)
	for i := 0; i < ing.workers; i++ {
		go func(id int) {
			defer ing.wg.Done()
			ing.log.Printf("worker #%d started", id)
			for {
				select {
				case <-ctx.Done():
					return
				case task, ok := <-ing.jobs:
					if !ok {
						return
					}
					ing.process(ctx, task)
				}
			}
		}(i)
	}

	// Ждём завершения всех воркеров (graceful shutdown), логируем.
	go func() {
		<-ctx.Done() // это отдельный «сторож»: дожидается всех и пишет итог
		ing.wg.Wait()
		ing.log.Printf("all workers stopped, chunks indexed total: %d", ing.ChunksTotal.Load())
	}()
}

// Wait — используется при shutdown, чтобы main не закрыл БД раньше,
// чем воркеры закончат писать. Экспонируем ожидание наружу.
func (ing *Ingester) Wait() { ing.wg.Wait() }

// process — полный цикл одного документа.
// Каждый шаг обновляет статус в БД: если процесс упадёт, дашборд
// покажет, на каком этапе застрял документ — вместо молчаливой потери.
func (ing *Ingester) process(ctx context.Context, t Task) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	_ = ing.store.SetStatus(ctx, t.DocID, "parsing", "")

	text, err := extractText(t.Body, t.Name)
	if err != nil {
		ing.fail(ctx, t.DocID, fmt.Sprintf("extract text: %v", err))
		return
	}
	if strings.TrimSpace(text) == "" {
		ing.fail(ctx, t.DocID, "file contains no extractable text")
		return
	}

	chunks := chunkText(text, 800, 100)

	_ = ing.store.SetStatus(ctx, t.DocID, "embedding", "")

	// Эмбеддинг идёт батчами по 32: один HTTP-запрос на пачку.
	// Так экономим и сетевые рантрипы, и накладные расходы модели.
	const batchSize = 32
	for start := 0; start < len(chunks); start += batchSize {
		// Проверка отмены на каждой итерации: пользователь мог удалить
		// документ, приложение — выключиться. Длинный цикл без check-а
		// не заметил бы сигнал.
		if ctx.Err() != nil {
			ing.fail(context.Background(), t.DocID, "cancelled")
			return
		}
		end := min(start+batchSize, len(chunks))
		batch := chunks[start:end]

		vectors, err := ing.llm.Embed(ctx, batch)
		if err != nil {
			ing.fail(ctx, t.DocID, fmt.Sprintf("embed batch %d: %v", start/batchSize, err))
			return
		}

		// Записываем чанки этой пачки.
		for i, vec := range vectors {
			if err := ing.store.AddChunk(ctx, t.DocID, int64(start)+int64(i), batch[i], vec); err != nil {
				ing.fail(ctx, t.DocID, fmt.Sprintf("save chunk: %v", err))
				return
			}
			ing.ChunksTotal.Add(1)
		}
	}

	_ = ing.store.SetStatus(ctx, t.DocID, "ready", "")
	ing.log.Printf("doc #%d (%s): %d chunks indexed", t.DocID, t.Name, len(chunks))
}

// fail — единая точка «документ не прошёл»: статус + ошибка в БД.
func (ing *Ingester) fail(ctx context.Context, docID int64, msg string) {
	_ = ing.store.SetStatus(ctx, docID, "error", msg)
	ing.log.Printf("doc #%d failed: %s", docID, msg)
}

// --- Чанкинг ---
//
// Режем текст на куски ~chunkSize символов с перекрытием overlap.
// Перекрытие спасает от классической ошибки RAG: мысль, разорванная
// границей чанка, живёт в двух чанках сразу — хотя бы один из них
// окажется полным и попадёт в контекст модели.

func chunkText(text string, chunkSize, overlap int) []string {
	// Нормализуем пробелы: переносы строк и табы мешают и поиску,
	// и модели. Сжимаем последовательности whitespace в один пробел.
	text = strings.Join(strings.Fields(text), " ")

	if chunkSize <= 0 {
		chunkSize = 800
	}
	if overlap < 0 || overlap >= chunkSize {
		overlap = chunkSize / 8
	}

	var chunks []string
	for start := 0; start < len(text); {
		end := min(start+chunkSize, len(text))

		// Строку не режем посреди UTF-8 символа: отступаем назад,
		// пока не найдём начало корректной руны. Для кириллицы это
		// важно — каждый символ два байта, тупой срез по индексу
		// даёт невалидные байты в середине.
		for end > start && end < len(text) && !utf8.RuneStart(text[end]) {
			end--
		}

		chunk := strings.TrimSpace(text[start:end])
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
		if end >= len(text) {
			break
		}
		start = end - overlap // двигаемся с перекрытием
		if start <= 0 {
			start = end // страховка от зацикливания при аномальном overlap
		}
	}
	return chunks
}

// extractText достаёт текст из файла. MVP-версия честно и просто:
// UTF-8 текст читаем как есть; для .txt/.md/.html этого достаточно
// для полезной системы. PDF/DOCX — следующая фаза: там нужна внешняя
// библиотека, и мы сознательно не тянули её в ядро MVP.
func extractText(body []byte, name string) (string, error) {
	// Проверяем валидность UTF-8: бинарный файл (zip, картинка) вернёт
	// false, и мы откажемся вежливо, а не запишем мусор в индекс.
	if !utf8.Valid(body) {
		return "", fmt.Errorf("%s is not a text file (binary?)", name)
	}
	return string(body), nil
}
