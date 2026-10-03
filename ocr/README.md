# Optional PaddleOCR ONNX Runtime

OCR is optional. `filecontentextractor.Open` does not initialize or load ONNX
Runtime. The shared library and models are required only when calling
`ImageToText`, `ImageBytesToText`, or `OpenWithOptions` with OCR enabled.

The supported model set is PP-OCRv4 mobile Chinese:

- `ch_PP-OCRv4_det_mobile.onnx`
- `ch_PP-OCRv4_rec_mobile.onnx`
- `ch_ppocr_mobile_v2.0_cls_mobile.onnx`（仅开启方向分类时加载）
- `ppocr_keys_v1.txt`

Run `scripts/download_ocr_assets.sh` on macOS or Git Bash/MSYS2. Models come
from RapidOCR's ONNX conversion of PaddleOCR and are licensed under Apache-2.0.
ONNX Runtime is MIT licensed.

## Runtime compatibility

- macOS 12.7.6 Intel/Apple Silicon: official ONNX Runtime 1.19.2 CPU library.
- Windows 7 SP1 x64: use the complete `x64` directory from
  `yycmagic/onnxruntime-for-win7`, release `onnxruntime-win-1.20.0-for-win7`.
  Keep all supplied DLLs together. Microsoft's official DLL does not support
  Windows 7.
- The Go binding is pinned to the ONNX Runtime 1.19 C API. ONNX Runtime 1.20
  preserves compatibility with that C API.

Windows OCR builds require `CGO_ENABLED=1` and a MinGW-w64 compiler. A normal
`CGO_ENABLED=0` build remains available and supports every non-OCR feature; its
OCR calls return `ocr.ErrUnavailable`.

Example Windows builds:

```bash
# Normal extractor: no OCR runtime is required.
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o extract.exe ./cmd/extract

# OCR-capable extractor, built on macOS/Linux with MinGW-w64.
CGO_ENABLED=1 GOOS=windows GOARCH=amd64 \
  CC=x86_64-w64-mingw32-gcc go build -o extract-ocr.exe ./cmd/extract
```

## Integration test

After downloading assets, set the runtime and model locations. On macOS Intel:

```bash
export FCE_ONNXRUNTIME_PATH="$PWD/ocr_runtime/runtime/lib/libonnxruntime.1.19.2.dylib"
export FCE_PADDLEOCR_MODEL_DIR="$PWD/ocr_runtime/models"
go test ./... -count=1
```

The real PDF OCR tests are skipped when either environment variable is absent.
To compare the generated page text with Python PaddleOCR 2.7.3:

```bash
python3 scripts/compare_paddleocr.py \
  'cmd/extract/test/红头文件样例1-关于加强煤矿冲击地压源头治理的通知（发改能源〔2019〕764号）.pdf' \
  'cmd/extract/test/红头文件样例1-关于加强煤矿冲击地压源头治理的通知（发改能源〔2019〕764号）'
```

## Pipeline tuning

The detector preprocessing, bilinear resizing, BGR/CHW normalization, DB
connected regions, optional 2x2 dilation, crop ordering, and foreground color
estimation are implemented in Go and do not require OpenCV. Recognition can
batch crops sorted by aspect ratio, and document OCR can process pages with a
configurable worker count. Both defaults are `1` on CPU: ONNX Runtime already
uses multiple threads internally, and this project's Intel macOS benchmark was
slower with batch size 6 or two page workers.

Set `EnableOrientationClassification` to load the classifier model and rotate
180-degree crops when its confidence reaches `OrientationThreshold` (default
0.9). This remains opt-in because it costs another model and can rotate an
ambiguous low-quality crop incorrectly. `EnableDetectionDilation` is also
opt-in so existing detection results remain stable.

## Asset checksums

```text
d2a7720d45a54257208b1e13e36a8479894cb74155a5efe29462512d42f49da9  ch_PP-OCRv4_det_mobile.onnx
48fc40f24f6d2a207a2b1091d3437eb3cc3eb6b676dc3ef9c37384005483683b  ch_PP-OCRv4_rec_mobile.onnx
e47acedf663230f8863ff1ab0e64dd2d82b838fceb5957146dab185a89d6215c  ch_ppocr_mobile_v2.0_cls_mobile.onnx
28b2362ad4ab2dc38769aa72feb535e3a9ddb3fd2a7585a05920e6393b1dc7f7  ppocr_keys_v1.txt
6536e36d7ea92e32d53dad7ddd0fdf10be5b62d1dace85a13e1295ff81e9b5d4  onnxruntime-osx-x86_64-1.19.2.tgz
370c49770e2e1f243e17c7b227bb7f4b3da793b847d02f38016dc0e46c30fbe1  onnxruntime-osx-arm64-1.19.2.tgz
3dd631d8cbb61102754fa9a10a1fd7eac5eff395e936437157df6940cf428f91  onnxruntime-win-1.20.0-for-win7.zip
```
