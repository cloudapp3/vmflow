package controlapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/cloudapp3/vmflow/config"
)

// botConfigResponse is the GET /v1/config/bot payload. Tokens are returned in
// plaintext because the caller is an authenticated admin (who can already read
// config.yaml); the TUI masks them in its editor.
type botConfigResponse struct {
	Revision        string `json:"revision"`
	BotToken        string `json:"bot_token"`
	BotChat         int64  `json:"bot_chat"`
	BotControlToken string `json:"bot_control_token"`
	Running         bool   `json:"running"`
}

type botConfigRequest struct {
	BotToken        string `json:"bot_token"`
	BotChat         int64  `json:"bot_chat"`
	BotControlToken string `json:"bot_control_token"`
}

// renderBotConfig edits the top-level bot_token, bot_chat, and bot_control_token
// nodes of source, preserving all other nodes and comments.
func renderBotConfig(source []byte, settings BotSettings) ([]byte, error) {
	document, err := parseYAMLDocument(source)
	if err != nil {
		return nil, err
	}
	root := document.Content[0]
	tokenNode, err := encodeYAMLNode(settings.Token)
	if err != nil {
		return nil, fmt.Errorf("encode bot_token: %w", err)
	}
	chatNode, err := encodeYAMLNode(settings.ChatID)
	if err != nil {
		return nil, fmt.Errorf("encode bot_chat: %w", err)
	}
	controlNode, err := encodeYAMLNode(settings.ControlToken)
	if err != nil {
		return nil, fmt.Errorf("encode bot_control_token: %w", err)
	}
	setTopLevelYAMLNode(root, "bot_token", tokenNode)
	setTopLevelYAMLNode(root, "bot_chat", chatNode)
	setTopLevelYAMLNode(root, "bot_control_token", controlNode)
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

// BuildBotCandidate renders and validates a bot-field-only replacement document.
func (document *configDocument) BuildBotCandidate(settings BotSettings) (*configCandidate, error) {
	if document == nil || strings.TrimSpace(document.path) == "" || len(document.raw) == 0 {
		return nil, fmt.Errorf("build bot candidate: invalid source document")
	}
	raw, err := renderBotConfig(document.raw, settings)
	if err != nil {
		return nil, fmt.Errorf("build bot candidate: %w", err)
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("build bot candidate: validate: %w", err)
	}
	return &configCandidate{
		Config:         cfg,
		Revision:       configRevision(raw),
		path:           document.path,
		raw:            raw,
		sourceRevision: document.Revision,
	}, nil
}

func (runtime *Runtime) handleBotConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		runtime.getBotConfig(w, r)
	case http.MethodPut:
		runtime.putBotConfig(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

func (runtime *Runtime) getBotConfig(w http.ResponseWriter, r *http.Request) {
	if !runtime.authorizeWrite(w, r) {
		return
	}
	document, err := loadConfigDocument(runtime.ConfigPath)
	if err != nil {
		runtime.log(r).Error("load bot config failed", "component", "controlapi", "event", "bot_config_load_failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "configuration could not be loaded"})
		return
	}
	running := runtime.Bot != nil && runtime.Bot.Running()
	w.Header().Set("ETag", configETag(document.Revision))
	writeJSON(w, http.StatusOK, botConfigResponse{
		Revision:        document.Revision,
		BotToken:        document.Config.BotToken,
		BotChat:         document.Config.BotChat,
		BotControlToken: document.Config.BotControlToken,
		Running:         running,
	})
}

func (runtime *Runtime) putBotConfig(w http.ResponseWriter, r *http.Request) {
	if !runtime.authorizeConfigWrite(w, r) {
		return
	}
	revision, err := requestRevision(r)
	if err != nil {
		writeRevisionHeaderError(w, err)
		return
	}
	draft, err := decodeBotConfigRequest(w, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	settings := normalizeBotSettings(BotSettings{Token: draft.BotToken, ChatID: draft.BotChat, ControlToken: draft.BotControlToken})
	if runtime.Bot == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "bot controller unavailable"})
		return
	}

	runtime.reloadMu.Lock()
	defer runtime.reloadMu.Unlock()

	document, err := loadConfigDocument(runtime.ConfigPath)
	if err != nil {
		runtime.log(r).Error("load bot config failed", "component", "controlapi", "event", "bot_config_load_failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "configuration could not be loaded"})
		return
	}
	if document.Revision != revision {
		writeJSON(w, http.StatusPreconditionFailed, map[string]any{"error": "configuration revision changed", "current_revision": document.Revision})
		return
	}
	candidate, err := document.BuildBotCandidate(settings)
	if err != nil {
		runtime.log(r).Error("build bot candidate failed", "component", "controlapi", "event", "bot_config_build_failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "configuration could not be prepared"})
		return
	}
	staged, err := candidate.Stage()
	if err != nil {
		writeJSON(w, http.StatusPreconditionFailed, map[string]any{"error": "configuration changed before write"})
		return
	}
	defer staged.Discard()

	previousSettings := botSettingsFromConfig(document.Config)
	previousRunning := runtime.Bot != nil && runtime.Bot.Running()
	botApplied := false
	if runtime.Bot != nil {
		if applyErr := runtime.Bot.Apply(settings); applyErr != nil {
			runtime.log(r).Error("bot candidate failed readiness check", "component", "controlapi", "event", "bot_apply_failed", "error", safeBotApplyError(applyErr))
			writeBotApplyError(w, applyErr, runtime.Bot.Running())
			return
		}
		botApplied = true
	}
	if runtime.configHooks != nil && runtime.configHooks.BeforeCommit != nil {
		runtime.configHooks.BeforeCommit(staged)
	}
	if err := staged.Commit(); err != nil {
		if staged.Committed() {
			runtime.updateBotBaseline(candidate.Config)
			runtime.markDegraded("bot configuration committed but durability sync failed")
			runtime.log(r).Error("bot config committed with durability failure", "component", "controlapi", "event", "bot_config_commit_durability_failed", "error", err)
			w.Header().Set("ETag", configETag(candidate.Revision))
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error":     "configuration committed but durability could not be confirmed",
				"committed": true,
				"revision":  candidate.Revision,
				"running":   runtime.Bot != nil && runtime.Bot.Running(),
			})
			return
		}
		rollbackErr := runtime.restoreBotRuntime(previousSettings, previousRunning, botApplied)
		status := http.StatusInternalServerError
		if errors.Is(err, errConfigRevisionConflict) && rollbackErr == nil {
			status = http.StatusPreconditionFailed
		}
		if rollbackErr != nil {
			status = http.StatusServiceUnavailable
			runtime.markDegraded("bot configuration commit and runtime rollback failed")
		}
		runtime.log(r).Error("bot config commit failed", "component", "controlapi", "event", "bot_config_commit_failed", "error", err, "rollback_failed", rollbackErr != nil)
		writeJSON(w, status, map[string]any{
			"error":       "configuration could not be committed",
			"rollback_ok": rollbackErr == nil,
			"running":     runtime.Bot != nil && runtime.Bot.Running(),
		})
		return
	}
	runtime.updateBotBaseline(candidate.Config)
	runtime.log(r).Info("bot config updated", "component", "controlapi", "event", "bot_config_commit", "has_token", settings.Token != "", "has_control_token", settings.ControlToken != "")
	w.Header().Set("ETag", configETag(candidate.Revision))
	writeJSON(w, http.StatusOK, botConfigResponse{
		Revision:        candidate.Revision,
		BotToken:        candidate.Config.BotToken,
		BotChat:         candidate.Config.BotChat,
		BotControlToken: candidate.Config.BotControlToken,
		Running:         runtime.Bot != nil && runtime.Bot.Running(),
	})
}

func (runtime *Runtime) handleBotStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if !runtime.authorizeConfigWrite(w, r) {
		return
	}
	if runtime.Bot == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "bot controller unavailable"})
		return
	}
	runtime.reloadMu.Lock()
	defer runtime.reloadMu.Unlock()
	document, err := loadConfigDocument(runtime.ConfigPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "configuration could not be loaded"})
		return
	}
	settings := botSettingsFromConfig(document.Config)
	if err := runtime.Bot.Apply(settings); err != nil {
		runtime.log(r).Error("bot start failed", "component", "controlapi", "event", "bot_start_failed", "error", safeBotApplyError(err))
		writeBotApplyError(w, err, runtime.Bot.Running())
		return
	}
	runtime.log(r).Info("bot started", "component", "controlapi", "event", "bot_start", "running", runtime.Bot.Running())
	writeJSON(w, http.StatusOK, map[string]any{"running": runtime.Bot.Running()})
}

