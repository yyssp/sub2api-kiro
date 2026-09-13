//go:build unit

package cursor

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"strings"
	"testing"
)

// gzipBombFrame 构造一个「压缩后很小、解压后极大」的 Connect-RPC 帧。
// 返回的帧压缩后远低于 50MB 帧长闸门，因此闸门放行，解压才是真正的风险点。
func gzipBombFrame(t *testing.T, decompressedSize int) []byte {
	t.Helper()
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	// 全零负载的压缩比极高，模拟 protobuf/JSON 这类高重复内容的最坏情况。
	if _, err := io.Copy(zw, io.LimitReader(zeroReader{}, int64(decompressedSize))); err != nil {
		t.Fatalf("压缩失败: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭 gzip writer 失败: %v", err)
	}
	payload := compressed.Bytes()

	frame := make([]byte, 5, 5+len(payload))
	frame[0] = 0x01 // gzip 标志位
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	return append(frame, payload...)
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// TestReadFrame_RejectsGzipBomb 是 A 项的主回归护栏。
// 还原改动前的 io.ReadAll(zr) 会让本用例失败（解压出 100MB 且 err == nil）。
func TestReadFrame_RejectsGzipBomb(t *testing.T) {
	// 必须严格超过 maxDecompressedFrameSize，否则用例会「因为没超限而通过」，
	// 变成一条永远绿的假护栏。
	decompressed := int(maxDecompressedFrameSize) + (16 << 20)
	frame := gzipBombFrame(t, decompressed)

	if len(frame) > 50*1024*1024 {
		t.Fatalf("前提不成立: 构造的帧 %d 字节已被帧长闸门拦下，测不到解压路径", len(frame))
	}

	sr := NewStreamReader(io.NopCloser(bytes.NewReader(frame)))
	_, payload, err := sr.ReadFrame()

	if err == nil {
		t.Fatalf("gzip 炸弹未被拦截: 压缩后 %d 字节解压出 %d 字节且未报错", len(frame), len(payload))
	}
	if !strings.Contains(err.Error(), "解压") {
		t.Fatalf("错误信息应指明解压超限, 实际: %v", err)
	}
	// 触顶必须报错，不能返回「截断但成功」的半截 payload 给解析器。
	if len(payload) != 0 {
		t.Fatalf("解压超限时不应返回 payload, 实际返回 %d 字节", len(payload))
	}
}

// TestReadFrame_AcceptsNormalGzipFrame 是反向护栏：
// 防止「把上限设得过低/直接拒绝所有压缩帧」这种假修复通过测试。
func TestReadFrame_AcceptsNormalGzipFrame(t *testing.T) {
	original := []byte(strings.Repeat("cursor agent.v1 payload ", 4096)) // ~96KB
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(original); err != nil {
		t.Fatalf("压缩失败: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭 gzip writer 失败: %v", err)
	}

	frame := make([]byte, 5, 5+compressed.Len())
	frame[0] = 0x01 | 0x02 // gzip + end-of-stream: 标志位是位域, 不是枚举
	binary.BigEndian.PutUint32(frame[1:5], uint32(compressed.Len()))
	frame = append(frame, compressed.Bytes()...)

	sr := NewStreamReader(io.NopCloser(bytes.NewReader(frame)))
	flag, payload, err := sr.ReadFrame()
	if err != nil {
		t.Fatalf("正常压缩帧不应报错: %v", err)
	}
	if !bytes.Equal(payload, original) {
		t.Fatalf("解压内容不一致: 期望 %d 字节, 实际 %d 字节", len(original), len(payload))
	}
	if flag&0x01 != 0 {
		t.Fatalf("解压后应清除 gzip 标志位, 实际 flag=%#x", flag)
	}
	if flag&0x02 == 0 {
		t.Fatalf("end-of-stream 标志位不应被解压逻辑抹掉, 实际 flag=%#x", flag)
	}
}
