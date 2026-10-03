package ocr

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

type paddleEngine struct {
	config     Config
	detector   inferenceSession
	recognizer inferenceSession
	classifier inferenceSession
	characters []string
}

var engineCache = struct {
	sync.Mutex
	engines map[string]*paddleEngine
}{engines: make(map[string]*paddleEngine)}

// ImageToText recognizes an image file with PaddleOCR ONNX.
func ImageToText(path string, config Config) (Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{}, fmt.Errorf("read OCR image %q: %w", path, err)
	}
	return ImageBytesToText(data, config)
}

// ImageBytesToText recognizes encoded PNG, JPEG, or GIF image data.
func ImageBytesToText(data []byte, config Config) (Result, error) {
	image, err := decodeImage(data)
	if err != nil {
		return Result{}, err
	}
	engine, err := getEngine(config)
	if err != nil {
		return Result{}, err
	}
	return engine.recognize(image)
}

func getEngine(config Config) (*paddleEngine, error) {
	config = config.withDefaults()
	if err := config.validateFiles(); err != nil {
		return nil, err
	}
	key := config.cacheKey()
	engineCache.Lock()
	defer engineCache.Unlock()
	if engine := engineCache.engines[key]; engine != nil {
		return engine, nil
	}
	detector, err := newInferenceSession(config.RuntimePath, config.DetectionModelPath)
	if err != nil {
		return nil, err
	}
	recognizer, err := newInferenceSession(config.RuntimePath, config.RecognitionModelPath)
	if err != nil {
		return nil, err
	}
	characters, err := loadDictionary(config.DictionaryPath)
	if err != nil {
		return nil, err
	}
	var classifier inferenceSession
	if config.EnableOrientationClassification {
		classifier, err = newInferenceSession(config.RuntimePath, config.ClassifierModelPath)
		if err != nil {
			return nil, err
		}
	}
	engine := &paddleEngine{
		config: config, detector: detector, recognizer: recognizer,
		classifier: classifier, characters: characters,
	}
	engineCache.engines[key] = engine
	return engine, nil
}

func (engine *paddleEngine) recognize(image *rgbImage) (Result, error) {
	detectorData, detectorShape, scaleX, scaleY := detectorInput(image, engine.config.MaxSideLength)
	detectorOutput, err := engine.detector.run(detectorShape, detectorData)
	if err != nil {
		return Result{}, err
	}
	boxes, err := detectorBoxes(detectorOutput, engine.config.DetectionThreshold,
		engine.config.BoxThreshold, scaleX, scaleY, image.width, image.height,
		engine.config.EnableDetectionDilation)
	if err != nil {
		return Result{}, err
	}
	crops := make([]*rgbImage, len(boxes))
	for index, box := range boxes {
		crops[index] = cropImage(image, box)
	}
	recognized, err := engine.recognizeCrops(crops)
	if err != nil {
		return Result{}, err
	}
	lines := make([]Line, 0, len(boxes))
	for index, box := range boxes {
		result := recognized[index]
		result.text = strings.TrimSpace(result.text)
		if result.text == "" || result.confidence < engine.config.MinConfidence {
			continue
		}
		lines = append(lines, Line{
			Text: result.text, Confidence: result.confidence,
			Bounds: Box{X: float64(box.x0), Y: float64(box.y0), Width: float64(box.x1 - box.x0), Height: float64(box.y1 - box.y0)},
			Color:  foregroundColor(crops[index]),
		})
	}
	parts := make([]string, len(lines))
	for index := range lines {
		parts[index] = lines[index].Text
	}
	return Result{Text: strings.Join(parts, "\n"), Lines: lines}, nil
}

func (engine *paddleEngine) recognizeCrops(crops []*rgbImage) ([]recognition, error) {
	if engine.classifier != nil {
		if err := classifyOrientations(engine.classifier, crops, engine.config.RecognitionBatchSize,
			engine.config.OrientationThreshold); err != nil {
			return nil, err
		}
	}
	recognized := make([]recognition, len(crops))
	order := make([]int, len(crops))
	for index := range order {
		order[index] = index
	}
	sort.SliceStable(order, func(i, j int) bool {
		left, right := crops[order[i]], crops[order[j]]
		return float64(left.width)/float64(maxInt(1, left.height)) <
			float64(right.width)/float64(maxInt(1, right.height))
	})
	batchSize := maxInt(1, engine.config.RecognitionBatchSize)
	for start := 0; start < len(order); start += batchSize {
		end := minInt(len(order), start+batchSize)
		batch := make([]*rgbImage, end-start)
		for index, originalIndex := range order[start:end] {
			batch[index] = crops[originalIndex]
		}
		input, shape := recognizerBatchInput(batch)
		output, err := engine.recognizer.run(shape, input)
		if err != nil {
			return nil, err
		}
		results, err := decodeRecognitionBatch(output, engine.characters)
		if err != nil {
			return nil, err
		}
		for index, result := range results {
			recognized[order[start+index]] = result
		}
	}
	return recognized, nil
}
