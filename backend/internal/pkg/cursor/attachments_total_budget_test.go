//go:build unit

package cursor

import (
	"strings"
	"testing"
)

// imageMsgOfSize 构造一条带单张指定大小图片的用户消息。
// 每张都远低于 maxInlineAttachmentBytes（16MB），因此单个上限拦不住它们——
// 这正是 D 项的要害：N 张合法图片可以叠加成任意大的请求体。
func imageMsgOfSize(n int) ChatMessage {
	return ChatMessage{
		Role:   "user",
		Images: []ImageAttachment{{Data: make([]byte, n), MIMEType: "image/png"}},
	}
}

// TestCollectCurrentTurnAttachments_EnforcesAggregateBudget 是 D 项主护栏。
// 移除聚合预算会让本用例失败（15 张 * 15MB = 225MB 全部通过）。
func TestCollectCurrentTurnAttachments_EnforcesAggregateBudget(t *testing.T) {
	const each = 15 << 20 // 15MB，单个上限（16MB）以下
	var msgs []ChatMessage
	for i := 0; i < 15; i++ {
		msgs = append(msgs, imageMsgOfSize(each))
	}

	images, _ := CollectCurrentTurnAttachments(msgs)

	var total int
	for _, img := range images {
		total += len(img.Data)
	}
	if int64(total) > maxTotalAttachmentBytes {
		t.Fatalf("附件总量未受限: %d 字节 (上限 %d)", total, maxTotalAttachmentBytes)
	}
	if len(images) == len(msgs) {
		t.Fatalf("所有 %d 张图片都通过了，聚合预算没有生效", len(images))
	}
	// 预算内的附件必须保留，不能因为超限就整批丢弃。
	if len(images) == 0 {
		t.Fatal("预算内的附件被整批丢弃，应保留能放下的部分")
	}
}

// TestCollectCurrentTurnAttachments_KeepsAttachmentsWithinBudget 反向护栏：
// 防止把预算设得过低（或无条件丢弃）这种假修复通过测试。
func TestCollectCurrentTurnAttachments_KeepsAttachmentsWithinBudget(t *testing.T) {
	msgs := []ChatMessage{imageMsgOfSize(1 << 20), imageMsgOfSize(2 << 20)}

	images, _ := CollectCurrentTurnAttachments(msgs)

	if len(images) != 2 {
		t.Fatalf("预算充裕时不应丢弃附件: 期望 2 张, 实际 %d 张", len(images))
	}
	if len(images[0].Data) != 1<<20 || len(images[1].Data) != 2<<20 {
		t.Fatal("附件内容被改写")
	}
}

// TestCollectCurrentTurnAttachments_BudgetSharedAcrossImagesAndDocuments
// 确认预算是图片与文档**共用**的：分别计算等于把上限翻倍。
func TestCollectCurrentTurnAttachments_BudgetSharedAcrossImagesAndDocuments(t *testing.T) {
	bigText := strings.Repeat("x", 8<<20) // 8MB 文本
	msgs := []ChatMessage{
		{Role: "user", Images: []ImageAttachment{{Data: make([]byte, 14<<20), MIMEType: "image/png"}}},
		{Role: "user", Documents: []DocumentAttachment{
			{Text: bigText, Filename: "a.txt", MIMEType: "text/plain"},
			{Text: bigText, Filename: "b.txt", MIMEType: "text/plain"},
			{Text: bigText, Filename: "c.txt", MIMEType: "text/plain"},
		}},
	}

	images, documents := CollectCurrentTurnAttachments(msgs)

	var total int
	for _, img := range images {
		total += len(img.Data)
	}
	for _, doc := range documents {
		total += len(doc.Data) + len(doc.Text)
	}
	if int64(total) > maxTotalAttachmentBytes {
		t.Fatalf("图片与文档应共用同一预算, 合计 %d 字节超过上限 %d", total, maxTotalAttachmentBytes)
	}
}

// TestCollectCurrentTurnAttachments_DocumentTextCountsTowardBudget
// 文档正文会被拼进用户文本发往上游，必须计入预算，不能只算二进制 Data。
func TestCollectCurrentTurnAttachments_DocumentTextCountsTowardBudget(t *testing.T) {
	var msgs []ChatMessage
	for i := 0; i < 12; i++ {
		msgs = append(msgs, ChatMessage{
			Role:      "user",
			Documents: []DocumentAttachment{{Text: strings.Repeat("y", 8<<20), Filename: "f.txt", MIMEType: "text/plain"}},
		})
	}

	_, documents := CollectCurrentTurnAttachments(msgs)

	var total int
	for _, doc := range documents {
		total += len(doc.Data) + len(doc.Text)
	}
	if int64(total) > maxTotalAttachmentBytes {
		t.Fatalf("文档正文未计入预算: 合计 %d 字节 (上限 %d)", total, maxTotalAttachmentBytes)
	}
}
