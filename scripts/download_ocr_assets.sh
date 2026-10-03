#!/usr/bin/env sh
set -eu

destination=${1:-ocr_runtime}
mkdir -p "$destination/models" "$destination/runtime"

download() {
    url=$1
    output=$2
    if command -v curl >/dev/null 2>&1; then
        curl -L --fail --retry 2 -o "$output" "$url"
    elif command -v wget >/dev/null 2>&1; then
        wget -O "$output" "$url"
    else
        echo "curl or wget is required" >&2
        exit 1
    fi
}

verify() {
    expected=$1
    file=$2
    if command -v shasum >/dev/null 2>&1; then
        actual=$(shasum -a 256 "$file" | awk '{print $1}')
    elif command -v sha256sum >/dev/null 2>&1; then
        actual=$(sha256sum "$file" | awk '{print $1}')
    else
        echo "shasum or sha256sum is required to verify OCR assets" >&2
        exit 1
    fi
    if [ "$actual" != "$expected" ]; then
        echo "checksum mismatch for $file: got $actual, want $expected" >&2
        exit 1
    fi
}

model_base=https://www.modelscope.cn/models/RapidAI/RapidOCR/resolve/v3.9.2/onnx/PP-OCRv4
download "$model_base/det/ch_PP-OCRv4_det_mobile.onnx" "$destination/models/ch_PP-OCRv4_det_mobile.onnx"
download "$model_base/rec/ch_PP-OCRv4_rec_mobile.onnx" "$destination/models/ch_PP-OCRv4_rec_mobile.onnx"
download "$model_base/cls/ch_ppocr_mobile_v2.0_cls_mobile.onnx" "$destination/models/ch_ppocr_mobile_v2.0_cls_mobile.onnx"
download "https://www.modelscope.cn/models/RapidAI/RapidOCR/resolve/v2.0.7/paddle/PP-OCRv4/rec/ch_PP-OCRv4_rec_infer/ppocr_keys_v1.txt" "$destination/models/ppocr_keys_v1.txt"
verify d2a7720d45a54257208b1e13e36a8479894cb74155a5efe29462512d42f49da9 "$destination/models/ch_PP-OCRv4_det_mobile.onnx"
verify 48fc40f24f6d2a207a2b1091d3437eb3cc3eb6b676dc3ef9c37384005483683b "$destination/models/ch_PP-OCRv4_rec_mobile.onnx"
verify e47acedf663230f8863ff1ab0e64dd2d82b838fceb5957146dab185a89d6215c "$destination/models/ch_ppocr_mobile_v2.0_cls_mobile.onnx"
verify 28b2362ad4ab2dc38769aa72feb535e3a9ddb3fd2a7585a05920e6393b1dc7f7 "$destination/models/ppocr_keys_v1.txt"

case "$(uname -s):$(uname -m)" in
    Darwin:x86_64)
        archive="$destination/onnxruntime.tgz"
        download "https://github.com/microsoft/onnxruntime/releases/download/v1.19.2/onnxruntime-osx-x86_64-1.19.2.tgz" "$archive"
        verify 6536e36d7ea92e32d53dad7ddd0fdf10be5b62d1dace85a13e1295ff81e9b5d4 "$archive"
        tar -xzf "$archive" -C "$destination/runtime" --strip-components=1
        ;;
    Darwin:arm64)
        archive="$destination/onnxruntime.tgz"
        download "https://github.com/microsoft/onnxruntime/releases/download/v1.19.2/onnxruntime-osx-arm64-1.19.2.tgz" "$archive"
        verify 370c49770e2e1f243e17c7b227bb7f4b3da793b847d02f38016dc0e46c30fbe1 "$archive"
        tar -xzf "$archive" -C "$destination/runtime" --strip-components=1
        ;;
    MINGW*:x86_64|MSYS*:x86_64|CYGWIN*:x86_64)
        archive="$destination/onnxruntime-win7.zip"
        download "https://github.com/yycmagic/onnxruntime-for-win7/releases/download/onnxruntime-win-1.20.0-for-win7/onnxruntime-win-1.20.0-for-win7.zip" "$archive"
        verify 3dd631d8cbb61102754fa9a10a1fd7eac5eff395e936437157df6940cf428f91 "$archive"
        unzip -q "$archive" "x64/*" -d "$destination/runtime"
        echo "Copy every DLL from $destination/runtime/x64 next to the executable on Windows 7."
        ;;
    *)
        echo "Unsupported platform. Download ONNX Runtime manually; PaddleOCR models are in $destination/models." >&2
        exit 1
        ;;
esac

echo "OCR assets downloaded to $destination"
