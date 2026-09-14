package kiro

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 真实上游回归：source.type="text" 的 document 块此前被静默丢弃 ——
// buildDocumentTextFallback 对一切非 pdf 格式 return ""，
// 导致上游收不到文档内容，但请求照常 200，模型自信地答「没看到相关信息」。
func TestDocumentTextSourceIsInlined(t *testing.T) {
	part := gjson.Parse(`{
		"type": "document",
		"source": {"type": "text", "media_type": "text/plain",
		           "data": "Project PHOENIX. Owner: Li Wei. Budget: 42000 USD."}
	}`)

	got := buildDocumentTextFallback(part)

	require.NotEmpty(t, got, "text/plain document 块不能被丢弃")
	require.Contains(t, got, "Li Wei", "文档正文必须进入负载")
	require.Contains(t, got, "42000", "文档正文必须进入负载")
}

// base64 编码的文本文档同样要内联（source.type 省略时的常见形态）。
func TestDocumentBase64TextSourceIsInlined(t *testing.T) {
	raw := "invoice_id,amount\nINV-7742,980\n"
	part := gjson.Parse(`{
		"type": "document",
		"name": "invoices.csv",
		"source": {"type": "base64", "media_type": "text/csv",
		           "data": "` + base64.StdEncoding.EncodeToString([]byte(raw)) + `"}
	}`)

	got := buildDocumentTextFallback(part)

	require.Contains(t, got, "INV-7742")
	require.Contains(t, got, "invoices.csv", "文件名应出现在附件描述里")
	require.Contains(t, got, "format=csv")
}

// 超长文本必须截断，避免单个文档把负载顶过体积守卫阈值。
func TestDocumentTextTruncatedWhenTooLong(t *testing.T) {
	long := strings.Repeat("A", 20000)
	part := gjson.Parse(`{
		"type": "document",
		"source": {"type": "text", "media_type": "text/plain", "data": "` + long + `"}
	}`)

	got := buildDocumentTextFallback(part)

	require.Contains(t, got, "[document text truncated]")
	require.Less(t, len(got), 20000, "超长文档必须被截断")
}

// 二进制内容（非合法 UTF-8）不应被当成文本内联，避免污染负载。
func TestDocumentBinaryNonUTF8Rejected(t *testing.T) {
	bin := base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe, 0x00, 0x01, 0x02})
	part := gjson.Parse(`{
		"type": "document",
		"source": {"type": "base64", "media_type": "application/msword", "data": "` + bin + `"}
	}`)

	require.Empty(t, buildDocumentTextFallback(part), "非 UTF-8 二进制不应内联为文本")
}

// PDF 原有路径不能被回归破坏。
func TestDocumentPDFPathUnchanged(t *testing.T) {
	pdf := []byte("%PDF-1.4\n4 0 obj<</Length 60>>stream\n" +
		"BT /F1 14 Tf 20 50 Td (SECRET CODE: ZEBRA-9931) Tj ET\nendstream endobj\n%%EOF")
	part := gjson.Parse(`{
		"type": "document",
		"source": {"type": "base64", "media_type": "application/pdf",
		           "data": "` + base64.StdEncoding.EncodeToString(pdf) + `"}
	}`)

	got := buildDocumentTextFallback(part)

	require.Contains(t, got, "Attached PDF document", "PDF 仍走原有抽取路径")
	require.Contains(t, got, "ZEBRA-9931")
}
