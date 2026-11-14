// Package summarize — иерархическая выжимка документов (map-reduce).
//
// Map: документ читается по секциям из ~5 чанков ПО ПОРЯДКУ, каждая
// сжимается в структурированную заметку фактов. Reduce: заметки
// сливаются пачками, пока суммарная длина не упадёт до целевой
// (~10% исходника). Задача идёт фоном, результат — в базу и в .txt.
package summarize

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/WOLFraMix/recall/internal/store"
)

const (
	sectionChunks = 5                // чанков в секции (~4000 символов)
	sectionMax    = 1500             // предел заметки секции (было 600 — маленькие секции ужимались в кашу)
	reduceBatch   = 4                // заметок на один reduce-вызов
	finalMaxPass  = 2                // финальных проходов дожима не больше двух
	callTimeout   = 10 * time.Minute // на один вызов LLM
)

type Service struct {
	baseURL string
	model   string
	outDir  string
	st      *store.Store
	client  *http.Client
	running sync.Map // docID -> struct{}: защита от двойного запуска
}

func New(baseURL, model, outDir string, st *store.Store) *Service {
	return &Service{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		outDir:  outDir,
		st:      st,
		client:  &http.Client{Timeout: callTimeout},
	}
}

// Start запускает фоновую выжимку. false — задача уже идёт.
func (sv *Service) Start(docID int64) bool {
	if _, busy := sv.running.LoadOrStore(docID, struct{}{}); busy {
		return false
	}
	go func() {
		defer sv.running.Delete(docID)
		sv.run(context.Background(), docID)
	}()
	return true
}

// Промпты — главный урок первой выжимки: «сжато» модель поняла как
// «обобщи в три абзаца» и потеряла факты. Теперь просим ПЕРЕЧИСЛЕНИЕ:
// факты по порядку появления, с именами и числами. Генерализация = потеря.
const sysMap = "Ты — редактор-конспектолог. Тебе дают ОДНУ ЧАСТЬ большого " +
	"документа. Прочитай её и составь заметку фактов: перечисляй события, " +
	"утверждения, определения и инструкции В ТОМ ПОРЯДКЕ, В КОТОРОМ они " +
	"появляются в тексте. Каждое смысловое предложение источника — отдельный " +
	"пункт. Сохраняй имена, названия, числа, команды и формулировки ДОСЛОВНО, " +
	"где это важно. НИЧЕГО не добавляй от себя и не делай общих выводов " +
	"вместо конкретики. Пиши на русском."

const sysReduce = "Ты — редактор-конспектолог. Тебе дают последовательные " +
	"заметки о частях одного документа (они идут в порядке появления в " +
	"документе). Слей их в ЕДИНУЮ заметку: сохрани ВСЕ факты, имена, числа " +
	"и команды, убери только повторы и связки. Порядок фактов — как в " +
	"документе. Не обобщай: «поддержка форматов A и B» — правильно, " +
	"«добавлена поддержка форматов» — потеря. НИЧЕГО не выдумывай. " +
	"Пиши на русском."

