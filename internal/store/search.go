package store

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// phraseRe — извлекает фразы в кавычках: пользователь имеет право
// искать точное сочетание слов, а не россыпь терминов.
var phraseRe = regexp.MustCompile(`"([^"]+)"`)

// russianStop — служебные слова русского языка. В FTS они не должны
// становиться термами: bm25 понижает их вес в СВОЕЙ шкале, но RRF видит
// только позиции — а частотные слова дают им позиции, забивая канал шумом.
var russianStop = map[string]struct{}{
	"а": {}, "без": {}, "больше": {}, "будет": {}, "будто": {}, "бы": {},
	"был": {}, "была": {}, "были": {}, "было": {}, "быть": {}, "в": {},
	"вам": {}, "вас": {}, "вдруг": {}, "ведь": {}, "во": {}, "вот": {},
	"все": {}, "всего": {}, "всех": {}, "вы": {}, "где": {}, "да": {},
	"даже": {}, "для": {}, "до": {}, "его": {}, "ее": {}, "ей": {},
	"её": {}, "если": {}, "есть": {}, "еще": {}, "ещё": {}, "же": {},
	"за": {}, "здесь": {}, "и": {}, "из": {}, "или": {}, "им": {},
	"их": {}, "к": {}, "как": {}, "когда": {}, "кто": {}, "куда": {},
	"ли": {}, "лишь": {}, "либо": {}, "мне": {}, "может": {}, "мы": {},
	"на": {}, "над": {}, "не": {}, "него": {}, "нее": {}, "неё": {},
	"нет": {}, "ни": {}, "нибудь": {}, "но": {}, "о": {}, "об": {},
	"однако": {}, "он": {}, "она": {}, "они": {}, "опять": {}, "от": {},
	"по": {}, "под": {}, "пока": {}, "потом": {}, "потому": {}, "при": {},
	"про": {}, "просто": {}, "пусть": {}, "раз": {}, "с": {}, "сам": {},
	"свой": {}, "себе": {}, "себя": {}, "со": {}, "совсем": {}, "так": {},
	"также": {}, "такой": {}, "там": {}, "тем": {}, "теперь": {}, "то": {},
	"тогда": {}, "того": {}, "тоже": {}, "той": {}, "только": {}, "том": {},
	"тот": {}, "тут": {}, "ты": {}, "у": {}, "уже": {}, "хоть": {},
	"чего": {}, "чем": {}, "через": {}, "что": {}, "чтобы": {}, "эти": {},
	"это": {}, "этот": {}, "я": {},
}

// IsStopword — проверка, является ли слово служебным (для инструментов,
// которые должны повторять логику FTS-канала: ftscheck, подсветка).
func IsStopword(w string) bool {
	_, ok := russianStop[strings.ToLower(w)]
	return ok
}

// Параметры DF-фильтра — уроки бенчмарка:
//
//	dfAbsPct — абсолютный потолок: терм в >5% чанков корпуса не
//	           различает документы;
//	dfRelFactor — относительный потолок: терм в >10 раз частотнее
//	           самого редкого терма запроса — шум на фоне якоря;
//	dfMinKeep — нижняя граница порогов: запрос с одним сверхредким
//	           словом не должен выкашивать всех соседей.
const (
	dfAbsPct    = 5
	dfRelFactor = 10
	dfMinKeep   = 5
)

// Параметры переранжирования — второй этап fusion:
//
//	covBonus — за каждую покрытую основу запроса в тексте чанка;
//	phraseBonus — за точную фразу из кавычек.
//
// RRF видит только ПОЗИЦИИ каналов и не знает, чем именно чанк совпал:
// «машинально» и «машине» дают одинаковые позиции. Покрытие различает
// чанк, отвечающий на весь запрос, от чанка с одним случайным словом.
const (
	covBonus    = 0.03
	phraseBonus = 0.05
)

// Hit — результат поиска: чанк, его документ и оценка канала.
type Hit struct {
	ChunkID int64
	DocID   int64
	DocName string
	Seq     int
	Text    string
	Score   float64
}

// TermDF — строка «плана запроса»: терм, его частота в корпусе
// и решение фильтра. Используется ftscheck: диагностика обязана
// показывать те же решения, что принимает продакшен-канал.
type TermDF struct {
	Term   string
	DF     int
	Kept   bool
	Reason string
}

// termInfo — терм в машинной форме: для переранжирования нужен не
// MATCH-синтаксис, а основа, которую можно искать подстрокой в тексте.
type termInfo struct {
	stem     string // основа без * и кавычек
	isPhrase bool
}

// normalize — общая нормализация сравнения текста: нижний регистр, ё → е.
func normalize(s string) string {
	return strings.ReplaceAll(strings.ToLower(s), "ё", "е")
}

