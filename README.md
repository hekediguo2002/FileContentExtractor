# FileContentExtractor

使用 Go 标准库从 **DOCX、DOC、XLSX、XLS、PPTX、PPT、PDF、RTF、OFD** 中提取文字、文字样式、位置和内嵌图片。所有格式都会转换成统一的数据模型，方便业务代码处理。

通过 `Open`/`Read` 返回的所有格式都会清理汉字及中文标点之间由 OCR 或排版产生的多余空格和软换行，例如 `关 于`、`关\n于` 都会规范为 `关于`；中文日期中的空白也会清理，例如 `2022 年 1 月 21 日`、`2017 年 4 月\n24 日` 会分别规范为 `2022年1月21日`、`2017年4月24日`。被空格、制表符或软换行拆开的四位年份也会在紧邻 `年` 时合并，例如 `2\t0\t26年`、`2 0 2 6 年 9 月` 会规范为 `2026年`、`2026年9月`；有效的月份、日期及年月区间同样会处理，例如 `2026年9月2 9日`、`2026年1 - 3月` 会规范为 `2026年9月29日`、`2026年1-3月`。英文单词之间的空格、英文换行以及普通 TSV 表格的单元格和行边界保持不变。DOC、DOCX 和带网格线的 PDF 表格还会按最小单元格返回结构化内容。

项目使用 Go 1.20。默认关闭 OCR 时不需要 Office、ONNX Runtime 或其他运行时组件；可选 PaddleOCR 功能使用动态加载的 ONNX Runtime，未开启时不会加载或依赖其 DLL/DYLIB。

> 这是面向“内容提取”的只读解析器，不是完整的 Word、PDF 或 PowerPoint 排版引擎。默认不执行 OCR；只有调用 OCR API 或通过 `OpenWithOptions` 显式开启时才执行。复杂排版的自动分页结果可能与原软件显示不同。

## 支持的格式

| 格式 | 主要内容 | 图片 | 页面含义 |
| --- | --- | --- | --- |
| DOCX | 正文、表格、文字样式 | 内嵌图片 | 按布局估算的页面 |
| DOC | Word 文本、文本框、基础字符样式 | JPEG、PNG 等 | 按布局估算的页面 |
| XLSX | 工作表单元格、单元格样式 | 工作表图片 | 一个工作表一页 |
| XLS | BIFF 单元格、字体和 XF 样式 | OfficeArt 图片 | 一个工作表一页 |
| PPTX | 文本框、表格、文字样式 | 幻灯片图片 | 一张幻灯片一页 |
| PPT | 幻灯片文字 | OfficeArt 图片 | 一张幻灯片一页 |
| PDF | 常见页面文字和基础文字状态 | 常见页面图片 | PDF 原生页面 |
| RTF | 普通文本、表格、字体表、颜色表、Unicode | PNG/JPEG 等 pict 图片 | 按布局估算的页面 |
| OFD | `TextObject` 文字、网格表格和页面对象 | MultiMedia 图片数据及位置 | OFD 原生页面 |

## 快速开始

```go
package main

import (
    "fmt"
    "log"

    fce "github.com/hekediguo2002/FileContentExtractor"
)

func main() {
    document, err := fce.Open("example.pdf")
    if err != nil {
        log.Fatal(err)
    }
    for _, page := range document.Pages {
        fmt.Printf("page %d: %s\n", page.Number, page.Text)
        for _, run := range page.Runs {
            fmt.Printf("%s %.2fpt %s (%.2f, %.2f)\n",
                run.Font, run.Size, run.Color, run.Bounds.X, run.Bounds.Y)
        }
        for _, image := range page.Images {
            fmt.Printf("image %s: %dx%d, %d bytes\n",
                image.Name, image.Width, image.Height, len(image.Data))
        }
        for _, table := range page.Tables {
            for _, row := range table.Rows {
                for _, cell := range row.Cells {
                    fmt.Printf("cell[%d,%d]: %s\n", cell.Row, cell.Column, cell.Text)
                }
            }
        }
    }
}
```

## 可选 PaddleOCR

OCR 默认关闭，原有 `Open` 行为不变。准备 PP-OCRv4 ONNX 模型和对应平台的 ONNX Runtime 后，可以识别单张图片：

```go
config := fce.OCRConfig{
    RuntimePath: "/path/to/libonnxruntime.dylib", // Windows 为 onnxruntime.dll
    ModelDir:    "/path/to/ocr_models",
}
text, err := fce.ImageToText("scan.png", config)
```

或者对文档解析器已经提取出的页面图片启用 OCR：

