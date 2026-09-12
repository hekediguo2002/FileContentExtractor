# FileContentExtractor

使用 Go 标准库从 **DOCX、DOC、XLSX、XLS、PPTX、PPT、PDF、RTF、OFD** 中提取文字、文字样式、位置和内嵌图片。所有格式都会转换成统一的数据模型，方便业务代码处理。

项目使用 Go 1.20，零第三方依赖，不需要安装 Office 或其他运行时组件。

> 这是面向“内容提取”的只读解析器，不是完整的 Word、PDF 或 PowerPoint 排版引擎，也不执行 OCR。复杂排版的自动分页结果可能与原软件显示不同。

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
| RTF | 普通文本、字体表、颜色表、Unicode | 不提取图片 | 按布局估算的页面 |
| OFD | `TextObject` 文字和页面对象 | 记录位置 | OFD 原生页面 |

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
    }
}
```

`fce.Read` 是 `fce.Open` 的别名；`fce.MustRead` 在出错时 panic（按调用方需要选用）。

## Demo

`cmd/extract` 是提取文字和图片的示例程序：遍历指定目录（或单个文件），把每个文档的逐页文字和图片写到同名子目录，解析失败的文件只打印错误并继续：

```bash
go run ./cmd/extract testfile    # 缺省目录为 test，不存在时退回 testfile
go run ./cmd/extract demo.docx   # 也支持单个文件
```

输出形如 `demo/page_1.txt`、`demo/image_1.png`。图片扩展名按魔数嗅探真实格式（png/jpg/gif/bmp/tiff），识别不出时使用提取器给出的格式。

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

图片的 `Data` 是文件中提取出的原始或解压后数据。JPEG/PNG 等自描述格式可直接保存；PDF Flate 原始像素流会尽量合成为 PNG，无法合成时仍是需按 ColorSpace/DecodeParms 封装的原始数据。

## 能力边界

PDF 支持常见的非加密 PDF、Flate 内容流、PDF 1.5 Object Stream、页面 MediaBox、`Tj`/`TJ`、ToUnicode CMap、字体/字号/颜色/基础坐标和页面引用图片。暂不支持加密文件、所有 PDF 过滤器、复杂文字变换、完整图形裁剪、Type 3 字体和无 ToUnicode 的任意自定义编码。

DOCX 支持正文及表格中的段落、运行级直接字体/字号/颜色、制表符/换行、显式分页符、lastRenderedPageBreak、节页面尺寸/页边距/段落间距/行距和内嵌图片。分页器按页面框进行换行和分页，并校验 `docProps/app.xml` 保存页数；明显失真的保存页数会被忽略。暂未计算完整样式表继承、复杂浮动环绕、页眉页脚和脚注。

DOC 支持 OLE2 Compound File、WordDocument、0Table/1Table、Unicode/单字节 Piece Table、字符 CHPX FKP、SPRM 字号/颜色/字体索引、SttbfFfn 字体名称表，以及通过 `sprmCPicLocation` 读取 Data stream 中的 JPEG/PNG BLIP。分页器使用文件保存页数约束页面框布局，文本框内容按锚点字符位置附加到对应页面。单字节 piece 按字节读取，非 ASCII 旧代码页没有标准库字符集转换能力。

RTF 支持控制字解析、字体表/颜色表、`\uN` Unicode 转义与 `\'xx`（按 CP1252 解码），分页为布局估算。GBK 双字节序列不在支持范围内。

OFD 支持包结构校验、公共资源字体表、TextObject 的边界/字号/颜色与行聚合。只提取既有文本，不做渲染或 OCR；图片仅记录边界位置。

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
go build ./...
go vet ./...
```
