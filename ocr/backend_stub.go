//go:build !cgo

package ocr

import "fmt"

func newInferenceSession(runtimeLibraryPath, modelPath string) (inferenceSession, error) {
	return nil, fmt.Errorf("%w: rebuild with CGO_ENABLED=1 to use PaddleOCR ONNX", ErrUnavailable)
}
