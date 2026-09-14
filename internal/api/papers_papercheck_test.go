package api

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCheckAcademicPaperPDFAcceptsPaper(t *testing.T) {
	pdf := academicPaperPDF()
	if !isPDFBytes(pdf) {
		t.Fatal("fixture is not a PDF")
	}
	meta, err := inspectAcademicPaperPDF(pdf)
	if err != nil {
		t.Fatalf("expected academic paper to pass: %v text=%q pages=%d", err, meta.Text, meta.Pages)
	}
	if meta.Pages < 2 {
		t.Fatalf("pages=%d", meta.Pages)
	}
	if utf8.RuneCountInString(meta.Text) < paperCheckMinRunes {
		t.Fatalf("text too short: %q", meta.Text)
	}
	if !strings.Contains(strings.ToLower(meta.Title), "agent memory") &&
		!strings.Contains(strings.ToLower(meta.Text), "agent memory") {
		t.Fatalf("title/text missing paper heading: title=%q", meta.Title)
	}
}

func TestCheckAcademicPaperPDFRejectsNonPaper(t *testing.T) {
	cases := []struct {
		name string
		pdf  []byte
		code string
	}{
		{"not pdf", []byte("hello this is not a pdf file at all"), importErrNotPDF},
		{"too small", []byte("%PDF-1.4 tiny"), importErrTooSmall},
		{"slides", slideDeckPDF(), importErrNotPaper},
		{"random text pdf", helloWorldPDF(), importErrNotPaper},
		{"scanned", scannedImagePDF(), importErrScanned},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkAcademicPaperPDF(tc.pdf)
			if err == nil {
				t.Fatal("expected reject")
			}
			var check *importCheckError
			if !asImportCheck(err, &check) || check.Code != tc.code {
				t.Fatalf("code=%v err=%v want %s", check, err, tc.code)
			}
		})
	}
}

func TestCheckAcademicPaperPDFZH(t *testing.T) {
	pdf := zhAcademicPaperPDF()
	if err := checkAcademicPaperPDF(pdf); err != nil {
		t.Fatalf("ZH paper should pass: %v", err)
	}
}

