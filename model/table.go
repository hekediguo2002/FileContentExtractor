package model

import (
	"strings"
	"unicode/utf8"
)

// TableTSV serializes a structured table with one physical line per row and
// one tab-separated field per column. Cell.Text remains unchanged; only the
// serialized representation removes layout-only line wrapping.
func TableTSV(table Table) string {
	columnCount := 0
	for _, row := range table.Rows {
		if len(row.Cells) > columnCount {
			columnCount = len(row.Cells)
		}
		for _, cell := range row.Cells {
			span := cell.ColSpan
			if span < 1 {
				span = 1
			}
			if end := cell.Column + span; end > columnCount {
				columnCount = end
			}
		}
	}
	if columnCount == 0 {
		return ""
	}

	rows := make([]string, 0, len(table.Rows))
	for _, row := range table.Rows {
		values := make([]string, columnCount)
		occupied := make([]bool, columnCount)
		next := 0
		for _, cell := range row.Cells {
			column := cell.Column
			if column < 0 || column >= columnCount || occupied[column] {
				column = nextFreeColumn(occupied, next)
			}
			if column >= columnCount {
				break
			}
			values[column] = FlattenCellText(cell.Text)
			span := cell.ColSpan
			if span < 1 {
				span = 1
			}
			for index := column; index < column+span && index < columnCount; index++ {
				occupied[index] = true
			}
			next = nextFreeColumn(occupied, 0)
		}
		rows = append(rows, strings.Join(values, "\t"))
	}
	return strings.Join(rows, "\n")
}

func nextFreeColumn(occupied []bool, start int) int {
	if start < 0 {
		start = 0
	}
	for start < len(occupied) && occupied[start] {
		start++
	}
	return start
}

// FlattenCellText removes tabs and layout line breaks from a single TSV field.
// A space is retained when a wrapped line splits two ASCII words or numbers.
func FlattenCellText(value string) string {
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == '\r' || r == '\n' || r == '\t'
	})
	var out strings.Builder
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if out.Len() > 0 {
			left, _ := utf8.DecodeLastRuneInString(out.String())
			right, _ := utf8.DecodeRuneInString(part)
			if asciiWordRune(left) && asciiWordRune(right) {
				out.WriteByte(' ')
			}
		}
		out.WriteString(part)
	}
	return out.String()
}

func asciiWordRune(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z'
}
