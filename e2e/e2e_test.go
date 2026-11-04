//go:build e2e

// Package e2e drives the built plugin binary the way the node does — the
// DomainPluginService Initialize the node sends, then LLMService Chat and
// StreamChat — against a real otari, and checks what comes back against what
// otari itself recorded for the call (GET /api/v1/usage/requests/{id}).
//
// Environment:
//
//	OTARI_E2E_PLUGIN_BIN  the qntx-otari-plugin binary
//	OTARI_E2E_URL         where otari serves
//	OTARI_E2E_KEY_FILE    a file holding an otari API key
//	OTARI_E2E_POLICY      a routing policy whose first candidate fails, falling
//	                      over to OTARI_E2E_PROVIDER:OTARI_E2E_MODEL
//	OTARI_E2E_PROVIDER    the instance that serves the policy's fallback
//	OTARI_E2E_MODEL       the model it is sent
//	OTARI_E2E_COST        the cost otari settles for one call (decimal text)
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/teranos/QNTX/plugin/grpc/protocol"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func env(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s is not set", name)
	}
	return v
}

// startPlugin runs the binary as the node does and dials the port it says.
func startPlugin(t *testing.T) *grpc.ClientConn {
	bin := env(t, "OTARI_E2E_PLUGIN_BIN")
	cmd := exec.Command(bin, "--port", "50140", "--log-level", "debug")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	ports := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			fmt.Fprintln(os.Stderr, "plugin:", line)
			if port, ok := strings.CutPrefix(line, "QNTX_PLUGIN_PORT="); ok {
				ports <- port
			}
		}
	}()
	var port string
	select {
	case port = <-ports:
	case <-time.After(30 * time.Second):
		t.Fatal("the plugin said no port")
	}
	conn, err := grpc.NewClient("127.0.0.1:"+port, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return conn
}

