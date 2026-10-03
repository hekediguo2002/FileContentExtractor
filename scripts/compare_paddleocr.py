#!/usr/bin/env python3
"""Compare Go OCR page text with Python PaddleOCR on the same PDF pages."""

import argparse
import difflib
import re
import sys
from pathlib import Path

import fitz
import numpy as np
from paddleocr import PaddleOCR


def compact(text: str) -> str:
    return re.sub(r"\s+", "", text)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("pdf", type=Path)
    parser.add_argument("go_output", type=Path)
    parser.add_argument("--det-model-dir")
    parser.add_argument("--rec-model-dir")
    parser.add_argument("--cls-model-dir")
    parser.add_argument("--minimum-ratio", type=float, default=0.75)
    parser.add_argument("--baseline-dir", type=Path, default=Path("tmp/ocr_compare"))
    args = parser.parse_args()

    kwargs = {"use_angle_cls": True, "lang": "ch", "use_gpu": False, "show_log": False}
    for option, key in (
        (args.det_model_dir, "det_model_dir"),
        (args.rec_model_dir, "rec_model_dir"),
        (args.cls_model_dir, "cls_model_dir"),
    ):
        if option:
            kwargs[key] = option
    engine = PaddleOCR(**kwargs)
    document = fitz.open(args.pdf)
    args.baseline_dir.mkdir(parents=True, exist_ok=True)
    failed = False
    for index, page in enumerate(document, start=1):
        pixmap = page.get_pixmap(alpha=False)
        image = np.frombuffer(pixmap.samples, dtype=np.uint8).reshape(pixmap.height, pixmap.width, pixmap.n)
        result = engine.ocr(image[:, :, :3], cls=True)
        lines = []
        for group in result or []:
            if group:
                lines.extend(item[1][0] for item in group)
        python_text = "\n".join(lines)
        (args.baseline_dir / f"page_{index}.txt").write_text(python_text, encoding="utf-8")
        go_path = args.go_output / f"page_{index}.txt"
        if not go_path.exists():
            print(f"page {index}: missing Go output {go_path}")
            failed = True
            continue
        go_text = go_path.read_text(encoding="utf-8")
        ratio = difflib.SequenceMatcher(None, compact(python_text), compact(go_text)).ratio()
        print(f"page {index}: similarity={ratio:.4f} python_chars={len(compact(python_text))} go_chars={len(compact(go_text))}")
        if index < len(document) and ratio < args.minimum_ratio:
            failed = True
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