// planAll — единая точка построения FTS-запроса: фразы, стоп-слова,
// псевдо-стемминг, DF-фильтр. KeywordSearch и HybridSearch зовут её,
// поэтому логика не может разъехаться между каналами.
// Возвращает готовое MATCH-выражение, df самого редкого терма
// (специфичность — для адаптивного веса канала), машинные термы
// (для переранжирования) и человекочитаемый план (для ftscheck).
func (s *Store) planAll(ctx context.Context, query string) (string, int, []termInfo, []TermDF, error) {
	phrases := phraseRe.FindAllString(query, -1)
	rest := phraseRe.ReplaceAllString(query, " ")

	var kept []string
	var keptTerms []termInfo
	var plan []TermDF

	// Фразы — явный выбор пользователя: не фильтруем ни чем.
	for _, p := range phrases {
		p = strings.Join(strings.Fields(p), " ")
		if p == "" {
			continue
		}
		p = strings.ReplaceAll(p, `"`, `""`)
		term := `"` + p + `"`
		kept = append(kept, term)
		keptTerms = append(keptTerms, termInfo{stem: normalize(p), isPhrase: true})
		plan = append(plan, TermDF{Term: term, Kept: true, Reason: "фраза — не фильтруем"})
	}

	// Одиночные слова: чистка → стоп-лист → псевдо-стемминг.
	var words []string
	for _, w := range strings.Fields(rest) {
		w = strings.ToLower(strings.TrimFunc(w, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		}))
		if w == "" {
			continue
		}
		if _, stop := russianStop[w]; stop {
			plan = append(plan, TermDF{Term: w, Kept: false, Reason: "стоп-слово"})
			continue
		}
		r := []rune(w)
		if len(r) == 0 {
			continue
		}
		if len(r) > 4 {
			// Псевдо-стемминг: unicode61 не знает русской морфологии,
			// «машина» и «машине» для него разные слова. Префикс *
			// покрывает формы ценой лёгкого шума.
			stem := string(r[:len(r)-1])
			words = append(words, stem+"*")
			keptTerms = append(keptTerms, termInfo{stem: normalize(stem)})
		} else {
			q := `"` + strings.ReplaceAll(w, `"`, `""`) + `"`
			words = append(words, q)
			keptTerms = append(keptTerms, termInfo{stem: normalize(w)})
		}
	}
	if len(words) == 0 {
		if len(kept) == 0 {
			return "", 0, keptTerms, plan, nil
		}
		return strings.Join(kept, " OR "), 0, keptTerms, plan, nil
	}

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks`).Scan(&total); err != nil {
		return "", 0, keptTerms, plan, err
	}
	absLimit := total * dfAbsPct / 100
	if absLimit < dfMinKeep {
		absLimit = dfMinKeep
	}

	// df каждого терма — один COUNT по FTS-индексу, микросекунды.
	type wdf struct {
		term string
		df   int
	}
	var wdfs []wdf
	for _, term := range words {
		var df int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM chunks_fts WHERE chunks_fts MATCH ?`, term,
		).Scan(&df); err != nil {
			plan = append(plan, TermDF{Term: term, Kept: false, Reason: "ошибка подсчёта"})
			continue
		}
		wdfs = append(wdfs, wdf{term, df})
	}

	// Специфичность запроса = df самого редкого встречающегося терма.
	dfMin := -1
	for _, d := range wdfs {
		if d.df > 0 && (dfMin == -1 || d.df < dfMin) {
			dfMin = d.df
		}
	}
	relLimit := 0
	if dfMin > 0 {
		relLimit = dfRelFactor * dfMin
	}

	for _, d := range wdfs {
		switch {
		case d.df == 0:
			plan = append(plan, TermDF{d.term, 0, false, "нет в корпусе"})
		case d.df > absLimit:
			plan = append(plan, TermDF{d.term, d.df, false,
				fmt.Sprintf("частотный: %d > %d (5%% корпуса)", d.df, absLimit)})
		case relLimit > 0 && d.df > relLimit:
			plan = append(plan, TermDF{d.term, d.df, false,
				fmt.Sprintf("шумный: %d > %d (%d× редчайшего)", d.df, relLimit, dfRelFactor)})
		default:
			kept = append(kept, d.term)
			// В keptTerms попадают только живые термы — переранжирование
			// считает покрытие по ним, а не по отсечённому шуму.
			stem := strings.Trim(d.term, `*"`)
			keptTerms = append(keptTerms, termInfo{stem: normalize(stem)})
			plan = append(plan, TermDF{d.term, d.df, true, ""})
		}
	}

	// Всё отсечено — берём два самых редких: канал пусть молчит
	// осмысленно, а не возвращает ноль.
	if len(kept) == 0 && len(wdfs) > 0 {
		sort.Slice(wdfs, func(i, j int) bool { return wdfs[i].df < wdfs[j].df })
		for i, d := range wdfs {
			if i == 2 {
				break
			}
			if d.df == 0 {
				continue
			}
			kept = append(kept, d.term)
			stem := strings.Trim(d.term, `*"`)
			keptTerms = append(keptTerms, termInfo{stem: normalize(stem)})
		}
	}
	if len(kept) == 0 {
		return "", 0, keptTerms, plan, nil
	}
	if dfMin < 0 {
		dfMin = 0
	}
	return strings.Join(kept, " OR "), dfMin, keptTerms, plan, nil
}

