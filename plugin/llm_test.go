package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/teranos/QNTX/plugin/grpc/protocol"
	"github.com/teranos/otari-qntx/otari"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fakeOtari answers as otari's source says it does: the Otari-* headers, the
// policy name in a routed body's model, usage with cost_usd as decimal text.
// It keeps what it was sent.
type fakeOtari struct {
	got    []map[string]any
	status int
	body   string
}

func (f *fakeOtari) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, otari.ChatPath, r.URL.Path)
		require.Equal(t, "Bearer tk-test", r.Header.Get("Authorization"))
		var req map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		f.got = append(f.got, req)
		w.Header().Set("Otari-Request-ID", "req-123")
		if f.status != 0 {
			w.Header().Set("Otari-Error-Code", "pricing_required")
			w.WriteHeader(f.status)
			io.WriteString(w, f.body)
			return
		}
		w.Header().Set("Otari-Provider", "secondary")
		w.Header().Set("Otari-Model", "good-model")
		w.Header().Set("Otari-Attempt-Count", "2")
		w.Header().Set("Otari-Fallback", "true")
		usage := `{"completion_tokens":6,"prompt_tokens":11,"total_tokens":17,"cost_usd":"0.000023","pricing_source":"deployment"}`
		if req["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, piece := range []string{"Hel", "lo"} {
				fmt.Fprintf(w, "data: {\"model\":\"fast\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", piece)
			}
			io.WriteString(w, ": keepalive\n\n")
			io.WriteString(w, "data: {\"model\":\"fast\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprintf(w, "data: {\"model\":\"fast\",\"choices\":[],\"usage\":%s}\n\n", usage)
			io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"model":"fast","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":%s}`, usage)
	})
}

func newTestPlugin(t *testing.T, f *fakeOtari) *Plugin {
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	p := New()
	p.client = otari.New(srv.URL, "tk-test", 5*time.Second)
	return p
}

func TestChat_KeepsWhatWasAskedApartFromWhatServedIt(t *testing.T) {
	f := &fakeOtari{}
	p := newTestPlugin(t, f)

	resp, err := p.llm.Chat(context.Background(), &protocol.LLMChatRequest{
		Provider: "otari", Model: "fast",
		Messages: []*protocol.ChatMessage{{Role: "system", Content: "brief"}, {Role: "user", Content: "hi"}},
	})
	require.NoError(t, err)

	assert.Equal(t, "Hello", resp.Content)
	assert.Equal(t, "good-model", resp.Model, "the answer is attributed to the model that ran, not the policy")
	assert.Equal(t, int32(11), resp.PromptTokens)
	assert.Equal(t, int32(6), resp.CompletionTokens)
	assert.Equal(t, int32(17), resp.TotalTokens)

	s := resp.Served
	require.NotNil(t, s)
	assert.Equal(t, "otari", s.RequestedProvider)
	assert.Equal(t, "fast", s.RequestedModel)
	assert.Equal(t, "secondary", s.UpstreamProvider)
	assert.Equal(t, "good-model", s.ExecutedModel)
	assert.Equal(t, "fast", s.ResponseModel)
	assert.Equal(t, "req-123", s.RequestId)
	require.NotNil(t, s.CostUsd)
	assert.Equal(t, "0.000023", *s.CostUsd)
	assert.Equal(t, "deployment", s.PricingSource)
	assert.Equal(t, int32(2), s.AttemptCount)
	assert.True(t, s.Fallback)

	// The policy is sent as named; unset temperature and max_tokens are not sent.
	require.Len(t, f.got, 1)
	assert.Equal(t, "fast", f.got[0]["model"])
	assert.Equal(t, false, f.got[0]["stream"])
	assert.NotContains(t, f.got[0], "temperature")
	assert.NotContains(t, f.got[0], "max_tokens")
	assert.Len(t, f.got[0]["messages"], 2)
}

func TestChat_TheDeprecatedSingleTurnFieldsStillWork(t *testing.T) {
	f := &fakeOtari{}
	p := newTestPlugin(t, f)

	_, err := p.llm.Chat(context.Background(), &protocol.LLMChatRequest{
		Provider: "otari", Model: "secondary:good-model", SystemPrompt: "brief", UserPrompt: "hi",
		Temperature: 0.3, MaxTokens: 64,
	})
	require.NoError(t, err)
	require.Len(t, f.got, 1)
	assert.Equal(t, []any{
		map[string]any{"role": "system", "content": "brief"},
		map[string]any{"role": "user", "content": "hi"},
	}, f.got[0]["messages"])
	assert.Equal(t, 0.3, f.got[0]["temperature"])
	assert.Equal(t, float64(64), f.got[0]["max_tokens"])
}

func TestChat_AttachmentsRideTheLastUserMessage(t *testing.T) {
	f := &fakeOtari{}
	p := newTestPlugin(t, f)

	_, err := p.llm.Chat(context.Background(), &protocol.LLMChatRequest{
		Provider: "otari", Model: "fast", UserPrompt: "what is this",
		Attachments: []*protocol.Attachment{
			{MimeType: "image", Data: "data:image/png;base64,AAAA"},
			{MimeType: "file", Data: "data:application/pdf;base64,BBBB", Filename: "doc.pdf"},
		},
	})
	require.NoError(t, err)
	msgs := f.got[0]["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	assert.Equal(t, map[string]any{"type": "text", "text": "what is this"}, content[0])
	assert.Equal(t, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}}, content[1])
	assert.Equal(t, map[string]any{"type": "file", "file": map[string]any{"file_data": "data:application/pdf;base64,BBBB", "filename": "doc.pdf"}}, content[2])
}

func TestChat_ACallNamingNoModelIsRefused(t *testing.T) {
	f := &fakeOtari{}
	p := newTestPlugin(t, f)

	_, err := p.llm.Chat(context.Background(), &protocol.LLMChatRequest{Provider: "otari", UserPrompt: "hi"})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Empty(t, f.got, "otari is not asked to pick a model")
}

func TestChat_OtarisRefusalKeepsItsStatusCodeAndRequestID(t *testing.T) {
	f := &fakeOtari{status: http.StatusPaymentRequired, body: `{"detail":"No pricing is configured for model 'fast'","code":"pricing_required"}`}
	p := newTestPlugin(t, f)

	_, err := p.llm.Chat(context.Background(), &protocol.LLMChatRequest{Provider: "otari", Model: "fast", UserPrompt: "hi"})
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "402")
	assert.Contains(t, err.Error(), "pricing_required")
	assert.Contains(t, err.Error(), "No pricing is configured")
	assert.Contains(t, err.Error(), "req-123")
}

// chunkSink is a StreamChat server stream that keeps what it is sent.
type chunkSink struct {
	grpc.ServerStream
	ctx    context.Context
	chunks []*protocol.LLMChatChunk
}

func (s *chunkSink) Send(c *protocol.LLMChatChunk) error { s.chunks = append(s.chunks, c); return nil }
func (s *chunkSink) Context() context.Context          { return s.ctx }
func (s *chunkSink) SetHeader(metadata.MD) error        { return nil }
func (s *chunkSink) SendHeader(metadata.MD) error       { return nil }
func (s *chunkSink) SetTrailer(metadata.MD)             {}

func TestStreamChat_TheFinalChunkCarriesUsageAndTheServedReport(t *testing.T) {
	f := &fakeOtari{}
	p := newTestPlugin(t, f)
	sink := &chunkSink{ctx: context.Background()}

	require.NoError(t, p.llm.StreamChat(&protocol.LLMChatRequest{Provider: "otari", Model: "fast", UserPrompt: "hi"}, sink))

	require.Len(t, sink.chunks, 3)
	text := ""
	for _, c := range sink.chunks {
		_, carried, err := c.SignalCarried()
		require.NoError(t, err, "every chunk says whether it carries a signal")
		assert.False(t, carried)
		text += c.Token
	}
	assert.Equal(t, "Hello", text)
	final := sink.chunks[2]
	assert.True(t, final.Done)
	assert.Equal(t, "good-model", final.Model)
	assert.Equal(t, int32(17), final.TotalTokens)
	require.NotNil(t, final.Served)
	assert.Equal(t, "fast", final.Served.RequestedModel)
	assert.Equal(t, "secondary", final.Served.UpstreamProvider)
	assert.Equal(t, "req-123", final.Served.RequestId)
	assert.Equal(t, "0.000023", final.Served.GetCostUsd())
	assert.Equal(t, true, f.got[0]["stream"])
	for _, c := range sink.chunks[:2] {
		assert.Nil(t, c.Served)
	}
}

func TestUsageCost_KeepsTheDecimalTextOtariWrote(t *testing.T) {
	for raw, want := range map[string]string{`"0.000023"`: "0.000023", `0.1`: "0.1", `"12.5"`: "12.5"} {
		u := &otari.Usage{CostUSD: json.RawMessage(raw)}
		require.NotNil(t, u.Cost())
		assert.Equal(t, want, *u.Cost())
	}
	assert.Nil(t, (&otari.Usage{}).Cost())
	assert.Nil(t, (&otari.Usage{CostUSD: json.RawMessage("null")}).Cost())
}