func TestRequireImportTag(t *testing.T) {
	if _, err := requireImportTag(""); err == nil {
		t.Fatal("empty tag")
	}
	if _, err := requireImportTag("nope"); err == nil {
		t.Fatal("unknown tag")
	}
	got, err := requireImportTag(" Other ")
	if err != nil || got != "other" {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestTitleFromUploadName(t *testing.T) {
	if got := titleFromUploadName(`C:\Users\x\Agent_Memory_Survey.pdf`); got != "Agent Memory Survey" {
		t.Fatalf("got %q", got)
	}
	if got := titleFromUploadName("../etc/passwd.pdf"); got != "passwd" {
		t.Fatalf("traversal base: %q", got)
	}
}

func asImportCheck(err error, dest **importCheckError) bool {
	var check *importCheckError
	if !errors.As(err, &check) || check == nil {
		return false
	}
	*dest = check
	return true
}

func academicPaperPDF() []byte {
	page1 := []string{
		"A Study of Agent Memory Systems for Large Language Models",
		"Ada Lovelace and Alan Turing",
		"Abstract",
		"This paper presents a comprehensive study of memory architectures for large language model agents including retrieval modules and long-term storage.",
		"We report experiments on standard agent benchmarks and discuss failure modes of naive context stuffing.",
		"1 Introduction",
		"Large language models are increasingly deployed as autonomous agents that use tools and memory across multi-step tasks.",
		"We describe the problem setting, related systems, and the contributions of this work in detail.",
	}
	page2 := []string{
		"2 Related Work",
		"Prior work on retrieval-augmented generation and episodic agent memory is reviewed with emphasis on write and eviction policies.",
		"3 Methodology",
		"We evaluate several memory modules on tool-use and long-horizon planning tasks with controlled ablations.",
		"4 Conclusion",
		"Memory remains a central challenge for reliable LLM agents and we outline open research questions.",
		"References",
		"Smith J. 2024. Agent Memory. Journal of Artificial Intelligence Research 80 1-20.",
		"Jones A. 2023. Tool Use in Language Agents. NeurIPS.",
	}
	return buildTextPDF([][]string{page1, page2}, nil)
}

func zhAcademicPaperPDF() []byte {
	page1 := []string{
		"面向大语言模型智能体的记忆机制研究",
		"摘要",
		"本文系统研究大语言模型智能体的记忆架构，包括检索增强与长期存储，并在多个基准上给出实验。",
		"引言",
		"大语言模型正被广泛用作可调用工具的自主智能体，记忆成为关键能力。本文介绍问题定义与主要贡献。",
	}
	page2 := []string{
		"相关工作",
		"回顾检索增强生成与智能体记忆的已有方法。",
		"方法",
		"我们设计对照实验比较多种记忆模块。",
		"结论",
		"记忆仍是可靠智能体的核心挑战。",
		"参考文献",
		"张三. 2024. 智能体记忆. 人工智能学报.",
	}
	return buildTextPDF([][]string{page1, page2}, nil)
}

func slideDeckPDF() []byte {
	return buildTextPDF([][]string{{
		"Quarterly All-Hands",
		"Agenda",
		"1. Wins this quarter",
		"2. Hiring update",
		"3. Q and A",
	}}, nil)
}

func helloWorldPDF() []byte {
	return buildTextPDF([][]string{{"Hello world", "This is a random one-page note."}}, nil)
}

func scannedImagePDF() []byte {
	extra := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		extra = append(extra, "<< /Type /XObject /Subtype /Image /Width 10 /Height 10 /ColorSpace /DeviceGray /BitsPerComponent 8 /Length 0 >>\nstream\n\nendstream")
	}
	return buildTextPDF([][]string{{"Scan"}}, extra)
}

func buildTextPDF(pages [][]string, extra []string) []byte {
	if len(pages) == 0 {
		pages = [][]string{{"Empty"}}
	}
	n := len(pages)
	pageIDs := make([]int, n)
	contentIDs := make([]int, n)
	for i := 0; i < n; i++ {
		pageIDs[i] = 4 + i
		contentIDs[i] = 4 + n + i
	}
	maxID := 4 + 2*n - 1 + len(extra)
	objs := make([]string, maxID+1)
	kids := make([]string, 0, n)
	for _, id := range pageIDs {
		kids = append(kids, fmt.Sprintf("%d 0 R", id))
	}
	objs[1] = "<< /Type /Catalog /Pages 2 0 R >>"
	objs[2] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n)
	objs[3] = "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"
	for i := 0; i < n; i++ {
		objs[pageIDs[i]] = fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 3 0 R >> >> >>",
			contentIDs[i])
		stream := pageContentStream(pages[i])
		objs[contentIDs[i]] = fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream)
	}
	for i, e := range extra {
		objs[4+2*n+i] = e
	}
	return assemblePDF(objs)
}

func pageContentStream(lines []string) string {
	var b strings.Builder
	b.WriteString("BT\n/F1 12 Tf\n72 720 Td\n")
	for i, line := range lines {
		if i > 0 {
			b.WriteString("0 -16 Td\n")
		}
		fmt.Fprintf(&b, "(%s) Tj\n", pdfEscape(line))
	}
	b.WriteString("ET\n")
	return b.String()
}

func pdfEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `(`, `\(`)
	s = strings.ReplaceAll(s, `)`, `\)`)
	return s
}

func assemblePDF(objects []string) []byte {
	var buf strings.Builder
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i := 1; i < len(objects); i++ {
		if objects[i] == "" {
			continue
		}
		offsets[i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i, objects[i])
	}
	startxref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(objects))
	buf.WriteString("0000000000 65535 f \n")
	for i := 1; i < len(objects); i++ {
		if objects[i] == "" {
			buf.WriteString("0000000000 65535 f \n")
			continue
		}
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects), startxref)
	return []byte(buf.String())
}
