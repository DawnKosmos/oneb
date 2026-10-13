package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// A partial stream is never a successful assistant message.
func readReply(r io.Reader) (string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	var output strings.Builder
	var data []string
	handle := func() (bool, error) {
		if len(data) == 0 {
			return false, nil
		}
		raw := strings.Join(data, "\n")
		data = nil
		if raw == "[DONE]" {
			return false, nil
		}
		var event struct {
			Type     string `json:"type"`
			Delta    string `json:"delta"`
			Code     string `json:"code"`
			Response struct {
				Status string `json:"status"`
				Error  *struct {
					Code string `json:"code"`
				} `json:"error"`
				Output []struct {
					Content []struct {
						Type    string `json:"type"`
						Text    string `json:"text"`
						Refusal string `json:"refusal"`
					} `json:"content"`
				} `json:"output"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			return false, errors.New("Invalid OpenAI stream")
		}
		switch event.Type {
		case "response.output_text.delta":
			if output.Len()+len(event.Delta) > 2<<20 {
				return false, errors.New("Reply exceeds the MVP size limit")
			}
			output.WriteString(event.Delta)
		case "response.failed", "response.incomplete", "error":
			code := event.Code
			if event.Response.Error != nil {
				code = event.Response.Error.Code
			}
			// Only a structured error code is exposed, never raw provider bodies or tokens.
			if len(code) > 100 {
				code = ""
			}
			return false, errors.New("OpenAI could not complete the reply: " + code + ". Check ChatGPT plan usage or try again")
		case "response.completed":
			if event.Response.Status != "completed" {
				return false, errors.New("OpenAI did not complete the reply")
			}
			if output.Len() == 0 {
				for _, item := range event.Response.Output {
					for _, c := range item.Content {
						if c.Type == "output_text" {
							output.WriteString(c.Text)
						} else if c.Type == "refusal" {
							output.WriteString(c.Refusal)
						}
					}
				}
			}
			if output.Len() == 0 {
				return false, errors.New("OpenAI completed without a text reply")
			}
			return true, nil
		}
		return false, nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			done, err := handle()
			if err != nil {
				return "", err
			}
			if done {
				return output.String(), nil
			}
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if scanner.Err() != nil {
		return "", errors.New("OpenAI stream was interrupted. Try again")
	}
	done, err := handle()
	if err != nil {
		return "", err
	}
	if done {
		return output.String(), nil
	}
	return "", errors.New("OpenAI stream ended before completion. Try again")
}