```go
document, err := fce.OpenWithOptions("scan.pdf", fce.Options{
    OCR: fce.OCRConfig{
        Enabled:     true,
        RuntimePath: "/path/to/libonnxruntime.dylib",
        ModelDir:    "/path/to/ocr_models",
    },
})
```

`ImageToText` 和文档 OCR 都会执行与普通文档相同的中文空格、软换行和日期归一化。PDF、DOC、DOCX、PPT、PPTX 使用同一套页面级 OCR 兜底流程：原生解析得到至少 16 个有效字母或数字时不执行 OCR；原生文字为空或明显过少时，只有 `OCR.Enabled=true` 才识别页面扫描图。页码横线、空白和标点不计入阈值。其他能够提取页面图片的格式也复用该统一流程。OCR 文字作为 `TextRun` 返回，`Font` 为 `OCR`，位置由图片像素坐标映射到页面坐标，字号为文字框高度估算值，颜色由文字框中的前景像素估算。

检测、缩放、BGR/CHW 归一化、DB 连通区域、可选 2x2 膨胀、阅读顺序和颜色估计均为纯 Go 实现，不依赖 OpenCV。识别支持按宽高比分组批处理，文档支持页面并发；CPU 实测中 ONNX Runtime 自身已经并行，批大小和页面并发默认都为 1，避免线程过量竞争。可通过 `RecognitionBatchSize` 和 `PageWorkers` 显式调优。`EnableOrientationClassification` 可按需加载第三个模型，纠正置信度达到阈值的 180 度文字框；默认关闭，不影响原有模型部署。

### 下载 OCR 资源

脚本会下载 PP-OCRv4 检测、识别、方向分类模型、字符字典以及当前平台的 ONNX Runtime，并校验所有文件的 SHA-256。方向分类模型会一起下载，但只有开启 `-ocr-cls=true` 时才加载。

macOS 或 Windows 的 Git Bash/MSYS2 中执行：

```bash
./scripts/download_ocr_assets.sh ./ocr_runtime
```

不传目录时也默认写入 `./ocr_runtime`。脚本需要 `curl` 或 `wget`、`shasum` 或 `sha256sum`；Windows 还需要 `unzip`。下载后的主要目录为：

```text
ocr_runtime/
├── models/             PP-OCRv4 ONNX 模型和字符字典
└── runtime/
    ├── lib/            macOS ONNX Runtime
    └── x64/            Windows 7 x64 ONNX Runtime 及其依赖 DLL
```

macOS 12.7.6：

```bash
go run ./cmd/extract \
  -ocr=true \
  -onnxruntime ./ocr_runtime/runtime/lib/libonnxruntime.1.19.2.dylib \
  -ocr-model-dir ./ocr_runtime/models \
  scan.pdf
```

Windows 7 x64（Git Bash）：

```bash
./extract.exe \
  -ocr=true \
  -onnxruntime ./ocr_runtime/runtime/x64/onnxruntime.dll \
  -ocr-model-dir ./ocr_runtime/models \
  scan.pdf
```

Windows 运行时必须保留 `ocr_runtime/runtime/x64` 中的全部 DLL；部署时可以把这些 DLL 一起复制到 `extract.exe` 同级目录，并相应调整 `-onnxruntime` 路径。也可以通过环境变量设置路径：

```bash
export FCE_ONNXRUNTIME_PATH="$PWD/ocr_runtime/runtime/lib/libonnxruntime.1.19.2.dylib"
export FCE_PADDLEOCR_MODEL_DIR="$PWD/ocr_runtime/models"
go run ./cmd/extract -ocr=true scan.pdf
```

模型名称、校验值以及 Windows 7/macOS 12.7.6 的详细兼容说明见 [ocr/README.md](ocr/README.md)。测试命令也支持：

```bash
go run ./cmd/extract -ocr=true \
  -onnxruntime /path/to/libonnxruntime.dylib \
  -ocr-model-dir /path/to/ocr_models \
  scan.pdf
```

需要方向分类或调优时可增加 `-ocr-cls`、`-ocr-cls-threshold`、`-ocr-batch-size`、`-ocr-workers` 和 `-ocr-dilation`。布尔参数推荐写成 `-ocr=true`、`-ocr-cls=true`，不要把 `true` 写成独立的位置参数。

`fce.Read` 是 `fce.Open` 的别名；`fce.MustRead` 在出错时 panic（按调用方需要选用）。

## Demo

