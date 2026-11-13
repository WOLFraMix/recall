package ingest

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"

	pdf "github.com/ledongthuc/pdf"
)

// extractPDF вытаскивает текстовый слой PDF.
// ledongthuc/pdf — чистый Go (совместим с CGO_ENABLED=0 в Dockerfile).
// Метод r.GetPlainText() возвращает io.Reader со всем текстом документа:
// внутри он сам обходит страницы и кеширует шрифты — нам остаётся прочитать.
// Важно: это НЕ OCR. Если текст в PDF — картинка (скан), функция
// вернёт пустой слой, и мы честно сообщим об этом пользователю,
// а не запишем в базу «пустой документ».
func extractPDF(data []byte) (string, error) {
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("open pdf: %w", err)
	}

	rd, err := r.GetPlainText()
	if err != nil {
		return "", fmt.Errorf("pdf text layer: %w", err)
	}
	out, err := io.ReadAll(rd)
	if err != nil {
		return "", fmt.Errorf("read pdf text: %w", err)
	}
	if strings.TrimSpace(string(out)) == "" {
		return "", errors.New("в PDF нет текстового слоя (скан?) — поддерживаются только текстовые PDF")
	}
	return string(out), nil
}

// extractDOCX достаёт текст из .docx без внешних зависимостей.
// DOCX — это ZIP, в котором текст лежит в word/document.xml в виде
// последовательности <w:t>фрагмент</w:t> внутри абзацев <w:p>.
// Мы проходим по XML потоковым декодером (не грузим DOM целиком),
// собираем текст и перевод строки на границах абзацев.
// Старый бинарный .doc не поддерживаем — это уже другой формат,
// честно просим конвертировать в docx.
func extractDOCX(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("docx is not a zip: %w", err)
	}

	var docFile *zip.File
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			docFile = f
			break
		}
	}
	if docFile == nil {
		return "", errors.New("docx: word/document.xml не найден — это точно .docx, а не .doc?")
	}

	rc, err := docFile.Open()
	if err != nil {
		return "", fmt.Errorf("open document.xml: %w", err)
	}
	defer rc.Close()

	var sb strings.Builder
	inText := false
	dec := xml.NewDecoder(rc)
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("parse document.xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "t" {
				inText = true // тег <w:t> — текстовый узел
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				sb.WriteString("\n") // конец абзаца — перевод строки
			}
		case xml.CharData:
			if inText {
				sb.Write(t)
			}
		}
	}
	return sb.String(), nil
}
