// Package plugin is the QNTX LLM provider backed by otari (ADR-014): a Go
// domain plugin that serves QNTX's LLMService (plugin/grpc/protocol/llm.proto)
// by calling otari's chat completions endpoint.
//
// It keeps four things apart, and hands each back in LLMServed:
//
//   - the QNTX provider the call named (LLMChatRequest.provider, this plugin);
//   - the model or routing policy the call named (LLMChatRequest.model), sent
//     to otari as its `model`, unaltered;
//   - the upstream provider and model otari actually used (Otari-Provider,
//     Otari-Model) and the model its body named;
//   - otari's request id (Otari-Request-ID) and the usage and cost it settled.
package plugin

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/teranos/QNTX/plugin"
	"github.com/teranos/QNTX/plugin/grpc/protocol"
	"github.com/teranos/otari-qntx/otari"
	errors "github.com/teranos/sacred-error"
	"go.uber.org/zap"
)

// Name is the plugin's name, which is also the provider a QNTX call names to
// reach it.
const Name = "otari"

// Version is the plugin's version.
var Version = "0.1.0"

// Configuration keys (am.toml [otari], or PUT /api/plugins/otari/config).
const (
	KeyURL        = "otari_url"
	KeyAPIKeyFile = "api_key_file"
	KeyTimeout    = "timeout_seconds"
	// EnvAPIKey is read when api_key_file is not set.
	EnvAPIKey = "OTARI_API_KEY"
)

const defaultTimeout = 300 * time.Second

// Plugin is the otari provider.
type Plugin struct {
	plugin.Base
	client *otari.Client
	logger *zap.SugaredLogger
	llm    *llmServer
}

// New is the otari provider, unconfigured until Initialize.
func New() *Plugin {
	p := &Plugin{
		Base: plugin.NewBase(plugin.Metadata{
			Name:        Name,
			Version:     Version,
			QNTXVersion: ">= 0.1.0",
			Description: "LLM provider backed by otari, Mozilla AI's OpenAI-compatible gateway",
			Author:      "QNTX",
			License:     "Apache-2.0",
		}),
	}
	p.llm = &llmServer{plugin: p}
	return p
}

// Initialize reads the configuration and reaches otari once.
func (p *Plugin) Initialize(ctx context.Context, services plugin.ServiceRegistry) error {
	p.Init(services)
	p.logger = services.Logger(Name)
	cfg := services.Config(Name)

	client, err := clientFrom(cfg.GetString(KeyURL), cfg.GetString(KeyAPIKeyFile), cfg.GetInt(KeyTimeout))
	if err != nil {
		return err
	}
	p.client = client

	if err := p.client.Health(ctx); err != nil {
		// otari may start after the node; the plugin serves once it is up,
		// and Health says it is not.
		p.logger.Warnw("otari is not reachable yet", "url", client.BaseURL, "error", err)
	}
	p.logger.Infow("otari provider initialized", "url", client.BaseURL)
	return nil
}

// clientFrom builds the client from the configuration. The API key is read
// from a file (or OTARI_API_KEY), never from the plugin's config record.
func clientFrom(url, keyFile string, timeoutSeconds int) (*otari.Client, error) {
	if strings.TrimSpace(url) == "" {
		return nil, errors.Newf("%s is not set: the plugin is told where otari is (e.g. http://127.0.0.1:8000)", KeyURL)
	}
	apiKey := os.Getenv(EnvAPIKey)
	if keyFile != "" {
		raw, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, errors.Wrapf(err, "reading the otari API key from %s", keyFile)
		}
		apiKey = strings.TrimSpace(string(raw))
	}
	if apiKey == "" {
		return nil, errors.Newf("no otari API key: set %s to a file holding one, or %s", KeyAPIKeyFile, EnvAPIKey)
	}
	timeout := defaultTimeout
	if timeoutSeconds > 0 {
		timeout = time.Duration(timeoutSeconds) * time.Second
	}
	return otari.New(url, apiKey, timeout), nil
}

// RegisterHTTP registers nothing: the plugin is reached through LLMService.
func (p *Plugin) RegisterHTTP(mux *http.ServeMux) error { return nil }

// Health is whether otari answers its health route.
func (p *Plugin) Health(ctx context.Context) plugin.HealthStatus {
	if p.IsPaused() {
		return plugin.HealthStatus{Healthy: true, Paused: true, Message: "paused"}
	}
	if p.client == nil {
		return plugin.HealthStatus{Healthy: false, Message: "not initialized"}
	}
	if err := p.client.Health(ctx); err != nil {
		return plugin.HealthStatus{Healthy: false, Message: err.Error(), Details: map[string]any{"otari_url": p.client.BaseURL}}
	}
	return plugin.HealthStatus{Healthy: true, Message: "otari answers", Details: map[string]any{"otari_url": p.client.BaseURL}}
}

// ConfigSchema describes the configuration for the node's UI.
func (p *Plugin) ConfigSchema() map[string]plugin.ConfigField {
	return map[string]plugin.ConfigField{
		KeyURL: {
			Type:        "string",
			Description: "Where otari serves, e.g. http://127.0.0.1:8000",
			Required:    true,
			Pattern:     "^https?://",
		},
		KeyAPIKeyFile: {
			Type:        "string",
			Description: "A file holding the otari API key the node calls with (otherwise OTARI_API_KEY)",
		},
		KeyTimeout: {
			Type:         "number",
			Description:  "Seconds a call, its stream included, may take",
			DefaultValue: "300",
			Min:          plugin.BoundAt("1"),
			Max:          plugin.Unbounded(),
		},
	}
}

// LLMService is the plugin's own LLMService: StreamChat, multi-turn messages
// and the served report, none of which plugin.LLMProvider's Chat carries.
func (p *Plugin) LLMService() protocol.LLMServiceServer { return p.llm }

var _ plugin.DomainPlugin = (*Plugin)(nil)
var _ plugin.ConfigurablePlugin = (*Plugin)(nil)