`cmd/extract` 是提取文字和图片的示例程序：遍历指定目录（或单个文件），把每个文档的逐页文字和图片写到同名子目录，解析失败的文件只打印错误并继续。PNG、JPG、JPEG 可以作为独立输入，开启 OCR 后识别结果写入同名目录的 `page_1.txt`：

```bash
go run ./cmd/extract testfile    # 缺省目录为 test，不存在时退回 testfile
go run ./cmd/extract demo.docx   # 也支持单个文件
go run ./cmd/extract -ocr=true \
  -onnxruntime ./ocr_runtime/runtime/lib/libonnxruntime.1.19.2.dylib \
  -ocr-model-dir ./ocr_runtime/models \
  scan.png
```

输出形如 `demo/page_1.txt`、`demo/page_1_table_1.tsv`、`demo/image_1.png`。表格 TSV 每行对应一个表格行、使用制表符分隔单元格，并合并单元格内部的排版换行；图片扩展名按魔数嗅探真实格式（png/jpg/gif/bmp/tiff），识别不出时使用提取器给出的格式。

## 目录结构

- `model/`：统一的文档、页面、文字运行和图片模型
- `layout/`：面向 DOC/DOCX/RTF 的只读页面框、换行和分页布局
- `pdf/`：PDF 对象、对象流、页面、内容流、ToUnicode CMap、文字状态和图片解析
- `docx/`：OOXML ZIP、段落/表格内文字、运行样式、显式分页和关系图片解析
- `doc/`：OLE2/CFB、FIB、CLX/Piece Table、字符 CHPX 以及 HTML 伪 DOC 解析
- `xlsx/`：OOXML 工作表、共享字符串、单元格样式和工作表图片
- `xls/`：OLE2 BIFF 工作表、SST、单元格、字体/XF 和 OfficeArt 图片
- `pptx/`：OOXML 幻灯片、文本框、表格、运行样式和幻灯片图片
- `ppt/`：OLE2 PowerPoint 记录、幻灯片文字原子和 OfficeArt 图片
- `rtf/`：RTF 控制字、字体表、颜色表和 `\uN` Unicode 转义解析
- `ofd/`：OFD（GB/T 33190）包结构、公共资源和页面对象解析
- `ocr/`：可选 PaddleOCR ONNX 检测、识别、坐标和阅读顺序处理
- `internal/ole/`：OLE2/CFB 复合文档读取（扇区链、MiniFAT、目录条目）
- `internal/officeart/`：OfficeArt BLIP 图片扫描（jpg/png/dib/tiff）
- `cmd/extract/`：批量提取文字和图片的 demo 命令行

## 数据模型

```go
type Document struct {
    Path, Format string   // 文件路径与格式（docx/doc/xlsx/xls/pptx/ppt/pdf/rtf/ofd）
    Pagination   string   // 分页来源：native / layout-estimated / worksheet / slide / single-flow
    Pages        []Page
}

type Page struct {
    Number        int      // 页码（表格为工作表序号，演示文稿为幻灯片序号）
    Name          string   // 工作表名等
    Width, Height float64  // 页面尺寸（pt）
    Text          string   // 整页纯文本
    Runs          []TextRun
    Images        []Image
    Tables        []Table
}

type Table struct {
    Bounds Rect
    Rows   []TableRow
}

type TableRow struct {
    Cells []TableCell
}

type TableCell struct {
    Row, Column      int
    RowSpan, ColSpan int
    Bounds           Rect
    Text             string
    Runs             []TextRun
}

type TextRun struct {
    Text   string
    Bounds Rect   // X/Y/Width/Height，单位 pt；表格中为从 0 开始的列/行坐标
    Font   string
    Size   float64
    Color  string  // "#RRGGBB"
}

type Image struct {
    Name, Format  string
    Width, Height int
    Bounds        Rect
    Data          []byte  // 文件中提取的原始或解压后数据
}
```

图片的 `Data` 是文件中提取出的原始或解压后数据。JPEG/PNG 等自描述格式可直接保存；PDF Flate 原始像素流会尽量合成为 PNG，`DeviceRGB`、`DeviceGray`、`DeviceCMYK` 以及可确定通道数的 `ICCBased` 色彩空间均可参与转换，无法合成时仍是需按 ColorSpace/DecodeParms 封装的原始数据。扫描型 PDF 如果没有文字层，默认页面文字为空但仍返回页面图片；显式开启可选 PaddleOCR 后，可以从这些图片识别文字。

## 能力边界

