package filecontentextractor

import (
	"fmt"
	"path/filepath"
	"strings"

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
type Page = model.Page
type Document = model.Document

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
		return docx.ParseFile(path)
	case ".pdf":
		return pdf.ParseFile(path)
	case ".doc":
		return doc.ParseFile(path)
	case ".xlsx":
		return xlsx.ParseFile(path)
	case ".xls":
		return xls.ParseFile(path)
	case ".pptx":
		return pptx.ParseFile(path)
	case ".ppt":
		return ppt.ParseFile(path)
	case ".rtf":
		return rtf.ParseFile(path)
	case ".ofd":
		return ofd.ParseFile(path)
	default:
		return nil, fmt.Errorf("unsupported file format: %s", ext)
	}
}

func Read(path string) (*Document, error) { return Open(path) }

func MustRead(path string) *Document {
	d, err := Open(path)
	if err != nil {
		panic(err)
	}
	return d
}
