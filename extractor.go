package filecontentextractor

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hekediguo2002/FileContentExtractor/doc"
	"github.com/hekediguo2002/FileContentExtractor/docx"
	"github.com/hekediguo2002/FileContentExtractor/model"
	"github.com/hekediguo2002/FileContentExtractor/ofd"
	"github.com/hekediguo2002/FileContentExtractor/pdf"
	"github.com/hekediguo2002/FileContentExtractor/ppt"
	"github.com/hekediguo2002/FileContentExtractor/pptx"
	"github.com/hekediguo2002/FileContentExtractor/rtf"
	"github.com/hekediguo2002/FileContentExtractor/xls"
	"github.com/hekediguo2002/FileContentExtractor/xlsx"
)

type Rect = model.Rect
type TextRun = model.TextRun
type Image = model.Image
type TableCell = model.TableCell
type TableRow = model.TableRow
type Table = model.Table
type Page = model.Page
type Document = model.Document

// TableTSV converts a structured table into one tab-separated line per row.
// Layout-only line breaks inside cells are removed; TableCell.Text is unchanged.
func TableTSV(table Table) string { return model.TableTSV(table) }

// Open 按扩展名选择解析器并提取文档内容。文档损坏或格式畸形时返回 error；
// 解析器内部的意外 panic 也会被 recover 并转换为 error，不会导致程序崩溃。
func Open(path string) (d *Document, err error) {
	defer func() {
		if r := recover(); r != nil {
			d, err = nil, fmt.Errorf("parse %s: internal panic: %v", path, r)
		}
	}()
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".docx":
		d, err = docx.ParseFile(path)
	case ".pdf":
		d, err = pdf.ParseFile(path)
	case ".doc":
		d, err = doc.ParseFile(path)
	case ".xlsx":
		d, err = xlsx.ParseFile(path)
	case ".xls":
		d, err = xls.ParseFile(path)
	case ".pptx":
		d, err = pptx.ParseFile(path)
	case ".ppt":
		d, err = ppt.ParseFile(path)
	case ".rtf":
		d, err = rtf.ParseFile(path)
	case ".ofd":
		d, err = ofd.ParseFile(path)
	default:
		return nil, fmt.Errorf("unsupported file format: %s", ext)
	}
	if err != nil || d == nil {
		return d, err
	}
	normalizeDocumentText(d)
	return d, nil
}

func Read(path string) (*Document, error) { return Open(path) }

func MustRead(path string) *Document {
	d, err := Open(path)
	if err != nil {
		panic(err)
	}
	return d
}

// normalizeDocumentText removes layout/OCR whitespace inserted between Chinese
// glyphs while preserving Latin word spaces and structural TSV row breaks.
func normalizeDocumentText(d *Document) {
	for pageIndex := range d.Pages {
		page := &d.Pages[pageIndex]
		protected, marker := protectTableLineBreaks(page.Text, page.Tables)
		page.Text = normalizeChinesePageWhitespace(protected)
		page.Text = strings.ReplaceAll(page.Text, marker, "\n")
		normalizeRuns(page.Runs)
		for tableIndex := range page.Tables {
			for rowIndex := range page.Tables[tableIndex].Rows {
				for cellIndex := range page.Tables[tableIndex].Rows[rowIndex].Cells {
					cell := &page.Tables[tableIndex].Rows[rowIndex].Cells[cellIndex]
					cell.Text = normalizeChineseSpaces(cell.Text)
					normalizeRuns(cell.Runs)
				}
			}
		}
	}
}

