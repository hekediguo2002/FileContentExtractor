package model

type Rect struct{ X, Y, Width, Height float64 }
type TextRun struct {
	Text   string
	Bounds Rect
	Font   string
	Size   float64
	Color  string
}
type Image struct {
	Name, Format  string
	Width, Height int
	Bounds        Rect
	Data          []byte
}
type TableCell struct {
	Row, Column      int
	RowSpan, ColSpan int
	Bounds           Rect
	Text             string
	Runs             []TextRun
}
type TableRow struct {
	Cells []TableCell
}
type Table struct {
	Bounds Rect
	Rows   []TableRow
}
type Page struct {
	Number        int
	Name          string
	Width, Height float64
	Text          string
	Runs          []TextRun
	Images        []Image
	Tables        []Table
}
type Document struct {
	Path, Format string
	Pagination   string
	Pages        []Page
}
