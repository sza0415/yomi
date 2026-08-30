package memory

import (
	"context"
	"testing"

	"github.com/ziangsun/szabot/internal/providers"
)

type extractorProvider struct {
	response string
}

func (p extractorProvider) Name() string { return "test" }
func (p extractorProvider) Chat(context.Context, providers.ChatRequest) (providers.ChatResponse, error) {
	return providers.ChatResponse{Content: p.response}, nil
}

func TestLLMExtractorParsesJSONArrayAndMarkdownFence(t *testing.T) {
	extractor := &LLMExtractor{Provider: extractorProvider{response: "```json\n[{\"kind\":\"preference\",\"content\":\"中文回答\",\"confidence\":0.9,\"importance\":0.8}]\n```"}, Model: "test"}
	got, err := extractor.Extract(context.Background(), ExtractionInput{UserText: "请记住我偏好中文回答"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != KindPreference || got[0].Content != "中文回答" {
		t.Fatalf("candidates = %#v", got)
	}
}

func TestLLMExtractorAcceptsWrappedJSON(t *testing.T) {
	extractor := &LLMExtractor{Provider: extractorProvider{response: `{"memories":[{"kind":"fact","content":"住在上海","confidence":0.8,"importance":0.5}]}`}, Model: "test"}
	got, err := extractor.Extract(context.Background(), ExtractionInput{UserText: "我住在上海"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "住在上海" {
		t.Fatalf("candidates = %#v", got)
	}
}