type usageRecord struct {
	RequestID        string `json:"request_id"`
	Status           string `json:"status"`
	CostUSD          string `json:"cost_usd"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
}

// otariUsage is what otari itself recorded for one request id.
func otariUsage(t *testing.T, requestID string) usageRecord {
	key, err := os.ReadFile(env(t, "OTARI_E2E_KEY_FILE"))
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodGet, env(t, "OTARI_E2E_URL")+"/api/v1/usage/requests/"+requestID, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(key)))
	var rec usageRecord
	require.Eventually(t, func() bool {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		raw, _ := io.ReadAll(resp.Body)
		return json.Unmarshal(raw, &rec) == nil && rec.RequestID == requestID
	}, 10*time.Second, 100*time.Millisecond, "otari has a usage record for %s", requestID)
	return rec
}

func TestE2E_TheNodesCallsThroughThePluginToOtari(t *testing.T) {
	policy := env(t, "OTARI_E2E_POLICY")
	upstream := env(t, "OTARI_E2E_PROVIDER")
	model := env(t, "OTARI_E2E_MODEL")
	cost := env(t, "OTARI_E2E_COST")
	conn := startPlugin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// What the node sends a plugin it loads.
	domain := protocol.NewDomainPluginServiceClient(conn)
	meta, err := domain.Metadata(ctx, &protocol.Empty{})
	require.NoError(t, err)
	assert.Equal(t, "otari", meta.Name)
	init, err := domain.Initialize(ctx, &protocol.InitializeRequest{Config: map[string]string{
		"otari_url":    env(t, "OTARI_E2E_URL"),
		"api_key_file": env(t, "OTARI_E2E_KEY_FILE"),
	}})
	require.NoError(t, err)
	assert.True(t, init.LlmProvider, "the plugin tells the node it is an LLM provider")
	health, err := domain.Health(ctx, &protocol.Empty{})
	require.NoError(t, err)
	assert.True(t, health.Healthy, health.Message)

	llm := protocol.NewLLMServiceClient(conn)
	messages := []*protocol.ChatMessage{{Role: "system", Content: "Be brief."}, {Role: "user", Content: "Say hello."}}

	t.Run("Chat", func(t *testing.T) {
		resp, err := llm.Chat(ctx, &protocol.LLMChatRequest{Provider: "otari", Model: policy, Messages: messages})
		require.NoError(t, err)
		assert.NotEmpty(t, resp.Content)
		assert.Equal(t, model, resp.Model)

		s := resp.GetServed()
		require.NotNil(t, s)
		assert.Equal(t, "otari", s.RequestedProvider)
		assert.Equal(t, policy, s.RequestedModel)
		assert.Equal(t, upstream, s.UpstreamProvider)
		assert.Equal(t, model, s.ExecutedModel)
		assert.Equal(t, policy, s.ResponseModel, "otari's routed body names the policy")
		assert.Equal(t, int32(2), s.AttemptCount)
		assert.True(t, s.Fallback)
		require.NotEmpty(t, s.RequestId)
		assert.Equal(t, cost, s.GetCostUsd())

		rec := otariUsage(t, s.RequestId)
		assert.Equal(t, "success", rec.Status)
		assert.Equal(t, rec.CostUSD, s.GetCostUsd(), "the cost reported is the cost otari recorded")
		assert.Equal(t, int32(rec.PromptTokens), resp.PromptTokens)
		assert.Equal(t, int32(rec.CompletionTokens), resp.CompletionTokens)
		assert.Equal(t, int32(rec.TotalTokens), resp.TotalTokens)
	})

	t.Run("StreamChat", func(t *testing.T) {
		stream, err := llm.StreamChat(ctx, &protocol.LLMChatRequest{Provider: "otari", Model: policy, Messages: messages})
		require.NoError(t, err)
		var text strings.Builder
		var final *protocol.LLMChatChunk
		pieces := 0
		for {
			chunk, err := stream.Recv()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			_, _, err = chunk.SignalCarried()
			require.NoError(t, err)
			text.WriteString(chunk.Token)
			if chunk.Done {
				final = chunk
			} else {
				pieces++
			}
		}
		assert.NotEmpty(t, text.String())
		assert.Greater(t, pieces, 1, "the answer arrives in pieces")
		require.NotNil(t, final)
		assert.Equal(t, model, final.Model)
		s := final.GetServed()
		require.NotNil(t, s)
		assert.Equal(t, policy, s.RequestedModel)
		assert.Equal(t, upstream, s.UpstreamProvider)
		assert.Equal(t, model, s.ExecutedModel)
		assert.Equal(t, cost, s.GetCostUsd())
		rec := otariUsage(t, s.RequestId)
		assert.Equal(t, rec.CostUSD, s.GetCostUsd())
		assert.Equal(t, int32(rec.TotalTokens), final.TotalTokens)
	})

	t.Run("a model named directly keeps the upstream's own name", func(t *testing.T) {
		resp, err := llm.Chat(ctx, &protocol.LLMChatRequest{Provider: "otari", Model: upstream + ":" + model, Messages: messages})
		require.NoError(t, err)
		s := resp.GetServed()
		assert.Equal(t, upstream+":"+model, s.RequestedModel)
		assert.Equal(t, model, s.ExecutedModel)
		assert.Equal(t, int32(1), s.AttemptCount)
		assert.False(t, s.Fallback)
		assert.NotEqual(t, s.RequestedModel, s.ResponseModel)
		t.Logf("asked %q, sent %q, upstream answered as %q", s.RequestedModel, s.ExecutedModel, s.ResponseModel)
	})

	t.Run("a model otari cannot price is refused, with otari's words", func(t *testing.T) {
		_, err := llm.Chat(ctx, &protocol.LLMChatRequest{Provider: "otari", Model: upstream + ":unpriced-model", Messages: messages})
		require.Error(t, err)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err), err.Error())
		assert.Contains(t, err.Error(), "Otari-Request-ID")
	})
}