func (runtime *Runtime) handleBotStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if !runtime.authorizeConfigWrite(w, r) {
		return
	}
	if runtime.Bot == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "bot controller unavailable"})
		return
	}
	runtime.reloadMu.Lock()
	defer runtime.reloadMu.Unlock()
	if err := runtime.Bot.Stop(); err != nil {
		runtime.log(r).Error("bot stop failed", "component", "controlapi", "event", "bot_stop_failed", "error", safeBotApplyError(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "bot could not be stopped", "running": runtime.Bot.Running()})
		return
	}
	runtime.log(r).Info("bot stopped", "component", "controlapi", "event", "bot_stop", "running", runtime.Bot.Running())
	writeJSON(w, http.StatusOK, map[string]any{"running": runtime.Bot.Running()})
}

func decodeBotConfigRequest(w http.ResponseWriter, r *http.Request) (botConfigRequest, error) {
	if r.Body == nil {
		return botConfigRequest{}, errors.New("missing request body")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxConfigManagementBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var req botConfigRequest
	if err := decoder.Decode(&req); err != nil {
		return botConfigRequest{}, fmt.Errorf("decode request: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return botConfigRequest{}, err
	}
	return req, nil
}

func normalizeBotSettings(settings BotSettings) BotSettings {
	settings.Token = strings.TrimSpace(settings.Token)
	settings.ControlToken = strings.TrimSpace(settings.ControlToken)
	return settings
}

func botSettingsFromConfig(cfg config.File) BotSettings {
	return normalizeBotSettings(BotSettings{
		Token:        cfg.BotToken,
		ChatID:       cfg.BotChat,
		ControlToken: cfg.BotControlToken,
	})
}

func (runtime *Runtime) restoreBotRuntime(settings BotSettings, wasRunning, changed bool) error {
	if runtime == nil || runtime.Bot == nil || !changed {
		return nil
	}
	if wasRunning {
		return runtime.Bot.Apply(settings)
	}
	return runtime.Bot.Stop()
}

func (runtime *Runtime) updateBotBaseline(cfg config.File) {
	if runtime == nil || runtime.StartupConfig == nil {
		return
	}
	runtime.StartupConfig.BotToken = cfg.BotToken
	runtime.StartupConfig.BotChat = cfg.BotChat
	runtime.StartupConfig.BotControlToken = cfg.BotControlToken
}

func writeBotApplyError(w http.ResponseWriter, err error, running bool) {
	status := http.StatusServiceUnavailable
	message := "bot readiness could not be established"
	if IsBotValidationError(err) {
		status = http.StatusUnprocessableEntity
		message = "invalid bot configuration"
	}
	writeJSON(w, status, map[string]any{
		"error":   message,
		"running": running,
	})
}

func safeBotApplyError(err error) string {
	if IsBotValidationError(err) {
		return "telegram bot settings are invalid"
	}
	return "telegram bot readiness or lifecycle operation failed"
}