func (sv *Service) run(ctx context.Context, docID int64) {
	fail := func(err error) {
		_ = sv.st.SetSummary(ctx, docID, "error", "", err.Error())
	}

	// Статус «running» — в базу с первой секунды. Раньше запись обновлялась
	// только в конце, и всё время работы UI читал старый статус (none или
	// прошлогодний ready): «Готово» через секунду после запуска и 404
	// на скачивании. Состояние задачи обязано жить в базе с первого
	// мгновения, а не только в финале.
	if err := sv.st.InitSummary(ctx, docID); err != nil {
		fail(err)
		return
	}

	texts, err := sv.st.DocChunks(ctx, docID)
	if err != nil {
		fail(err)
		return
	}
	if len(texts) == 0 {
		fail(fmt.Errorf("у документа нет чанков"))
		return
	}
	name, err := sv.st.DocName(ctx, docID)
	if err != nil {
		fail(err)
		return
	}

	total := 0
	for _, t := range texts {
		total += len(t)
	}
	// Цель ~10% исходника — но не меньше 800 символов: маленький документ
	// не обязан сжиматься в телеграмму. Второй урок первой выжимки:
	// у README 5 КБ было 600 символов лимита — и смысл умер.
	target := total / 10
	if target < 800 {
		target = 800
	}

	// --- Map: документ по порядку, секция за секцией ---
	var parts []string
	nSections := (len(texts) + sectionChunks - 1) / sectionChunks
	for start, i := 0, 1; start < len(texts); start, i = start+sectionChunks, i+1 {
		end := start + sectionChunks
		if end > len(texts) {
			end = len(texts)
		}
		section := strings.Join(texts[start:end], " ")
		out, err := sv.chat(ctx, sysMap,
			fmt.Sprintf("Часть %d из %d документа. Составь заметку фактов этой части.\n\n%s",
				i, nSections, section))
		if err != nil {
			fail(err)
			return
		}
		out = strings.TrimSpace(out)
		// Обрезаем по границе предложения, а не посередине слова:
		// жёсткий срез ломал последний факт заметки.
		if len(out) > sectionMax {
			if cut := strings.LastIndex(out[:sectionMax], ". "); cut > sectionMax/2 {
				out = out[:cut+1]
			} else {
				out = out[:sectionMax]
			}
		}
		parts = append(parts, out)
	}

	// --- Reduce: лесенка, пока не дожали до цели ---
	sumLen := func(ps []string) int {
		n := 0
		for _, p := range ps {
			n += len(p)
		}
		return n
	}
	for len(parts) > 1 && sumLen(parts) > target {
		var next []string
		for start := 0; start < len(parts); start += reduceBatch {
			end := start + reduceBatch
			if end > len(parts) {
				end = len(parts)
			}
			var sb strings.Builder
			for j, p := range parts[start:end] {
				fmt.Fprintf(&sb, "Заметка %d:\n%s\n\n", start+j+1, p)
			}
			out, err := sv.chat(ctx, sysReduce, sb.String())
			if err != nil {
				fail(err)
				return
			}
			next = append(next, strings.TrimSpace(out))
		}
		parts = next
	}

	final := strings.Join(parts, "\n\n")
	// Финальный дожим: не более finalMaxPass проходов и только если
	// сильно перебор (было target*2 — README сжимался до кашеобразности).
	for pass := 0; len(final) > target*3 && pass < finalMaxPass; pass++ {
		out, err := sv.chat(ctx, sysReduce,
			"Сократи эту заметку примерно на треть, сохранив ВСЕ имена, числа, команды и события.\n\n"+final)
		if err != nil {
			break // лучше длинная выжимка, чем упавшая задача
		}
		final = strings.TrimSpace(out)
	}

	// --- Запись: .txt файл + база ---
	if err := os.MkdirAll(sv.outDir, 0o755); err != nil {
		fail(err)
		return
	}
	safe := regexp.MustCompile(`[^\p{L}\p{N}\-_ ]+`).ReplaceAllString(name, "_")
	safe = strings.TrimSpace(safe)
	if safe == "" {
		safe = fmt.Sprintf("doc_%d", docID)
	}
	path := filepath.Join(sv.outDir, fmt.Sprintf("%d_%s.txt", docID, safe))
	body := fmt.Sprintf("Выжимка: %s\nСгенерировано Recall (модель %s), %s\n%s\n",
		name, sv.model, time.Now().Format("2006-01-02 15:04"),
		strings.Repeat("-", 60)+"\n"+final)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		fail(err)
		return
	}
	_ = sv.st.SetSummary(ctx, docID, "ready", final, "")
}

// chat — один вызов Ollama /api/chat без стриминга: выжимке стриминг
// не нужен, статус пользователь видит в UI по polling'у.
func (sv *Service) chat(ctx context.Context, system, user string) (string, error) {
	payload := map[string]any{
		"model": sv.model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"stream":  false,
		"options": map[string]any{"temperature": 0.2},
	}
	buf, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		sv.baseURL+"/api/chat", bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := sv.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama chat: статус %d", resp.StatusCode)
	}
	var out struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.Message.Content) == "" {
		return "", fmt.Errorf("пустой ответ модели")
	}
	return out.Message.Content, nil
}
