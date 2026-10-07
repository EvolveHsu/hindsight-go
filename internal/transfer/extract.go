// Package transfer also owns file-to-text conversion for the file_retain
// endpoint: the lite build converts the formats it can parse natively and
// refuses the rest with a clear error instead of silently storing bytes.
package transfer

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// SupportedExtracts lists the file extensions ConvertText accepts, for error
// messages.
const SupportedExtracts = ".txt, .md, .markdown, .text, .log, .csv, .tsv, .json, .yaml, .yml, .xml, .html, .htm, .rst, .tex, .pdf"

// ConvertText converts a stored file to text. Binary formats the lite build
// cannot parse (docx, pptx, xlsx, images, audio) return an error naming the
// supported set; upstream delegates those to pluggable parsers.
func ConvertText(filename string, contentType string, data []byte) (string, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".pdf":
		text, err := extractPDFText(data)
		if err != nil {
			return "", err
		}
		return text, nil
	}
	if isTextLike(ext, contentType) {
		if !utf8.Valid(data) {
			return "", fmt.Errorf("file %q is not valid UTF-8 text", filename)
		}
		return strings.TrimRight(string(data), "\n"), nil
	}
	return "", fmt.Errorf("cannot convert %q (%s) to text: this build parses %s", filename, describeType(ext, contentType), SupportedExtracts)
}

func describeType(ext, contentType string) string {
	switch {
	case ext != "" && contentType != "":
		return ext + " " + contentType
	case ext != "":
		return ext
	case contentType != "":
		return contentType
	default:
		return "unknown type"
	}
}

func isTextLike(ext, contentType string) bool {
	if strings.HasPrefix(contentType, "text/") {
		return true
	}
	switch ext {
	case ".txt", ".md", ".markdown", ".text", ".log", ".csv", ".tsv", ".json", ".yaml", ".yml",
		".xml", ".html", ".htm", ".rst", ".tex", ".go", ".py", ".js", ".ts", ".sql", ".sh":
		return true
	}
	switch contentType {
	case "application/json", "application/xml", "application/x-yaml", "application/yaml":
		return true
	}
	return false
}

var (
	pdfStreamRe = regexp.MustCompile(`(?s)stream\r?\n(.*?)\r?\nendstream`)
	pdfTextRe   = regexp.MustCompile(`\((?:\\.|[^\\()])*\)`)
)

// extractPDFText pulls the text operators out of a PDF's content streams. It
// handles uncompressed and FlateDecode streams, which covers the text PDFs
// produced by printers and office exports; scanned pages carry no text
// operators and are reported as such rather than guessed at.
func extractPDFText(data []byte) (string, error) {
	var parts []string
	for _, m := range pdfStreamRe.FindAllSubmatch(data, -1) {
		raw := m[1]
		content := raw
		if inflated, err := inflate(raw); err == nil {
			content = inflated
		}
		if !bytes.Contains(content, []byte("Tj")) && !bytes.Contains(content, []byte("TJ")) {
			continue
		}
		for _, t := range pdfTextRe.FindAll(content, -1) {
			parts = append(parts, unescapePDFString(string(t[1:len(t)-1])))
		}
	}
	text := strings.TrimSpace(strings.Join(parts, " "))
	if text == "" {
		return "", fmt.Errorf("PDF carries no extractable text (scanned documents need OCR, which this build does not run)")
	}
	return text, nil
}

func inflate(data []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, 64<<20))
	if err != nil {
		return nil, err
	}
	return out, nil
}

// unescapePDFString resolves the escape sequences a PDF literal string may
// carry, including the octal form used for byte escapes.
func unescapePDFString(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(s) {
			break
		}
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case '(', ')', '\\':
			b.WriteByte(s[i])
		case '\n':
			// line continuation
		default:
			if isOctal(s[i]) {
				j := i
				for j < len(s) && j < i+3 && isOctal(s[j]) {
					j++
				}
				if v, err := parseOctal(s[i:j]); err == nil {
					b.WriteByte(byte(v))
					i = j - 1
					continue
				}
			}
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

func parseOctal(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty octal escape")
	}
	n := 0
	for _, c := range []byte(s) {
		n = n*8 + int(c-'0')
	}
	return n, nil
}
