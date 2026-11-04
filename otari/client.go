// Package otari is a client of otari's OpenAI-compatible chat completions
// endpoint (POST /api/v1/chat/completions), reading back what otari reports of
// how it served a call: the Otari-* response headers and the usage it settles.
//
// What it reads is what otari's source writes (mozilla-ai/otari,
// src/gateway/core/config.py and src/gateway/api/routes/_pipeline.py):
//
//	Otari-Request-ID     the gateway's id for the call; its usage record is
//	                     GET /api/v1/usage/requests/{id}
//	Otari-Provider       the provider instance that served it, by its configured name
//	Otari-Model          the model that instance was sent, without the instance prefix;
//	                     left out by otari when the name is not printable ASCII
//	Otari-Attempt-Count  candidates the call was sent to, the serving one included
//	Otari-Fallback       "true" when a candidate other than the plan's first served it
//
// and in the body, `model` (the upstream's own name for a model asked for
// directly; the policy or alias name for a routed one) and `usage`, whose
// cost_usd otari writes as decimal text, with pricing_source beside it.
package otari

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	errors "github.com/teranos/sacred-error"
)

// The response headers otari sets on a completion it served.
const (
	HeaderRequestID    = "Otari-Request-ID"
	HeaderProvider     = "Otari-Provider"
	HeaderModel        = "Otari-Model"
	HeaderAttemptCount = "Otari-Attempt-Count"
	HeaderFallback     = "Otari-Fallback"
	HeaderErrorCode    = "Otari-Error-Code"
	// HeaderKey is otari's own credential header (API_KEY_HEADER).
	HeaderKey = "Otari-Key"
)

// ChatPath is otari's chat completions route: API_ROOT (/api/v1) + /chat/completions.
const ChatPath = "/api/v1/chat/completions"

// HealthPath is otari's health route.
const HealthPath = "/api/v1/health"

