// 提取文档文字和图片、对 PNG/JPG 图片执行 OCR 的 demo。
//
// 遍历指定目录（或单个文件），把每个文档的逐页文字和图片输出到同名子目录：
//
//	test/中文.docx -> test/中文/page_1.txt, test/中文/page_2.txt,
//	                  test/中文/image_1.png, ...
//	test/扫描件.png  -> test/扫描件/page_1.txt（需要 -ocr=true）
//
// 用法:
//
//	go run ./cmd/extract [文件或目录]   （缺省为 test，不存在时退回 testfile）
//
// 解析失败的文件只会打印错误并计入统计，不会导致程序崩溃。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	fce "github.com/hekediguo2002/FileContentExtractor"
)

var (
	errUnsupported = fmt.Errorf("unsupported")
	errOCRDisabled = fmt.Errorf("OCR disabled")
)

func main() {
	ocrEnabled := flag.Bool("ocr", false, "对提取出的页面图片运行 PaddleOCR ONNX")
	runtimePath := flag.String("onnxruntime", "", "onnxruntime 动态库路径")
	modelDir := flag.String("ocr-model-dir", "", "PaddleOCR ONNX 模型目录")
	ocrWorkers := flag.Int("ocr-workers", 0, "OCR 页面并发数（0 使用 CPU 默认值 1）")
	ocrBatchSize := flag.Int("ocr-batch-size", 0, "OCR 文字识别批大小（0 使用 CPU 默认值 1）")
	ocrClassification := flag.Bool("ocr-cls", false, "启用可选的 0/180 度文字方向分类")
	ocrClassifierModel := flag.String("ocr-cls-model", "", "PaddleOCR 方向分类 ONNX 模型路径")
	ocrClassifierThreshold := flag.Float64("ocr-cls-threshold", 0, "方向分类旋转阈值（0 使用默认值 0.9）")
	ocrDilation := flag.Bool("ocr-dilation", false, "启用 DB 检测结果 2x2 膨胀后处理")
	flag.Parse()
	options := fce.Options{OCR: fce.OCRConfig{
		Enabled:                         *ocrEnabled,
		RuntimePath:                     *runtimePath,
		ModelDir:                        *modelDir,
		PageWorkers:                     *ocrWorkers,
		RecognitionBatchSize:            *ocrBatchSize,
		EnableOrientationClassification: *ocrClassification,
		ClassifierModelPath:             *ocrClassifierModel,
		OrientationThreshold:            float32(*ocrClassifierThreshold),
		EnableDetectionDilation:         *ocrDilation,
	}}
	target := "test"
	if flag.NArg() > 0 {
		target = flag.Arg(0)
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
		switch err := extract(path, options); {
		case err == nil:
			extracted++
		case err == errUnsupported || err == errOCRDisabled:
			skipped++
		default:
			failed++
		}
	}
	fmt.Printf("完成: 提取 %d，失败 %d，跳过 %d\n", extracted, failed, skipped)
}

// extract 提取单个文件的文字和图片到同名子目录并打印耗时。
// 不支持的格式返回 errUnsupported，其余错误正常返回，绝不 panic。
func extract(path string, options fce.Options) error {
	start := time.Now()
	if isOCRImage(path) {
		return extractOCRImage(path, options.OCR, start)
	}
	doc, err := fce.OpenWithOptions(path, options)
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
	tableCount := 0
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
		for tableIndex, table := range page.Tables {
			content := fce.TableTSV(table)
			if content == "" {
				continue
			}
			tableCount++
			tablePath := filepath.Join(outDir, fmt.Sprintf("page_%d_table_%d.tsv", page.Number, tableIndex+1))
			if err := os.WriteFile(tablePath, []byte(content), 0o644); err != nil {
				fmt.Printf("失败 %s: 写 %s: %v\n", path, tablePath, err)
				return err
			}
		}
	}
	fmt.Printf("提取 %s -> %s (%d 页, %d 张图片, %d 个表格) 耗时: %s\n",
		path, outDir, len(doc.Pages), imageCount, tableCount, time.Since(start).Round(time.Millisecond))
	return nil
}

func isOCRImage(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg":
		return true
	default:
		return false
	}
}

func extractOCRImage(path string, config fce.OCRConfig, start time.Time) error {
	if !config.Enabled {
		fmt.Printf("跳过 %s: PNG/JPG 文字识别需要 -ocr=true\n", path)
		return errOCRDisabled
	}
	text, err := fce.ImageToText(path, config)
	if err != nil {
		fmt.Printf("失败 %s: %v (耗时 %s)\n", path, err, time.Since(start).Round(time.Millisecond))
		return err
	}
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	outDir := filepath.Join(filepath.Dir(path), base)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Printf("失败 %s: %v\n", path, err)
		return err
	}
	textPath := filepath.Join(outDir, "page_1.txt")
	if err := os.WriteFile(textPath, []byte(text), 0o644); err != nil {
		fmt.Printf("失败 %s: 写 %s: %v\n", path, textPath, err)
		return err
	}
	fmt.Printf("OCR %s -> %s (1 页) 耗时: %s\n",
		path, outDir, time.Since(start).Round(time.Millisecond))
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
