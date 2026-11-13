// Package api — HTTP-слой Recall: REST-эндпоинты, SSE-стриминг чата,
// встроенная статика. Правило слоя: API ничего не делает сам — он
// переводит HTTP-мир (запросы, статусы, JSON) в вызовы store/ingest/llm.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/WOLFraMix/recall/internal/config"
	"github.com/WOLFraMix/recall/internal/ingest"
	"github.com/WOLFraMix/recall/internal/llm"
	"github.com/WOLFraMix/recall/internal/store"
	"github.com/WOLFraMix/recall/web"
	"github.com/gin-gonic/gin"
)

// maxUpload — лимит размера файла. Защита от «залили ISO-образ на 4 ГБ
// и положили сервер» — лимит на входе обязателен в любом публичном API.
const maxUpload = 16 << 20 // 16 МБ

// API — зависимости слоя. Всё передаётся извне (инверсия зависимостей):
// пакет не создаёт БД и клиентов сам, а значит легко тестируется.
type API struct {
	cfg config.Config
	st  *store.Store
	ing *ingest.Ingester
	llm llm.Provider
}

// New собирает роутер и возвращает *http.Server (а не gin-роутер!):
// main-у нужен именно http.Server — у него есть метод Shutdown()
// для graceful shutdown.
func New(cfg config.Config, st *store.Store, ing *ingest.Ingester, provider llm.Provider) *http.Server {
	gin.SetMode(gin.ReleaseMode)
	a := &API{cfg: cfg, st: st, ing: ing, llm: provider}

	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())

	// Порог, с которого multipart-файл уходит на диск, а не в память.
	// Меньше — быстрее загрузка, меньше риск OOM при пачке файлов.
	r.MaxMultipartMemory = 8 << 20

	r.POST("/api/documents", a.upload)
	r.GET("/api/documents", a.listDocs)
	r.DELETE("/api/documents/:id", a.deleteDoc)
	r.GET("/api/search", a.search)
	r.POST("/api/chat", a.chat)
	r.GET("/api/stats", a.stats)

	// Всё, что не попало в /api/* — встроенная статика из embed.FS.
	r.NoRoute(func(c *gin.Context) {
		http.FileServer(http.FS(web.Files)).ServeHTTP(c.Writer, c.Request)
	})

	return &http.Server{
		Addr:    cfg.Listen,
		Handler: r,
		// ReadHeaderTimeout — защита от slowloris-атаки: клиент,
		// приславший заголовки по кусочкам, не удержит соединение вечно.
		ReadHeaderTimeout: 5 * time.Second,
	}
}

// --- Загрузка документов ---

func (a *API) upload(c *gin.Context) {
	// Ограничиваем размер ещё до чтения: MaxBytesReader обрывает тело,
	// если клиент попытается прислать больше лимита.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxUpload)

	fh, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "нет файла в поле 'file'"})
		return
	}
	src, err := fh.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "не удалось открыть файл"})
		return
	}
	defer func() { _ = src.Close() }()

	// Читаем файл в память целиком: 16 МБ — допустимо для MVP.
	body, err := io.ReadAll(src)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "не удалось прочитать файл"})
		return
	}

	// Раннее отсечение бинарников — но только для ТЕКСТОВЫХ форматов.
	// PDF и DOCX — бинарные по своей природе: их UTF-8-валидность
	// проверять бессмысленно (всегда «невалидны»), корректность
	// определит специализированный парсер внутри пайплайна.
	// Без этой оговорки проверка отсекала бы их за миллисекунды
	// с бессмысленной ошибкой «не является текстом».
	switch strings.ToLower(filepath.Ext(fh.Filename)) {
	case ".pdf", ".docx":
		// валидность проверит парсер
	default:
		if !utf8.Valid(body) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "файл не является текстом (бинарный?)"})
			return
		}
	}

	docID, err := a.st.AddDocument(c.Request.Context(), fh.Filename, int64(len(body)))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Backpressure в действии: очередь полна → откатываем запись
	// и честно просим повторить позже. Пользователь видит отказ
	// немедленно, а не ждёт зависший ответ.
	if !a.ing.Submit(ingest.Task{DocID: docID, Name: fh.Filename, Body: body}) {
		_ = a.st.DeleteDocument(c.Request.Context(), docID)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "очередь обработки переполнена, попробуйте позже"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"id": docID, "name": fh.Filename, "status": "pending"})
}

// --- Список и удаление ---

func (a *API) listDocs(c *gin.Context) {
	docs, err := a.st.ListDocuments(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, docs)
}

func (a *API) deleteDoc(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "некорректный id"})
		return
	}
	// ErrNotFound из store → честный 404, а не 500.
	if err := a.st.DeleteDocument(c.Request.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "документ не найден"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusNoContent)
}

// --- Поиск (без LLM) ---

