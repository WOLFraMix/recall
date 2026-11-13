package ingest

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Тестируем чанкинг: покрытие, перекрытие, целостность рун.
func TestChunkText(t *testing.T) {
	// 2000 символов — при chunkSize=800 должно выйти ~3-4 чанка.
	text := strings.Repeat("абвгдёжзийклмнопрсту ", 100)
	chunks := chunkText(text, 800, 100)
	if len(chunks) < 2 {
		t.Fatalf("ожидались несколько чанков, получено %d", len(chunks))
	}
	for i, c := range chunks {
		if len(c) == 0 {
			t.Fatalf("чанк %d пустой", i)
		}
		if !utf8.ValidString(c) {
			t.Fatalf("чанк %d — невалидный UTF-8 (разрез посреди руны)", i)
		}
	}
}

func TestChunkTextOverlap(t *testing.T) {
	text := strings.Repeat("x", 1000)
	chunks := chunkText(text, 400, 50)
	// С перекрытием 50 и шагом 350: 1000/350 → 3 чанка.
	if len(chunks) < 3 {
		t.Fatalf("перекрытие не работает: %d чанков", len(chunks))
	}
}
