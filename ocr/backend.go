package ocr

type tensorOutput struct {
	shape []int64
	data  []float32
}

type inferenceSession interface {
	run(shape []int64, data []float32) (tensorOutput, error)
}