func (a *API) search(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	if q == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "пустой запрос"})
		return
	}
	ctx := c.Request.Context()

	// Вопрос тоже превращаем в вектор: тот же эмбеддер, что и для чанков.
	// Оба вектора в одном пространстве — только тогда близость имеет смысл.
	vecs, err := a.llm.Embed(ctx, []string{q})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "эмбеддер недоступен: " + err.Error()})
		return
	}
	hits, err := a.st.HybridSearch(ctx, vecs[0], q, a.cfg.TopK)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, hits)
}

// --- Чат со стримингом (SSE) ---
//
// Server-Sent Events: сервер держит HTTP-соединение открытым и шлёт
// события текстом. Формат прост: "event: имя\ndata: полезная нагрузка\n\n".
// Браузер (или fetch-ридер) читает строки по мере прихода. Выбор SSE
// против WebSocket: нам нужен только сервер → клиент поток, SSE проще
// и проходит через обычные HTTP-инфраструктуры.

func (a *API) chat(c *gin.Context) {
	var req struct {
		Question string `json:"question" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "нужен JSON {\"question\": \"...\"}"})
		return
	}

	// Контекст запроса отменяется, когда клиент уходит (кнопка «стоп»
	// в UI делает fetch.abort() — соединение рвётся, ctx отменяется,
	// и генерация модели прерывается физически.
	ctx := c.Request.Context()

	// 1. Retrieval: вектор вопроса → гибридный поиск.
	vecs, err := a.llm.Embed(ctx, []string{req.Question})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "эмбеддер недоступен: " + err.Error()})
		return
	}
	hits, err := a.st.HybridSearch(ctx, vecs[0], req.Question, a.cfg.TopK)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 2. Заголовки SSE ДО любого вывода в тело ответа.
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	// X-Accel-Buffering отключает буферизацию nginx — если когда-нибудь
	// поставим за реверс-прокси, токены не будут копиться пачками.
	c.Header("X-Accel-Buffering", "no")

	// 3. Источники уходим СРАЗУ, до генерации: пользователь видит,
	// на чём строится ответ, пока модель ещё думает.
	a.sse(c, "sources", sourcesOf(hits))

	// 4. Стриминг токенов: канал из провайдера → SSE-события.
	tokens, err := a.llm.ChatStream(ctx, buildPrompt(req.Question, hits))
	if err != nil {
		a.sse(c, "error", gin.H{"error": err.Error()})
		a.sse(c, "done", gin.H{})
		return
	}
	for tok := range tokens {
		a.sse(c, "token", tok)
	}
	a.sse(c, "done", gin.H{})
}

// sse — один кадр потока. JSON-кодируем нагрузку: так безопасны
// переводы строк и кавычки в тексте токенов (сырой текст ломал бы кадры).
// Flush обязателен: без него данные копятся в буфере ОС и пользователь
// получает весь ответ одним куском в конце — стриминг умирает.
func (a *API) sse(c *gin.Context, event string, payload any) {
	data, _ := json.Marshal(payload)
	fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, data)
	if f, ok := c.Writer.(http.Flusher); ok {
		f.Flush()
	}
}

// sourcesOf — проекция Hit → форма для фронтенда (только нужное поле).
func sourcesOf(hits []store.Hit) []map[string]any {
	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		out = append(out, map[string]any{
			"chunk_id": h.ChunkID,
			"doc":      h.DocName,
			"seq":      h.Seq + 1, // человеку понятнее счёт с 1
			"text":     h.Text,
			"score":    h.Score,
		})
	}
	return out
}

// buildPrompt — сердце RAG: склейка найденных фрагментов и вопроса
// в промпт. Инструкция «только на основе фрагментов» (grounding) —
// главный способ бороться с галлюцинациями модели.
func buildPrompt(question string, hits []store.Hit) []llm.Message {
	var sb strings.Builder
	for i, h := range hits {
		fmt.Fprintf(&sb, "[%d] Документ «%s», фрагмент %d:\n%s\n\n",
			i+1, h.DocName, h.Seq+1, h.Text)
	}
	return []llm.Message{
		{
			Role: "system",
			Content: "Ты — ассистент по личному архиву документов пользователя. " +
				"Отвечай ТОЛЬКО на основе приведённых фрагментов. Если ответа в них нет — " +
				"честно скажи об этом. Ссылайся на фрагменты по номеру [N] там, где " +
				"конкретное утверждение взято из них. Отвечай на языке вопроса. " +
				"Если вопрос требует определить «главное» (героя, тему, вывод) — рассмотри " +
				"ВСЕ фрагменты и все имена/понятия в них, а не первое встретившееся." +
				"Не утверждай как факт того, чего во фрагментах нет: если ни один фрагмент " +
				"не даёт прямого ответа (например, нигде не сказано, кто «главный»), — " +
				"так и скажи и перечисли кандидатов из фрагментов.",
		},
		{
			Role: "user",
			Content: "Фрагменты документов:\n\n" + sb.String() +
				"\nВопрос: " + question,
		},
	}
}

// --- Статистика ---

func (a *API) stats(c *gin.Context) {
	docs, chunks, err := a.st.Stats(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"docs":                 docs,
		"chunks":               chunks,
		"chunks_indexed_total": a.ing.ChunksTotal.Load(), // atomic: чтение без блокировок
	})
}