// PlanQuery — разбор запроса для диагностики (ftscheck).
func (s *Store) PlanQuery(ctx context.Context, query string) ([]TermDF, int, error) {
	_, dfMin, _, plan, err := s.planAll(ctx, query)
	return plan, dfMin, err
}

// searchFTS — исполнение готового MATCH-выражения.
func (s *Store) searchFTS(ctx context.Context, match string, k int) ([]Hit, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.doc_id, d.name, c.seq, c.text, bm25(chunks_fts) AS score
		FROM chunks_fts f
		JOIN chunks c ON c.id = f.rowid
		JOIN documents d ON d.id = c.doc_id
		WHERE chunks_fts MATCH ?
		ORDER BY score ASC
		LIMIT ?`, match, k)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.ChunkID, &h.DocID, &h.DocName, &h.Seq, &h.Text, &h.Score); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// KeywordSearch — полнотекстовый канал: план запроса + исполнение.
func (s *Store) KeywordSearch(ctx context.Context, query string, k int) ([]Hit, error) {
	match, _, _, _, err := s.planAll(ctx, query)
	if err != nil {
		return nil, err
	}
	if match == "" {
		return nil, nil
	}
	return s.searchFTS(ctx, match, k)
}

// VectorSearch — семантический канал (без изменений).
func (s *Store) VectorSearch(ctx context.Context, queryVec []float32, k int) ([]Hit, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.doc_id, d.name, c.seq, c.text, c.embedding
		FROM chunks c
		JOIN documents d ON d.id = c.doc_id
		WHERE d.status = 'ready'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var all []Hit
	for rows.Next() {
		var h Hit
		var blob []byte
		if err := rows.Scan(&h.ChunkID, &h.DocID, &h.DocName, &h.Seq, &h.Text, &blob); err != nil {
			return nil, err
		}
		h.Score = cosine(queryVec, decodeVec(blob))
		all = append(all, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortByScoreDesc(all)
	if len(all) > k {
		all = all[:k]
	}
	return all, nil
}

// HybridSearch — fusion + переранжирование.
// Этап 1 (RRF с адаптивным весом FTS) собирает кандидат: кто в каком
// канале на какой позиции. Этап 2 (rerank) пересортировывает по
// признакам, которых у RRF нет: покрытие основ запроса и точные фразы.
func (s *Store) HybridSearch(ctx context.Context, queryVec []float32, query string, k int) ([]Hit, error) {
	match, dfMin, keptTerms, _, err := s.planAll(ctx, query)
	if err != nil {
		return nil, err
	}

	// Адаптивный вес FTS: редкий якорь → точный сигнал → вес 2;
	// только частотные слова → слабый сигнал → 0.5.
	weightFTS := 2.0
	switch {
	case dfMin > 30:
		weightFTS = 0.5
	case dfMin > 10:
		weightFTS = 1.0
	}

	// Кандидатов берём шире итога (k*3): переранжирование осмысленно
	// только когда есть из чего выбирать — правильный чанк может
	// стоять на 30-м месте канала и всё равно подняться на этап 2.
	var kw []Hit
	if match != "" {
		kw, err = s.searchFTS(ctx, match, k*3)
		if err != nil {
			return nil, err
		}
	}
	vec, err := s.VectorSearch(ctx, queryVec, k*3)
	if err != nil {
		return nil, err
	}

	const rrfK = 60.0
	scores := map[int64]float64{}
	bests := map[int64]Hit{}

	add := func(hits []Hit, w float64) {
		for i, h := range hits {
			rank := i + 1
			scores[h.ChunkID] += w / (rrfK + float64(rank))
			if prev, ok := bests[h.ChunkID]; !ok || len(h.Text) > len(prev.Text) {
				bests[h.ChunkID] = h
			}
		}
	}
	add(kw, weightFTS)
	add(vec, 1.0)

	// Этап 2: переранжирование. RRF не отличает «чанк отвечает на весь
	// запрос» от «чанк поймал одно случайное слово» — покрытие основ
	// различает. Фраза в кавычках — самый сильный пользовательский
	// сигнал, ей отдельный бонус.
	if len(keptTerms) > 0 {
		for id, h := range bests {
			txt := normalize(h.Text)
			matched := 0
			phraseHit := false
			for _, t := range keptTerms {
				if strings.Contains(txt, t.stem) {
					matched++
					if t.isPhrase {
						phraseHit = true
					}
				}
			}
			scores[id] += covBonus * float64(matched)
			if phraseHit {
				scores[id] += phraseBonus
			}
		}
	}

	merged := make([]Hit, 0, len(bests))
	for id, h := range bests {
		h.Score = scores[id]
		merged = append(merged, h)
	}
	sortByScoreDesc(merged)
	if len(merged) > k {
		merged = merged[:k]
	}
	return merged, nil
}