// Message is one OpenAI chat message. Content is a string, or a list of
// content parts when the message carries attachments.
type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// Request is the part of otari's ChatCompletionRequest this client sends.
// A zero Temperature or MaxTokens is not sent, leaving the default to otari
// and its upstream.
type Request struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature *float64  `json:"temperature,omitempty"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
	Stream      bool      `json:"stream"`
}

// Usage is otari's usage object as it serializes it: the OpenAI counts, plus
// the cost it settled and where the price came from.
type Usage struct {
	PromptTokens     int             `json:"prompt_tokens"`
	CompletionTokens int             `json:"completion_tokens"`
	TotalTokens      int             `json:"total_tokens"`
	CostUSD          json.RawMessage `json:"cost_usd,omitempty"`
	PricingSource    string          `json:"pricing_source,omitempty"`
}

// Cost is usage.cost_usd as the decimal text otari reported, or nil when it
// reported none. otari writes it as a JSON string; a bare JSON number is kept
// as its literal text, never re-rendered through a float.
func (u *Usage) Cost() *string {
	if u == nil || len(u.CostUSD) == 0 || string(u.CostUSD) == "null" {
		return nil
	}
	var text string
	if err := json.Unmarshal(u.CostUSD, &text); err == nil {
		return &text
	}
	literal := string(u.CostUSD)
	return &literal
}

// Served is what otari reported of how it served one call.
type Served struct {
	RequestID string
	// Provider is Otari-Provider: the instance that served the call.
	Provider string
	// Model is Otari-Model: the model that instance was sent. Empty when
	// otari left the header out.
	Model string
	// ResponseModel is the body's `model`, unaltered.
	ResponseModel string
	// AttemptCount is Otari-Attempt-Count; 0 when absent.
	AttemptCount int
	Fallback     bool
	Usage        *Usage
}

func servedFromHeaders(h http.Header) Served {
	s := Served{
		RequestID: h.Get(HeaderRequestID),
		Provider:  h.Get(HeaderProvider),
		Model:     h.Get(HeaderModel),
		Fallback:  strings.EqualFold(h.Get(HeaderFallback), "true"),
	}
	if n, err := strconv.Atoi(h.Get(HeaderAttemptCount)); err == nil {
		s.AttemptCount = n
	}
	return s
}

// Completion is a whole (unstreamed) answer.
type Completion struct {
	Content string
	Served  Served
}

// Delta is one streamed piece of an answer.
type Delta struct {
	Content string
}

// Error is otari refusing or failing a call: its HTTP status, its error code
// (Otari-Error-Code, or the body's code) and message, and the request id.
type Error struct {
	Status    int
	Code      string
	Message   string
	RequestID string
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "otari answered %d", e.Status)
	if e.Code != "" {
		fmt.Fprintf(&b, " (%s)", e.Code)
	}
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, " [Otari-Request-ID %s]", e.RequestID)
	}
	return b.String()
}

// Client reaches one otari deployment with one API key.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// New is a client of the otari at baseURL. timeout bounds a whole call,
// a stream included; zero is no bound.
func New(baseURL, apiKey string, timeout time.Duration) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: timeout},
	}
}

func (c *Client) post(ctx context.Context, req Request) (*http.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, errors.Wrap(err, "encoding the otari request")
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+ChatPath, bytes.NewReader(body))
	if err != nil {
		return nil, errors.Wrapf(err, "building the request to %s", c.BaseURL+ChatPath)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, errors.Wrapf(err, "calling otari at %s", c.BaseURL+ChatPath)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, errorFrom(resp)
	}
	return resp, nil
}

// errorFrom reads otari's error body: {"detail": "...", "code": "..."} from its
// own refusals, or an OpenAI {"error": {...}} object.
func errorFrom(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &Error{
		Status:    resp.StatusCode,
		Code:      resp.Header.Get(HeaderErrorCode),
		RequestID: resp.Header.Get(HeaderRequestID),
	}
	var body struct {
		Detail json.RawMessage `json:"detail"`
		Code   string          `json:"code"`
		Error  *struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &body) == nil {
		if e.Code == "" {
			e.Code = body.Code
		}
		var detail string
		switch {
		case json.Unmarshal(body.Detail, &detail) == nil:
			e.Message = detail
		case len(body.Detail) > 0:
			e.Message = string(body.Detail)
		case body.Error != nil:
			e.Message = body.Error.Message
		}
	}
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(raw))
	}
	return e
}

type completionBody struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content *string `json:"content"`
		} `json:"message"`
		Delta struct {
			Content *string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// Complete sends req unstreamed and returns otari's answer.
func (c *Client) Complete(ctx context.Context, req Request) (*Completion, error) {
	req.Stream = false
	resp, err := c.post(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var body completionBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, errors.Wrapf(err, "decoding otari's answer [Otari-Request-ID %s]", resp.Header.Get(HeaderRequestID))
	}
	if len(body.Choices) == 0 {
		return nil, errors.Newf("otari answered with no choices [Otari-Request-ID %s]", resp.Header.Get(HeaderRequestID))
	}
	served := servedFromHeaders(resp.Header)
	served.ResponseModel = body.Model
	served.Usage = body.Usage
	content := ""
	if body.Choices[0].Message.Content != nil {
		content = *body.Choices[0].Message.Content
	}
	return &Completion{Content: content, Served: served}, nil
}

// Stream sends req streamed. onDelta is called for each piece of content as it
// arrives; the returned Served carries the headers and the usage of the
// stream's final usage chunk (otari asks its upstream for one).
func (c *Client) Stream(ctx context.Context, req Request, onDelta func(Delta) error) (*Served, error) {
	req.Stream = true
	resp, err := c.post(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	served := servedFromHeaders(resp.Header)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	done := false
	for scanner.Scan() {
		line := scanner.Text()
		// SSE: comments (otari's ": keepalive") and blank separators carry nothing.
		if line == "" || strings.HasPrefix(line, ":") || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			done = true
			break
		}
		var chunk completionBody
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return nil, errors.Wrapf(err, "decoding a chunk of otari's stream [Otari-Request-ID %s]", served.RequestID)
		}
		if chunk.Error != nil {
			return nil, &Error{Status: resp.StatusCode, Code: fmt.Sprint(chunk.Error.Code), Message: chunk.Error.Message, RequestID: served.RequestID}
		}
		if chunk.Model != "" {
			served.ResponseModel = chunk.Model
		}
		if chunk.Usage != nil {
			served.Usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != nil && *choice.Delta.Content != "" {
				if err := onDelta(Delta{Content: *choice.Delta.Content}); err != nil {
					return nil, err
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, errors.Wrapf(err, "reading otari's stream [Otari-Request-ID %s]", served.RequestID)
	}
	if !done {
		return nil, errors.Newf("otari's stream ended before [DONE] [Otari-Request-ID %s]", served.RequestID)
	}
	return &served, nil
}

// Health asks otari's health route.
func (c *Client) Health(ctx context.Context) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+HealthPath, nil)
	if err != nil {
		return errors.Wrap(err, "building the health request")
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return errors.Wrapf(err, "reaching otari at %s", c.BaseURL)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errorFrom(resp)
	}
	return nil
}
