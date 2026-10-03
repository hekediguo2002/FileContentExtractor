package filecontentextractor

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

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
	var joined []rune
	lengths := make([]int, len(runs))
	for i := range runs {
		text := []rune(runs[i].Text)
		lengths[i] = len(text)
		joined = append(joined, text...)
	}
	remove := chineseWhitespaceDeletionMask(joined, 0, 0, false)
	offset := 0
	for i := range runs {
		var text strings.Builder
		for index, r := range joined[offset : offset+lengths[i]] {
			if !remove[offset+index] {
				text.WriteRune(r)
			}
		}
		runs[i].Text = text.String()
		offset += lengths[i]
	}
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
	remove := chineseWhitespaceDeletionMask(runes, before, after, preserveTSVRows)
	var out strings.Builder
	for i, r := range runes {
		if remove[i] {
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

func chineseWhitespaceDeletionMask(runes []rune, before, after rune, preserveTSVRows bool) []bool {
	remove := make([]bool, len(runes))
	leftRunes := make([]rune, len(runes))
	leftIndexes := make([]int, len(runes))
	lastRune, lastIndex := before, -1
	for i, r := range runes {
		leftRunes[i], leftIndexes[i] = lastRune, lastIndex
		if !unicode.IsSpace(r) {
			lastRune, lastIndex = r, i
		}
	}
	rightRunes := make([]rune, len(runes))
	nextRune := after
	for i := len(runes) - 1; i >= 0; i-- {
		rightRunes[i] = nextRune
		if !unicode.IsSpace(runes[i]) {
			nextRune = runes[i]
		}
	}
	markSpacedArabicYearWhitespace(runes, before, preserveTSVRows, remove)
	markSpacedChineseDateComponentWhitespace(runes, preserveTSVRows, remove)
	for i, r := range runes {
		if remove[i] || !isDateLayoutWhitespace(r) {
			continue
		}
		left, right := leftRunes[i], rightRunes[i]
		genericWhitespace := r != '\t'
		dateWhitespace := isChineseDateBoundary(left, right) ||
			isChineseDateRangeBoundary(left, right, leftIndexes[i], leftRunes, leftIndexes, before)
		shouldRemove := genericWhitespace && isChineseTextRune(left) && isChineseTextRune(right) || dateWhitespace
		structuralTSVWhitespace := preserveTSVRows && (r == '\t' || (r == '\r' || r == '\n') && isTSVRowBreak(runes, i))
		if shouldRemove && !structuralTSVWhitespace {
			remove[i] = true
		}
	}
	return remove
}

func markSpacedArabicYearWhitespace(runes []rune, before rune, preserveTSVRows bool, remove []bool) {
	previous := before
	for start := 0; start < len(runes); {
		if preserveTSVRows && (runes[start] == '\r' || runes[start] == '\n') && isTSVRowBreak(runes, start) {
			previous = 0
			start++
			continue
		}
		if !isASCIIDigit(runes[start]) {
			if !isDateLayoutWhitespace(runes[start]) {
				previous = runes[start]
			}
			start++
			continue
		}
		if isASCIIDigit(previous) {
			previous = runes[start]
			start++
			continue
		}
		var digits [4]rune
		digitCount := 0
		var whitespace []int
		end := start
		lastDigit := rune(0)
		for end < len(runes) {
			switch {
			case isASCIIDigit(runes[end]):
				lastDigit = runes[end]
				if digitCount >= len(digits) {
					digitCount++
					end++
					continue
				}
				digits[digitCount] = runes[end]
				digitCount++
			case isDateLayoutWhitespace(runes[end]):
				if preserveTSVRows && (runes[end] == '\r' || runes[end] == '\n') && isTSVRowBreak(runes, end) {
					goto validate
				}
				whitespace = append(whitespace, end)
			default:
				goto validate
			}
			end++
		}
	validate:
		if digitCount == len(digits) && end < len(runes) && runes[end] == '年' {
			year := 0
			for _, digit := range digits {
				year = year*10 + int(digit-'0')
			}
			if year >= 1000 && year <= 2999 {
				for _, index := range whitespace {
					remove[index] = true
				}
			}
		}
		previous = lastDigit
		start = end
	}
}

func markSpacedChineseDateComponentWhitespace(runes []rune, preserveTSVRows bool, remove []bool) {
	for start := 0; start < len(runes); {
		if !isASCIIDigit(runes[start]) {
			start++
			continue
		}
		var digits [2]rune
		digitCount := 0
		var whitespace []int
		end := start
		for end < len(runes) {
			switch {
			case isASCIIDigit(runes[end]):
				if digitCount < len(digits) {
					digits[digitCount] = runes[end]
				}
				digitCount++
			case isDateComponentWhitespace(runes[end]):
				if preserveTSVRows && (runes[end] == '\r' || runes[end] == '\n') && isTSVRowBreak(runes, end) {
					goto validate
				}
				whitespace = append(whitespace, end)
			default:
				goto validate
			}
			end++
		}
	validate:
		if digitCount > 0 && digitCount <= len(digits) && end < len(runes) {
			value := 0
			for i := 0; i < digitCount; i++ {
				value = value*10 + int(digits[i]-'0')
			}
			valid := runes[end] == '月' && value >= 1 && value <= 12 ||
				runes[end] == '日' && value >= 1 && value <= 31
			if valid {
				for _, index := range whitespace {
					remove[index] = true
				}
			}
		}
		start = end
	}
}

func isChineseDateBoundary(left, right rune) bool {
	return unicode.IsDigit(left) && strings.ContainsRune("年月日", right) ||
		strings.ContainsRune("年月", left) && unicode.IsDigit(right)
}

func isChineseDateRangeBoundary(left, right rune, leftIndex int, leftRunes []rune, leftIndexes []int, before rune) bool {
	if strings.ContainsRune("年月日", left) && isDateRangeConnector(right) {
		return true
	}
	if unicode.IsDigit(left) && isDateRangeConnector(right) {
		previous := before
		if leftIndex >= 0 {
			previous = leftRunes[leftIndex]
		}
		return strings.ContainsRune("年月日", previous)
	}
	if !isDateRangeConnector(left) || !unicode.IsDigit(right) {
		return false
	}
	previousIndex := -1
	previous := before
	if leftIndex >= 0 {
		previous = leftRunes[leftIndex]
		previousIndex = leftIndexes[leftIndex]
	}
	if strings.ContainsRune("年月日", previous) {
		return true
	}
	if !unicode.IsDigit(previous) || previousIndex < 0 {
		return false
	}
	return strings.ContainsRune("年月日", leftRunes[previousIndex])
}

func isDateRangeConnector(r rune) bool {
	return strings.ContainsRune("-－–—~～至", r)
}

func isDateLayoutWhitespace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\u00a0' || r == '\u3000' || r == '\r' || r == '\n'
}

func isDateComponentWhitespace(r rune) bool {
	return r == ' ' || r == '\u00a0' || r == '\u3000' || r == '\r' || r == '\n'
}

func isASCIIDigit(r rune) bool {
	return r >= '0' && r <= '9'
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
