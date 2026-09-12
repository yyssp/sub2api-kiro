package cursor

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

//   - TestResponsesInputAttachmentsArePreserved（依赖未移植符号 oaiToChat）

//   - TestAnthropicToChatPreservesTopLevelAndToolResultAttachments（依赖未移植符号 antReq）

func testPNG100x100() []byte {
	// 1x1 PNG is sufficient for wire tests; real CLI coverage uses the 100x100 fixture.
	return []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
		0xde,
	}
}

func testPDF(text string) []byte {
	return []byte("%PDF-1.4\n1 0 obj\n<<>>\nstream\nBT (" + text + ") Tj ET\nendstream\nendobj\n")
}

func testOfficeXML(files map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			panic(err)
		}
		_, _ = io.WriteString(w, content)
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func TestRawToStringAndAttachmentsParsesImageAndPDF(t *testing.T) {
	png := testPNG100x100()
	pdf := testPDF("PDF-42")
	raw, err := json.Marshal([]map[string]interface{}{
		{"type": "text", "text": "inspect"},
		{"type": "image", "source": map[string]interface{}{
			"type": "base64", "media_type": "image/png",
			"data": base64.StdEncoding.EncodeToString(png),
		}},
		{"type": "document", "source": map[string]interface{}{
			"type": "base64", "media_type": "application/pdf",
			"data": base64.StdEncoding.EncodeToString(pdf),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	text, images, documents := rawToStringAndAttachments(raw)
	if text != "inspect" {
		t.Fatalf("text=%q, want inspect", text)
	}
	if len(images) != 1 || !bytes.Equal(images[0].Data, png) {
		t.Fatalf("images=%d or image data mismatch", len(images))
	}
	if images[0].MIMEType != "image/png" {
		t.Fatalf("image mime=%q", images[0].MIMEType)
	}
	if len(documents) != 1 || !documents[0].IsPDF {
		t.Fatalf("documents=%+v, want one PDF", documents)
	}
	if !strings.Contains(documents[0].Text, "PDF-42") {
		t.Fatalf("pdf text=%q, want PDF-42", documents[0].Text)
	}
}

func TestDocumentAttachmentInfersCommonTextFilesWithoutMIME(t *testing.T) {
	raw, err := json.Marshal([]map[string]interface{}{{
		"type":     "file",
		"filename": "notes.md",
		"source": map[string]interface{}{
			"type": "base64",
			"data": base64.StdEncoding.EncodeToString([]byte("# AI2API-TEXT-42\n")),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, _, documents := rawToStringAndAttachments(raw)
	if len(documents) != 1 {
		t.Fatalf("documents=%d, want 1", len(documents))
	}
	if !strings.Contains(documents[0].Text, "AI2API-TEXT-42") {
		t.Fatalf("text=%q, want marker", documents[0].Text)
	}
}

func TestDocumentAttachmentSupportsDataURLAndCommonCodeFiles(t *testing.T) {
	dataURL := "data:text/plain;base64," + base64.StdEncoding.EncodeToString([]byte("AI2API-DATA-URL-42"))
	doc, ok := documentAttachmentFromMap(map[string]interface{}{
		"type":      "file",
		"filename":  "script.ts",
		"file_data": dataURL,
	})
	if !ok || doc.Text != "AI2API-DATA-URL-42" {
		t.Fatalf("data URL file text=%q ok=%v", doc.Text, ok)
	}

	for _, name := range []string{"config.yaml", "query.sql", "main.go", "view.vue", "notebook.ipynb"} {
		doc, ok := documentAttachmentFromMap(map[string]interface{}{
			"type":     "file",
			"filename": name,
			"source": map[string]interface{}{
				"type":       "base64",
				"media_type": "application/octet-stream",
				"data":       base64.StdEncoding.EncodeToString([]byte("AI2API-CODE-42")),
			},
		})
		if !ok || doc.Text != "AI2API-CODE-42" {
			t.Fatalf("%s was not extracted as text: text=%q ok=%v", name, doc.Text, ok)
		}
	}
}

func TestImageAttachmentSupportsOpenAIStringImageURL(t *testing.T) {
	raw := "data:image/png;base64," + base64.StdEncoding.EncodeToString(testPNG100x100())
	image, ok := imageAttachmentFromMap(map[string]interface{}{
		"type":      "image_url",
		"image_url": raw,
	})
	if !ok || len(image.Data) == 0 || image.MIMEType != "image/png" {
		t.Fatalf("string image_url decode failed: ok=%v bytes=%d mime=%q", ok, len(image.Data), image.MIMEType)
	}
}

func TestAttachmentsInferMIMEFromFilenameAndRawURLBase64(t *testing.T) {
	webp := []byte("RIFF0000WEBP")
	image, ok := imageAttachmentFromMap(map[string]interface{}{
		"type":     "image",
		"filename": "photo.webp",
		"source": map[string]interface{}{
			"type": "base64",
			"data": base64.RawURLEncoding.EncodeToString(webp),
		},
	})
	if !ok || image.MIMEType != "image/webp" || !bytes.Equal(image.Data, webp) {
		t.Fatalf("filename MIME inference failed: ok=%v mime=%q bytes=%q", ok, image.MIMEType, image.Data)
	}

	pdf := testPDF("PDF-NO-MIME-42")
	doc, ok := documentAttachmentFromMap(map[string]interface{}{
		"type":     "document",
		"filename": "report.pdf",
		"source": map[string]interface{}{
			"type": "base64",
			"data": base64.RawURLEncoding.EncodeToString(pdf),
		},
	})
	if !ok || doc.MIMEType != "application/pdf" || !doc.IsPDF || !strings.Contains(doc.Text, "PDF-NO-MIME-42") {
		t.Fatalf("PDF filename/raw-url inference failed: ok=%v mime=%q isPDF=%v text=%q", ok, doc.MIMEType, doc.IsPDF, doc.Text)
	}
}

func TestImageAttachmentPromotesGenericMIMEFromFilenameAndMagic(t *testing.T) {
	png := testPNG100x100()
	image, ok := imageAttachmentFromMap(map[string]interface{}{
		"type":     "image",
		"filename": "photo.png",
		"source": map[string]interface{}{
			"type":       "base64",
			"media_type": "application/octet-stream",
			"data":       base64.StdEncoding.EncodeToString(png),
		},
	})
	if !ok || image.MIMEType != "image/png" || !bytes.Equal(image.Data, png) {
		t.Fatalf("generic MIME was not promoted from filename: ok=%v mime=%q bytes=%d", ok, image.MIMEType, len(image.Data))
	}

	webp := append([]byte("RIFF0000"), []byte("WEBPpayload")...)
	image, ok = imageAttachmentFromMap(map[string]interface{}{
		"type": "image",
		"source": map[string]interface{}{
			"type":       "base64",
			"media_type": "application/octet-stream",
			"data":       base64.StdEncoding.EncodeToString(webp),
		},
	})
	if !ok || image.MIMEType != "image/webp" || !bytes.Equal(image.Data, webp) {
		t.Fatalf("generic MIME was not promoted from WebP magic: ok=%v mime=%q bytes=%q", ok, image.MIMEType, image.Data)
	}
}

func TestDocumentAttachmentSupportsPercentEncodedDataURL(t *testing.T) {
	doc, ok := documentAttachmentFromMap(map[string]interface{}{
		"type":      "file",
		"filename":  "note.txt",
		"file_data": "data:text/plain,hello%20AI2API%20%F0%9F%8C%8D",
	})
	if !ok || doc.Text != "hello AI2API \U0001f30d" {
		t.Fatalf("percent-encoded data URL was not decoded: ok=%v mime=%q text=%q", ok, doc.MIMEType, doc.Text)
	}
}

func TestOpenAINestedFileAttachment(t *testing.T) {
	raw, err := json.Marshal([]map[string]interface{}{{
		"type": "file",
		"file": map[string]interface{}{
			"filename":  "note.txt",
			"file_data": "data:text/plain;base64," + base64.StdEncoding.EncodeToString([]byte("NESTED-FILE-42")),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, _, docs := rawToStringAndAttachments(raw)
	if len(docs) != 1 || docs[0].Text != "NESTED-FILE-42" {
		t.Fatalf("nested OpenAI file not parsed: %+v", docs)
	}
}

func TestOfficeXMLAttachmentExtractionForDocxXlsxAndPptx(t *testing.T) {
	fixtures := map[string]struct {
		name  string
		entry string
		xml   string
		want  string
	}{
		"docx": {name: "sample.docx", entry: "word/document.xml", xml: "<w:document><w:t>DOCX-42</w:t></w:document>", want: "DOCX-42"},
		"xlsx": {name: "sample.xlsx", entry: "xl/worksheets/sheet1.xml", xml: "<worksheet><row><c><is><t>XLSX-42</t></is></c></row></worksheet>", want: "XLSX-42"},
		"pptx": {name: "sample.pptx", entry: "ppt/slides/slide1.xml", xml: "<p:sld><a:t>PPTX-42</a:t></p:sld>", want: "PPTX-42"},
	}
	for label, fx := range fixtures {
		doc, ok := documentAttachmentFromMap(map[string]interface{}{
			"type":     "file",
			"filename": fx.name,
			"source": map[string]interface{}{
				"type":       "base64",
				"media_type": "application/octet-stream",
				"data":       base64.StdEncoding.EncodeToString(testOfficeXML(map[string]string{fx.entry: fx.xml})),
			},
		})
		if !ok || !strings.Contains(doc.Text, fx.want) {
			t.Fatalf("%s office XML not extracted: ok=%v text=%q want=%q", label, ok, doc.Text, fx.want)
		}
	}
}

func TestBuildAgentClientMessageEncodesImagesPDFAndTextFile(t *testing.T) {
	png := testPNG100x100()
	in := AgentRequest{
		Model:   "claude-sonnet",
		Message: "inspect attachments",
		Images: []ImageAttachment{{
			Data: png, MIMEType: "image/png", Width: 1, Height: 1,
			UUID: "img-1", Path: "fixture.png",
		}},
		Documents: []DocumentAttachment{
			{Text: "plain text body", MIMEType: "text/plain", Filename: "fixture.txt"},
			{Text: "PDF-42", MIMEType: "application/pdf", Filename: "fixture.pdf", IsPDF: true, Data: testPDF("PDF-42")},
		},
	}
	wire := buildAgentClientMessage(in.Model, in)
	outer, err := pbParse(wire)
	if err != nil {
		t.Fatal(err)
	}
	run, ok := pbFirst(outer, 1)
	if !ok {
		t.Fatal("missing RunAgentRequest")
	}
	runParts, _ := pbParse(run.Data)
	action, ok := pbFirst(runParts, 2)
	if !ok {
		t.Fatal("missing action")
	}
	actionParts, _ := pbParse(action.Data)
	userAction, ok := pbFirst(actionParts, 1)
	if !ok {
		t.Fatal("missing UserMessageAction")
	}
	userActionParts, _ := pbParse(userAction.Data)
	user, ok := pbFirst(userActionParts, 1)
	if !ok {
		t.Fatal("missing UserMessage")
	}
	userParts, _ := pbParse(user.Data)
	selected, ok := pbFirst(userParts, 3)
	if !ok {
		t.Fatal("missing selected_context")
	}
	selectedParts, _ := pbParse(selected.Data)
	imagePart, ok := pbFirst(selectedParts, 1)
	if !ok {
		t.Fatal("missing selected image")
	}
	imageParts, _ := pbParse(imagePart.Data)
	imageData, ok := pbFirst(imageParts, 8)
	if !ok || !bytes.Equal(imageData.Data, png) {
		t.Fatal("selected image data missing or changed")
	}
	if _, ok := pbFirst(imageParts, 1); ok {
		t.Fatal("selected image unexpectedly uses blob_id field 1")
	}
	filePart, ok := pbFirst(selectedParts, 4)
	if !ok {
		t.Fatal("missing selected file")
	}
	fileParts, _ := pbParse(filePart.Data)
	fileText, ok := pbFirst(fileParts, 1)
	if !ok || string(fileText.Data) != "plain text body" {
		t.Fatalf("selected file text=%q", fileText.Data)
	}
	pdfPart, ok := pbFirst(selectedParts, 9)
	if !ok {
		t.Fatal("missing selected external PDF link")
	}
	pdfParts, _ := pbParse(pdfPart.Data)
	pdfText, ok := pbFirst(pdfParts, 3)
	if !ok || string(pdfText.Data) != "PDF-42" {
		t.Fatalf("selected pdf text=%q", pdfText.Data)
	}
	pdfFlag, ok := pbFirst(pdfParts, 4)
	if !ok || pdfFlag.Value != 1 {
		t.Fatalf("selected pdf is_pdf flag=%v", pdfFlag.Value)
	}
	userText, ok := pbFirst(userParts, 1)
	if !ok || !strings.Contains(string(userText.Data), "plain text body") || !strings.Contains(string(userText.Data), "PDF-42") {
		t.Fatalf("document text is not visible in user text: %q", userText.Data)
	}
}

func TestBuildAgentMessageMakesDocumentTextVisibleToUpstream(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "assistant", Content: "previous answer", Documents: []DocumentAttachment{{Text: "OLD-MARKER", Filename: "old.txt"}}},
		{Role: "user", Content: "inspect this", Documents: []DocumentAttachment{{Text: "CURRENT-MARKER", Filename: "current.txt"}}},
	}
	got := buildAgentMessageForTools(msgs, nil)
	if !strings.Contains(got, "CURRENT-MARKER") {
		t.Fatalf("current document text is not visible in user message: %q", got)
	}
	if strings.Contains(got, "OLD-MARKER") {
		t.Fatalf("historical document text was replayed into current prompt: %q", got)
	}
	doc := msgs[1].Documents
	context := renderDocumentContext(doc)
	if gotAgain := appendDocumentContext(got, doc); gotAgain != got || strings.Count(gotAgain, context) != 1 {
		t.Fatalf("document context was duplicated: %q", gotAgain)
	}
}

func TestCurrentTurnAttachmentsExcludeHistoricalMessages(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: "old", Images: []ImageAttachment{{Data: []byte("old")}}, Documents: []DocumentAttachment{{Text: "OLD"}}},
		{Role: "assistant", Content: "old answer"},
		{Role: "user", Content: "new", Images: []ImageAttachment{{Data: []byte("new")}}, Documents: []DocumentAttachment{{Text: "NEW"}}},
	}
	images := collectChatImages(msgs)
	documents := collectChatDocuments(msgs)
	if len(images) != 1 || string(images[0].Data) != "new" {
		t.Fatalf("images=%+v, want only current turn", images)
	}
	if len(documents) != 1 || documents[0].Text != "NEW" {
		t.Fatalf("documents=%+v, want only current turn", documents)
	}
}

func TestNonAttachmentMessageHasNoSelectedContext(t *testing.T) {
	wire := buildAgentClientMessage("claude-sonnet", AgentRequest{Message: "plain"})
	outer, err := pbParse(wire)
	if err != nil {
		t.Fatal(err)
	}
	run, _ := pbFirst(outer, 1)
	runParts, _ := pbParse(run.Data)
	action, _ := pbFirst(runParts, 2)
	actionParts, _ := pbParse(action.Data)
	userAction, _ := pbFirst(actionParts, 1)
	userActionParts, _ := pbParse(userAction.Data)
	user, _ := pbFirst(userActionParts, 1)
	userParts, _ := pbParse(user.Data)
	if _, ok := pbFirst(userParts, 3); ok {
		t.Fatal("plain message unexpectedly contains selected_context")
	}
}

func TestBuildAgentClientMessageOmitsUnsupportedCustomSystemPrompt(t *testing.T) {
	const system = "SYSTEM-MARKER: must remain gateway-only"
	wire := buildAgentClientMessage("claude-sonnet", AgentRequest{
		System:  system,
		Message: "USER-MARKER: answer directly",
	})
	outer, err := pbParse(wire)
	if err != nil {
		t.Fatal(err)
	}
	run, ok := pbFirst(outer, 1)
	if !ok {
		t.Fatal("missing RunAgentRequest")
	}
	runParts, err := pbParse(run.Data)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pbFirst(runParts, 8); ok {
		t.Fatal("普通 Cursor 请求不应发送不兼容的 custom_system_prompt(field 8)")
	}
	mcpTools, ok := pbFirst(runParts, 4)
	if !ok || mcpTools.Wire != 2 {
		t.Fatal("missing explicit empty mcp_tools wrapper")
	}
	if len(mcpTools.Data) != 0 {
		t.Fatalf("empty mcp_tools wrapper unexpectedly contains data: %x", mcpTools.Data)
	}
	action, ok := pbFirst(runParts, 2)
	if !ok {
		t.Fatal("missing action")
	}
	actionParts, _ := pbParse(action.Data)
	userAction, ok := pbFirst(actionParts, 1)
	if !ok {
		t.Fatal("missing user action")
	}
	userActionParts, _ := pbParse(userAction.Data)
	user, ok := pbFirst(userActionParts, 1)
	if !ok {
		t.Fatal("missing user message")
	}
	userParts, _ := pbParse(user.Data)
	userText, ok := pbFirst(userParts, 1)
	if !ok || string(userText.Data) != "USER-MARKER: answer directly" {
		t.Fatalf("user content was mixed with system prompt: %q", userText.Data)
	}
}

func TestBuildAgentClientMessageUsesRawAgentToolName(t *testing.T) {
	wire := buildAgentClientMessage("claude-sonnet", AgentRequest{
		Tools: []ToolDef{{
			Name:        "Agent",
			Description: "delegate a subtask",
			InputSchema: `{"type":"object","properties":{"prompt":{"type":"string"}},"required":["prompt"]}`,
		}},
	})
	outer, err := pbParse(wire)
	if err != nil {
		t.Fatal(err)
	}
	run, ok := pbFirst(outer, 1)
	if !ok {
		t.Fatal("missing RunAgentRequest")
	}
	runParts, err := pbParse(run.Data)
	if err != nil {
		t.Fatal(err)
	}
	toolWrapper, ok := pbFirst(runParts, 4)
	if !ok {
		t.Fatal("missing tool definition")
	}
	toolWrapperParts, err := pbParse(toolWrapper.Data)
	if err != nil {
		t.Fatal(err)
	}
	toolDef, ok := pbFirst(toolWrapperParts, 1)
	if !ok {
		t.Fatal("missing tool def payload")
	}
	toolDefParts, err := pbParse(toolDef.Data)
	if err != nil {
		t.Fatal(err)
	}
	namePart, ok := pbFirst(toolDefParts, 1)
	if !ok {
		t.Fatal("missing tool wire name")
	}
	if got := string(namePart.Data); got != "Agent" {
		t.Fatalf("tool wire name=%q, want Agent", got)
	}
}

func TestBuildAgentClientMessageEncodesMCPInputSchemaAsProtoValue(t *testing.T) {
	wire := buildAgentClientMessage("claude-fable-5", AgentRequest{
		Tools: []ToolDef{{
			Name:        "Read",
			Description: "read a file",
			InputSchema: `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`,
		}},
	})
	outer, err := pbParse(wire)
	if err != nil {
		t.Fatal(err)
	}
	run, ok := pbFirst(outer, 1)
	if !ok {
		t.Fatal("missing RunAgentRequest")
	}
	runParts, err := pbParse(run.Data)
	if err != nil {
		t.Fatal(err)
	}
	wrapper, ok := pbFirst(runParts, 4)
	if !ok {
		t.Fatal("missing mcp_tools wrapper")
	}
	wrapperParts, err := pbParse(wrapper.Data)
	if err != nil {
		t.Fatal(err)
	}
	def, ok := pbFirst(wrapperParts, 1)
	if !ok {
		t.Fatal("missing MCP tool definition")
	}
	defParts, err := pbParse(def.Data)
	if err != nil {
		t.Fatal(err)
	}
	provider, ok := pbFirst(defParts, 4)
	if !ok || string(provider.Data) != "claude-local" {
		t.Fatalf("provider=%q, want claude-local", provider.Data)
	}
	schema, ok := pbFirst(defParts, 3)
	if !ok || schema.Wire != 2 {
		t.Fatal("missing input_schema Value at field 3")
	}
	if _, ok := pbFirst(defParts, 6); ok {
		t.Fatal("input_schema must not be encoded as legacy field 6 JSON string")
	}
	valueParts, err := pbParse(schema.Data)
	if err != nil {
		t.Fatal(err)
	}
	structValue, ok := pbFirst(valueParts, 5)
	if !ok {
		t.Fatal("input_schema Value must use struct_value field 5")
	}
	structParts, err := pbParse(structValue.Data)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := pbFirst(structParts, 1)
	if !ok {
		t.Fatal("struct_value must contain map entry")
	}
	entryParts, err := pbParse(entry.Data)
	if err != nil {
		t.Fatal(err)
	}
	key, ok := pbFirst(entryParts, 1)
	if !ok || string(key.Data) != "properties" {
		t.Fatalf("first schema map key=%q, want properties", key.Data)
	}
}
