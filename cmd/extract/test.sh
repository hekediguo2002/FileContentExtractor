go build main.go
./main -ocr=true -onnxruntime ./ocr_runtime/runtime/lib/libonnxruntime.1.19.2.dylib -ocr-model-dir ./ocr_runtime/models