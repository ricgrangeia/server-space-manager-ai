package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ricgrangeia/server-space-manager-ai/internal/config"
)

type Client struct {
	cfg  config.LLMCfg
	http *http.Client
}

func New(cfg config.LLMCfg) *Client {
	t := time.Duration(cfg.TimeoutSeconds) * time.Second
	if t == 0 {
		t = 60 * time.Second
	}
	return &Client{cfg: cfg, http: &http.Client{Timeout: t}}
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatReq struct {
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	MaxTokens int       `json:"max_tokens,omitempty"`
	Stream    bool      `json:"stream"`
}

type chatResp struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (c *Client) Chat(ctx context.Context, system, user string) (string, error) {
	if !c.cfg.Enabled {
		return "", errors.New("llm disabled")
	}
	body := chatReq{
		Model: c.cfg.Model,
		Messages: []Message{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		MaxTokens: c.cfg.MaxTokens,
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", c.cfg.BaseURL+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var out chatResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Error != nil {
		return "", fmt.Errorf("llm: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", errors.New("llm: empty response")
	}
	return out.Choices[0].Message.Content, nil
}
