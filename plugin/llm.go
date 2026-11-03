package plugin

import (
	"context"
	"strings"

	"github.com/teranos/QNTX/plugin/grpc/protocol"
	"github.com/teranos/otari-qntx/otari"
	errors "github.com/teranos/sacred-error"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// llmServer serves QNTX's LLMService on otari.
type llmServer struct {
	protocol.UnimplementedLLMServiceServer
	plugin *Plugin
}

// Chat answers a call whole.
func (s *llmServer) Chat(ctx context.Context, req *protocol.LLMChatRequest) (*protocol.LLMChatResponse, error) {
	client, err := s.ready()
	if err != nil {
		return nil, err
	}
	otariReq, err := requestFrom(req)
	if err != nil {
		return nil, err
	}
	completion, err := client.Complete(ctx, otariReq)
	if err != nil {
		return nil, statusFrom(err)
	}
	served := completion.Served
	resp := &protocol.LLMChatResponse{
		Content: completion.Content,
		Model:   answeredModel(served),
		Served:  servedReport(req, served),
	}
	if served.Usage != nil {
		resp.PromptTokens = int32(served.Usage.PromptTokens)
		resp.CompletionTokens = int32(served.Usage.CompletionTokens)
		resp.TotalTokens = int32(served.Usage.TotalTokens)
	}
	return resp, nil
}

// StreamChat answers a call as otari streams it: a chunk per piece of
// content, then a final chunk with the model, the usage and the served report.
// otari reports no per-token signal, so every chunk says so.
func (s *llmServer) StreamChat(req *protocol.LLMChatRequest, srv protocol.LLMService_StreamChatServer) error {
	client, err := s.ready()
	if err != nil {
		return err
	}
	otariReq, err := requestFrom(req)
	if err != nil {
		return err
	}
	served, err := client.Stream(srv.Context(), otariReq, func(d otari.Delta) error {
		return srv.Send(&protocol.LLMChatChunk{
			Token:    d.Content,
			SignalOf: &protocol.LLMChatChunk_NoSignal{NoSignal: true},
		})
	})
	if err != nil {
		return statusFrom(err)
	}
	final := &protocol.LLMChatChunk{
		Done:     true,
		Model:    answeredModel(*served),
		SignalOf: &protocol.LLMChatChunk_NoSignal{NoSignal: true},
		Served:   servedReport(req, *served),
	}
	if served.Usage != nil {
		final.PromptTokens = int32(served.Usage.PromptTokens)
		final.CompletionTokens = int32(served.Usage.CompletionTokens)
		final.TotalTokens = int32(served.Usage.TotalTokens)
	}
	return srv.Send(final)
}

func (s *llmServer) ready() (*otari.Client, error) {
	if s.plugin.IsPaused() {
		return nil, status.Error(codes.Unavailable, "the otari provider is paused")
	}
	if s.plugin.client == nil {
		return nil, status.Error(codes.FailedPrecondition, "the otari provider is not initialized")
	}
	return s.plugin.client, nil
}

// answeredModel is the model the call's answer is attributed to: the model
// otari sent the serving upstream (Otari-Model), which for a routing policy is
// the model that ran rather than the policy's name. When otari left that
// header out (a name it cannot carry), the body's model is all it reported.
func answeredModel(served otari.Served) string {
	if served.Model != "" {
		return served.Model
	}
	return served.ResponseModel
}

// servedReport is LLMServed: what the call asked for, as received, beside what
// otari reported. Nothing otari did not report is filled in.
func servedReport(req *protocol.LLMChatRequest, served otari.Served) *protocol.LLMServed {
	report := &protocol.LLMServed{
		RequestedProvider: req.GetProvider(),
		RequestedModel:    req.GetModel(),
		UpstreamProvider:  served.Provider,
		ExecutedModel:     served.Model,
		ResponseModel:     served.ResponseModel,
		RequestId:         served.RequestID,
		AttemptCount:      int32(served.AttemptCount),
		Fallback:          served.Fallback,
	}
	if served.Usage != nil {
		report.CostUsd = served.Usage.Cost()
		report.PricingSource = served.Usage.PricingSource
	}
	return report
}

// requestFrom is the otari request for a QNTX call. The model or routing
// policy is otari's to resolve, so it is sent as named; a call naming none is
// refused rather than given one. messages, when present, take precedence over
// the deprecated system_prompt/user_prompt (llm.proto). A zero temperature or
// max_tokens is proto3's unset and is not sent, as gaze and scry read them.
// priority is the node's queue's (LLMServer), not otari's.
func requestFrom(req *protocol.LLMChatRequest) (otari.Request, error) {
	if strings.TrimSpace(req.GetModel()) == "" {
		return otari.Request{}, status.Error(codes.InvalidArgument,
			"the call names no model: otari serves the model or routing policy a call names, and picks none itself")
	}
	var messages []otari.Message
	if len(req.GetMessages()) > 0 {
		for _, m := range req.GetMessages() {
			messages = append(messages, otari.Message{Role: m.GetRole(), Content: m.GetContent()})
		}
	} else {
		if req.GetSystemPrompt() != "" { //nolint:staticcheck // the deprecated single-turn fields are the contract's fallback
			messages = append(messages, otari.Message{Role: "system", Content: req.GetSystemPrompt()}) //nolint:staticcheck
		}
		if req.GetUserPrompt() != "" { //nolint:staticcheck
			messages = append(messages, otari.Message{Role: "user", Content: req.GetUserPrompt()}) //nolint:staticcheck
		}
	}
	if len(messages) == 0 {
		return otari.Request{}, status.Error(codes.InvalidArgument, "the call carries no messages and no prompt")
	}
	if len(req.GetAttachments()) > 0 {
		var err error
		messages, err = withAttachments(messages, req.GetAttachments())
		if err != nil {
			return otari.Request{}, err
		}
	}
	out := otari.Request{Model: req.GetModel(), Messages: messages}
	if t := req.GetTemperature(); t > 0 {
		out.Temperature = &t
	}
	if n := int(req.GetMaxTokens()); n > 0 {
		out.MaxTokens = &n
	}
	return out, nil
}

// withAttachments puts the call's attachments on its last user message, as
// OpenAI content parts otari reads: an image as image_url, anything else as a
// file part carrying the data URI and filename (otari's content normalizer
// extracts or inlines it per model). The node sends mime_type "image" or
// "file" with a data URI (internal/ai/provider/grpc_client.go).
func withAttachments(messages []otari.Message, attachments []*protocol.Attachment) ([]otari.Message, error) {
	last := -1
	for i := range messages {
		if messages[i].Role == "user" {
			last = i
		}
	}
	if last < 0 {
		return nil, status.Error(codes.InvalidArgument, "the call carries attachments and no user message to carry them")
	}
	text, _ := messages[last].Content.(string)
	parts := []any{map[string]any{"type": "text", "text": text}}
	for _, a := range attachments {
		data := a.GetData()
		if data == "" {
			continue
		}
		isImage := strings.Contains(a.GetMimeType(), "image") || strings.HasPrefix(data, "data:image")
		if isImage {
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": data}})
			continue
		}
		if !strings.HasPrefix(data, "data:") {
			return nil, status.Errorf(codes.InvalidArgument, "attachment %q is not a data URI", a.GetFilename())
		}
		file := map[string]any{"file_data": data}
		if a.GetFilename() != "" {
			file["filename"] = a.GetFilename()
		}
		parts = append(parts, map[string]any{"type": "file", "file": file})
	}
	messages[last].Content = parts
	return messages, nil
}

// statusFrom maps otari's refusals onto gRPC codes the node can tell apart:
// retry later (rate limited) from fix the request from fix the deployment.
// The message keeps otari's status, code, words and request id.
func statusFrom(err error) error {
	var oe *otari.Error
	if !errors.As(err, &oe) {
		if errors.Is(err, context.Canceled) {
			return status.Error(codes.Canceled, err.Error())
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return status.Error(codes.DeadlineExceeded, err.Error())
		}
		return status.Error(codes.Unavailable, err.Error())
	}
	code := codes.Unknown
	switch {
	case oe.Status == 400 || oe.Status == 413 || oe.Status == 422:
		code = codes.InvalidArgument
	case oe.Status == 401:
		code = codes.Unauthenticated
	case oe.Status == 403:
		code = codes.PermissionDenied
	case oe.Status == 402 || oe.Status == 424:
		code = codes.FailedPrecondition
	case oe.Status == 404:
		code = codes.NotFound
	case oe.Status == 408 || oe.Status == 504:
		code = codes.DeadlineExceeded
	case oe.Status == 429:
		code = codes.ResourceExhausted
	case oe.Status >= 500:
		code = codes.Unavailable
	case oe.Status == 200:
		// An error reported inside a stream that had begun.
		code = codes.Aborted
	}
	return status.Error(code, oe.Error())
}
