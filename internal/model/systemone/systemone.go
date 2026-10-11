// Package systemone is a client for System One decision models (TEN-510),
// such as Cloudflare's Clef-flash: POST /v1/systemone with a state and typed
// questions, and back come probabilities, never text. Tenant uses one as the
// judge in the loop of a turn.
package systemone

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Question types.
const (
	Noul   = "noul"   // yes/no: Answer.Noul is P(true)
	Choice = "choice" // one of named options: Criteria is map[string]string
	Score  = "score"  // ordered levels: Criteria is []string, Answer.Score is the expected level
)

// Question is one typed question. Instructions say what to decide.
type Question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions,omitempty"`
	Criteria     any    `json:"criteria,omitempty"`
}

// Answer is a question's answer; which fields are set depends on its Type.
type Answer struct {
	Type          string             `json:"type"`
	Noul          float64            `json:"noul"`
	Choice        string             `json:"choice"`
	Score         float64            `json:"score"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// Usage is what one call cost. Decision models read only.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Client calls one System One server.
type Client struct {
	Endpoint string // base URL, without /v1/systemone
	Model    string
	Key      string       // sent as "Authorization: Bearer <Key>" when set; "" for a keyless server
	HTTP     *http.Client // nil: a client with a 10 s timeout
}

const maxResponse = 1 << 20

var defaultHTTP = &http.Client{Timeout: 10 * time.Second}

// Decide asks qs about state (a string, or anything JSON-encodable) and
// returns the answers by question id.
func (c *Client) Decide(ctx context.Context, state any, qs map[string]Question) (map[string]Answer, Usage, error) {
	body, err := json.Marshal(struct {
		Model     string              `json:"model"`
		State     any                 `json:"state"`
		Questions map[string]Question `json:"questions"`
	}{c.Model, state, qs})
	if err != nil {
		return nil, Usage{}, err
	}
	url := strings.TrimRight(c.Endpoint, "/") + "/v1/systemone"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	hc := c.HTTP
	if hc == nil {
		hc = defaultHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("systemone: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return nil, Usage{}, fmt.Errorf("systemone: %w", err)
	}
	var out struct {
		Answers map[string]Answer `json:"answers"`
		Usage   Usage             `json:"usage"`
		Error   string            `json:"error"`
	}
	decodeErr := json.Unmarshal(raw, &out)
	if resp.StatusCode/100 == 2 && decodeErr != nil {
		return nil, Usage{}, fmt.Errorf("systemone: the answer isn't System One JSON (%v): %.120s", decodeErr, raw)
	}
	if resp.StatusCode/100 != 2 {
		msg := out.Error
		if msg == "" {
			msg = strings.TrimSpace(string(raw[:min(len(raw), 200)]))
		}
		return nil, Usage{}, fmt.Errorf("systemone: HTTP %d: %s", resp.StatusCode, msg)
	}
	for id := range qs {
		if _, ok := out.Answers[id]; !ok {
			return nil, out.Usage, errors.New("systemone: no answer for " + id)
		}
	}
	return out.Answers, out.Usage, nil
}
