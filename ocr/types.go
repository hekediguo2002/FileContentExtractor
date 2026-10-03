package ocr

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// ErrUnavailable indicates that this build cannot run the optional ONNX
// backend. Normal document extraction does not require the backend.
var ErrUnavailable = errors.New("OCR is unavailable in this build")

// Config controls the optional PaddleOCR ONNX pipeline. Enabled is consumed by
// document extraction; direct image recognition is always explicit and ignores
// it.
type Config struct {
	Enabled                         bool
	RuntimePath                     string
	ModelDir                        string
	DetectionModelPath              string
	RecognitionModelPath            string
	ClassifierModelPath             string
	DictionaryPath                  string
	DetectionThreshold              float32
	BoxThreshold                    float32
	MinConfidence                   float32
	OrientationThreshold            float32
	MaxSideLength                   int
	RecognitionBatchSize            int
	PageWorkers                     int
	EnableOrientationClassification bool
	EnableDetectionDilation         bool
}

// Box is an OCR text rectangle in source-image pixel coordinates.
type Box struct {
	X, Y, Width, Height float64
}

// Line is one detected and recognized text region.
type Line struct {
	Text       string
	Confidence float32
	Bounds     Box
	Color      string
}

// Result contains OCR text in reading order and the corresponding regions.
type Result struct {
	Text  string
	Lines []Line
}

func (c Config) withDefaults() Config {
	if c.RuntimePath == "" {
		c.RuntimePath = os.Getenv("FCE_ONNXRUNTIME_PATH")
	}
	if c.ModelDir == "" {
		c.ModelDir = os.Getenv("FCE_PADDLEOCR_MODEL_DIR")
	}
	if c.ModelDir == "" {
		c.ModelDir = defaultAdjacentPath("ocr_models")
	}
	if c.RuntimePath == "" {
		c.RuntimePath = defaultAdjacentPath(runtimeLibraryName())
	}
	if c.DetectionModelPath == "" {
		c.DetectionModelPath = filepath.Join(c.ModelDir, "ch_PP-OCRv4_det_mobile.onnx")
	}
	if c.RecognitionModelPath == "" {
		c.RecognitionModelPath = filepath.Join(c.ModelDir, "ch_PP-OCRv4_rec_mobile.onnx")
	}
	if c.ClassifierModelPath == "" {
		c.ClassifierModelPath = filepath.Join(c.ModelDir, "ch_ppocr_mobile_v2.0_cls_mobile.onnx")
	}
	if c.DictionaryPath == "" {
		c.DictionaryPath = filepath.Join(c.ModelDir, "ppocr_keys_v1.txt")
	}
	if c.DetectionThreshold <= 0 {
		c.DetectionThreshold = 0.3
	}
	if c.BoxThreshold <= 0 {
		c.BoxThreshold = 0.6
	}
	if c.MinConfidence <= 0 {
		c.MinConfidence = 0.5
	}
	if c.OrientationThreshold <= 0 {
		c.OrientationThreshold = 0.9
	}
	if c.MaxSideLength <= 0 {
		c.MaxSideLength = 960
	}
	if c.RecognitionBatchSize <= 0 {
		c.RecognitionBatchSize = 1
	}
	if c.PageWorkers <= 0 {
		c.PageWorkers = 1
	}
	if c.PageWorkers < 1 {
		c.PageWorkers = 1
	}
	return c
}

func (c Config) validateFiles() error {
	files := []struct {
		name string
		path string
	}{
		{"ONNX Runtime", c.RuntimePath},
		{"detection model", c.DetectionModelPath},
		{"recognition model", c.RecognitionModelPath},
		{"character dictionary", c.DictionaryPath},
	}
	if c.EnableOrientationClassification {
		files = append(files, struct {
			name string
			path string
		}{"orientation classifier model", c.ClassifierModelPath})
	}
	for _, file := range files {
		if file.path == "" {
			return fmt.Errorf("OCR %s path is empty", file.name)
		}
		if info, err := os.Stat(file.path); err != nil {
			return fmt.Errorf("OCR %s %q: %w", file.name, file.path, err)
		} else if info.IsDir() {
			return fmt.Errorf("OCR %s %q is a directory", file.name, file.path)
		}
	}
	return nil
}

func (c Config) cacheKey() string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%g\x00%g\x00%g\x00%g\x00%d\x00%d\x00%t\x00%t",
		c.RuntimePath, c.DetectionModelPath, c.RecognitionModelPath,
		c.ClassifierModelPath, c.DictionaryPath, c.DetectionThreshold, c.BoxThreshold,
		c.MinConfidence, c.OrientationThreshold, c.MaxSideLength,
		c.RecognitionBatchSize, c.EnableOrientationClassification, c.EnableDetectionDilation)
}

func defaultAdjacentPath(name string) string {
	executable, err := os.Executable()
	if err == nil {
		candidate := filepath.Join(filepath.Dir(executable), name)
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate
		}
	}
	return name
}

func runtimeLibraryName() string {
	switch runtime.GOOS {
	case "windows":
		return "onnxruntime.dll"
	case "darwin":
		return "libonnxruntime.dylib"
	default:
		return "libonnxruntime.so"
	}
}