func protectTableLineBreaks(text string, tables []Table) (string, string) {
	marker := "\ue000FCE_TABLE_ROW\ue001"
	for strings.Contains(text, marker) {
		marker += "\ue001"
	}
	for _, table := range tables {
		tableText := model.TableTSV(table)
		if !strings.Contains(tableText, "\n") {
			continue
		}
		index := strings.Index(text, tableText)
		if index < 0 {
			continue
		}
		start, end := index, index+len(tableText)
		protected := strings.ReplaceAll(tableText, "\n", marker)
		if start > 0 && text[start-1] == '\n' {
			start--
			if start > 0 && text[start-1] == '\r' {
				start--
			}
			protected = marker + protected
		}
		if end < len(text) && text[end] == '\r' {
			end++
			if end < len(text) && text[end] == '\n' {
				end++
			}
			protected += marker
		} else if end < len(text) && text[end] == '\n' {
			end++
			protected += marker
		}
		text = text[:start] + protected + text[end:]
	}
	return text, marker
}

func normalizeRuns(runs []TextRun) {
	for i := range runs {
		prev := previousNonSpaceRune(runs, i)
		next := nextNonSpaceRune(runs, i)
		runs[i].Text = normalizeChineseSpacesWithContext(runs[i].Text, prev, next)
	}
}

func previousNonSpaceRune(runs []TextRun, before int) rune {
	for i := before - 1; i >= 0; i-- {
		text := runs[i].Text
		for len(text) > 0 {
			r, size := utf8.DecodeLastRuneInString(text)
			if !unicode.IsSpace(r) {
				return r
			}
			text = text[:len(text)-size]
		}
	}
	return 0
}

func nextNonSpaceRune(runs []TextRun, after int) rune {
	for i := after + 1; i < len(runs); i++ {
		for _, r := range runs[i].Text {
			if !unicode.IsSpace(r) {
				return r
			}
		}
	}
	return 0
}

func normalizeChineseSpaces(s string) string {
	return normalizeChineseWhitespaceWithContext(s, 0, 0, false)
}

func normalizeChineseSpacesWithContext(s string, before, after rune) string {
	return normalizeChineseWhitespaceWithContext(s, before, after, false)
}

func normalizeChinesePageWhitespace(s string) string {
	return normalizeChineseWhitespaceWithContext(s, 0, 0, true)
}

func normalizeChineseWhitespaceWithContext(s string, before, after rune, preserveTSVRows bool) string {
	runes := []rune(s)
	var out strings.Builder
	for i, r := range runes {
		isRemovable := r == ' ' || r == '\u00a0' || r == '\u3000' || r == '\r' || r == '\n'
		if !isRemovable {
			out.WriteRune(r)
			continue
		}
		left := before
		for j := i - 1; j >= 0; j-- {
			if !unicode.IsSpace(runes[j]) {
				left = runes[j]
				break
			}
		}
		right := after
		for j := i + 1; j < len(runes); j++ {
			if !unicode.IsSpace(runes[j]) {
				right = runes[j]
				break
			}
		}
		if isChineseTextRune(left) && isChineseTextRune(right) &&
			!(preserveTSVRows && (r == '\r' || r == '\n') && isTSVRowBreak(runes, i)) {
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

func isTSVRowBreak(runes []rune, index int) bool {
	start := index
	if start > 0 && runes[start] == '\n' && runes[start-1] == '\r' {
		start--
	}
	end := index
	if end+1 < len(runes) && runes[end] == '\r' && runes[end+1] == '\n' {
		end++
	}
	for i := start - 1; i >= 0 && runes[i] != '\r' && runes[i] != '\n'; i-- {
		if runes[i] == '\t' {
			return true
		}
	}
	for i := end + 1; i < len(runes) && runes[i] != '\r' && runes[i] != '\n'; i++ {
		if runes[i] == '\t' {
			return true
		}
	}
	return false
}

func isChineseTextRune(r rune) bool {
	if unicode.Is(unicode.Han, r) {
		return true
	}
	return (r >= 0x3000 && r <= 0x303f) ||
		(r >= 0xff01 && r <= 0xff65) || strings.ContainsRune("·—…‘’“”《》〈〉【】〔〕（），。！？；：、", r)
}