PDF 支持常见的非加密 PDF、空用户密码的 Standard Security Handler RC4 加密 PDF、Flate 内容流、PDF 1.5 Object Stream、页面 MediaBox、`Tj`/`TJ`、ToUnicode CMap、Adobe-CNS1 与 Adobe-GB1 的部分预定义 CMap、字体/字号/颜色/基础坐标、页面引用图片，以及根据闭合横纵网格识别表格单元格。无边框表格无法仅凭 PDF 内容流可靠判断，会作为普通文字返回。暂不支持需要用户输入密码的文件、AES 加密、所有 PDF 过滤器、复杂文字变换、完整图形裁剪、Type 3 字体和无 ToUnicode 的任意自定义编码。

DOCX 支持正文及表格中的段落、`tbl/tr/tc` 单元格边界、运行级直接字体/字号/颜色、制表符/换行、显式分页符、lastRenderedPageBreak、节页面尺寸/页边距/段落间距/行距和内嵌图片。分页器按页面框进行换行和分页，并校验 `docProps/app.xml` 保存页数；明显失真的保存页数会被忽略。暂未计算完整样式表继承、复杂浮动环绕、页眉页脚和脚注。

DOC 支持 OLE2 Compound File、WordDocument、0Table/1Table、Unicode/单字节 Piece Table、`0x07` 单元格/行结束标记、字符 CHPX FKP、SPRM 字号/颜色/字体索引、SttbfFfn 字体名称表，以及通过 `sprmCPicLocation` 读取 Data stream 中的 JPEG/PNG BLIP。分页器使用文件保存页数约束页面框布局，文本框内容按锚点字符位置附加到对应页面。单字节 piece 按字节读取，非 ASCII 旧代码页没有标准库字符集转换能力。

表格同时保留在 `Page.Tables` 中；原始单元格段落和排版换行保留在 `TableCell.Text`。`Page.Text` 和 `TableTSV` 为兼容纯文本消费者，使用 `\t` 分隔单元格、使用换行分隔表格行，并合并单元格内部的排版换行。跨列单元格后会保留空字段，使各行列位置一致。

RTF 支持控制字解析、字体表/颜色表、`\uN` Unicode 转义与 `\'xx`（按 CP1252 解码）、`trowd/cell/row` 表格、`page` 及 `sect/sbkpage` 分页，并提取 `pict` 中的 PNG/JPEG 等图片。GBK 双字节序列不在支持范围内。

OFD 支持包结构校验、公共资源字体表、TextObject 的边界/字号/颜色与行聚合、根据 PathObject 闭合横纵网格识别表格单元格，以及 `DocumentRes/PublicRes` 中 MultiMedia 图片和页面 `ImageObject` 的资源关联。只提取既有文本，不做渲染或 OCR。

`Document.Pagination` 表示分页来源：PDF/OFD 为 `native`；DOC/DOCX/RTF 为 `layout-estimated`；HTML 伪 DOC 为 `single-flow`；XLS/XLSX 为 `worksheet`；PPT/PPTX 为 `slide`。DOC/DOCX/RTF 的自动分页遵循与 Writer 类似的页面框、可用内容区、行布局和溢出换页模型，但不是完整排版引擎，复杂表格、字体替换、浮动环绕和域可能造成页边界差异。

XLS/XLSX 将每个工作表作为一个 `Page`，`Page.Name` 是工作表名称，单元格运行的 `Bounds.X/Y` 是从 0 开始的列/行坐标。PPT/PPTX 将每张幻灯片作为一个 `Page`；PPTX 保留文本框坐标、字号、颜色和关系图片，旧 PPT 提取 SlideList/Persist 文字与 OfficeArt 图片。BIFF SST 跨 CONTINUE 的极端长字符串、旧代码页 TextBytes、图表内部文字和旧 PPT 的完整字体继承仍属于有限支持。

## 健壮性

解析器把输入视为不可信数据：

- 所有长度/偏移在进入切片前校验边界，越界即报错或跳过
- OLE2 扇区计数按文件实际大小钳制，防止伪造头部引发巨量分配或长时间循环
- ZIP 条目设置 512MB 解压上限（xlsx/pptx/docx/ofd），防御 zip bomb
- PDF 解压流上限 256MB，对象引用递归深度上限 1000；PPT 容器嵌套深度上限 200
- `Open` 内置 recover 兜底：即使解析器出现意外 panic，也会转换为 error 返回

## 测试

```bash
go test ./...
go build ./...
go vet ./...
```

设置 `FCE_ONNXRUNTIME_PATH` 和 `FCE_PADDLEOCR_MODEL_DIR` 后，测试还会运行真实 PaddleOCR 样本回归。`scripts/compare_paddleocr.py` 使用 Python PaddleOCR 生成逐页对照结果，仅用于开发期自测。
