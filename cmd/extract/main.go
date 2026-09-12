// 提取文档文字和图片的 demo。
//
// 遍历指定目录（或单个文件），把每个文档的逐页文字和图片输出到同名子目录：
//
//	test/中文.docx -> test/中文/page_1.txt, test/中文/page_2.txt,
//	                  test/中文/image_1.png, ...
//
// 用法:
//
//	go run ./cmd/extract [文件或目录]   （缺省为 test，不存在时退回 testfile）
//
// 解析失败的文件只会打印错误并计入统计，不会导致程序崩溃。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	fce "filecontentextractor"
)

var errUnsupported = fmt.Errorf("unsupported")

func main() {
	target := "test"
	if len(os.Args) > 1 {
		target = os.Args[1]
	}
	info, err := os.Stat(target)
	if err != nil {
		// 缺省目录不存在时退回 testfile。
		if target == "test" {
			if _, ferr := os.Stat("testfile"); ferr == nil {
				target = "testfile"
				info, err = os.Stat(target)
			}
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "路径不存在: %s\n", target)
			os.Exit(1)
		}
	}

	var files []string
	if info.IsDir() {
		entries, err := os.ReadDir(target)
		if err != nil {
			fmt.Fprintf(os.Stderr, "读取目录失败: %v\n", err)
			os.Exit(1)
		}
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			files = append(files, filepath.Join(target, e.Name()))
		}
	} else {
		files = append(files, target)
	}

	extracted, failed, skipped := 0, 0, 0
	for _, path := range files {
		switch err := extract(path); {
		case err == nil:
			extracted++
		case err == errUnsupported:
			skipped++
		default:
			failed++
		}
	}
	fmt.Printf("完成: 提取 %d，失败 %d，跳过(不支持的格式) %d\n", extracted, failed, skipped)
}

// extract 提取单个文件的文字和图片到同名子目录并打印耗时。
// 不支持的格式返回 errUnsupported，其余错误正常返回，绝不 panic。
func extract(path string) error {
	start := time.Now()
	doc, err := fce.Open(path)
	if err != nil {
		if strings.Contains(err.Error(), "unsupported file format") {
			fmt.Printf("跳过 %s: %v\n", path, err)
			return errUnsupported
		}
		fmt.Printf("失败 %s: %v (耗时 %s)\n", path, err, time.Since(start).Round(time.Millisecond))
		return err
	}

	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	outDir := filepath.Join(filepath.Dir(path), base)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Printf("失败 %s: %v\n", path, err)
		return err
	}

	imageCount := 0
	for _, page := range doc.Pages {
		txtPath := filepath.Join(outDir, fmt.Sprintf("page_%d.txt", page.Number))
		if err := os.WriteFile(txtPath, []byte(page.Text), 0o644); err != nil {
			fmt.Printf("失败 %s: 写 %s: %v\n", path, txtPath, err)
			return err
		}
		for _, img := range page.Images {
			if len(img.Data) == 0 {
				continue
			}
			imageCount++
			ext := sniffImageFormat(img.Data)
			if ext == "" {
				ext = img.Format
			}
			if ext == "" {
				ext = "bin"
			}
			imgPath := filepath.Join(outDir, fmt.Sprintf("image_%d.%s", imageCount, ext))
			if err := os.WriteFile(imgPath, img.Data, 0o644); err != nil {
				fmt.Printf("失败 %s: 写 %s: %v\n", path, imgPath, err)
				return err
			}
		}
	}
	fmt.Printf("提取 %s -> %s (%d 页, %d 张图片) 耗时: %s\n",
		path, outDir, len(doc.Pages), imageCount, time.Since(start).Round(time.Millisecond))
	return nil
}

// sniffImageFormat 按魔数识别真实图片格式，识别不出返回空串。
func sniffImageFormat(data []byte) string {
	switch {
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "png"
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return "jpg"
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return "gif"
	case len(data) >= 2 && string(data[:2]) == "BM":
		return "bmp"
	case len(data) >= 4 && string(data[:4]) == "II*\x00" || len(data) >= 4 && string(data[:4]) == "MM\x00*":
		return "tiff"
	}
	return ""
}
