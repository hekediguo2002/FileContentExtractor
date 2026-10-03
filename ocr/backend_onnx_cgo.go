//go:build cgo

package ocr

import (
	"fmt"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

var (
	runtimeMutex sync.Mutex
	runtimePath  string
)

type onnxSession struct {
	session *ort.DynamicAdvancedSession
}

func newInferenceSession(runtimeLibraryPath, modelPath string) (inferenceSession, error) {
	if err := initializeRuntime(runtimeLibraryPath); err != nil {
		return nil, err
	}
	inputs, outputs, err := ort.GetInputOutputInfo(modelPath)
	if err != nil {
		return nil, fmt.Errorf("inspect ONNX model %q: %w", modelPath, err)
	}
	if len(inputs) != 1 || len(outputs) != 1 {
		return nil, fmt.Errorf("ONNX model %q has %d inputs and %d outputs; PaddleOCR expects one each", modelPath, len(inputs), len(outputs))
	}
	session, err := ort.NewDynamicAdvancedSession(modelPath,
		[]string{inputs[0].Name}, []string{outputs[0].Name}, nil)
	if err != nil {
		return nil, fmt.Errorf("open ONNX model %q: %w", modelPath, err)
	}
	return &onnxSession{session: session}, nil
}

func initializeRuntime(path string) error {
	runtimeMutex.Lock()
	defer runtimeMutex.Unlock()
	if ort.IsInitialized() {
		if runtimePath != path {
			return fmt.Errorf("ONNX Runtime is already initialized from %q, cannot switch to %q", runtimePath, path)
		}
		return nil
	}
	ort.SetSharedLibraryPath(path)
	if err := ort.InitializeEnvironment(); err != nil {
		return fmt.Errorf("load ONNX Runtime %q: %w", path, err)
	}
	runtimePath = path
	return nil
}

func (s *onnxSession) run(shape []int64, data []float32) (tensorOutput, error) {
	input, err := ort.NewTensor(ort.Shape(shape), data)
	if err != nil {
		return tensorOutput{}, fmt.Errorf("create ONNX input tensor: %w", err)
	}
	defer input.Destroy()
	outputs := []ort.ArbitraryTensor{nil}
	if err := s.session.Run([]ort.ArbitraryTensor{input}, outputs); err != nil {
		return tensorOutput{}, fmt.Errorf("run ONNX model: %w", err)
	}
	if outputs[0] == nil {
		return tensorOutput{}, fmt.Errorf("ONNX model returned no output")
	}
	defer outputs[0].Destroy()
	output, ok := outputs[0].(*ort.Tensor[float32])
	if !ok {
		return tensorOutput{}, fmt.Errorf("ONNX model returned %T, want float32 tensor", outputs[0])
	}
	return tensorOutput{shape: append([]int64(nil), output.GetShape()...), data: append([]float32(nil), output.GetData()...)}, nil
}
