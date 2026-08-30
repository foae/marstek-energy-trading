package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func telegramUpdatesResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestPollCommandsPersistsOffsetBeforeReturningCommand(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "telegram-update-offset")
	client, err := New("token", "123", statePath)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	messageDate := time.Now().Unix()
	client.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return telegramUpdatesResponse(fmt.Sprintf(`{"ok":true,"result":[{"update_id":42,"message":{"from":{"id":123},"chat":{"id":123,"type":"private"},"date":%d,"text":"/discharge 800"}}]}`, messageDate)), nil
	})}

	commands, err := client.PollCommands(context.Background())
	if err != nil {
		t.Fatalf("PollCommands() error = %v", err)
	}
	if len(commands) != 1 || commands[0] != "/discharge 800" {
		t.Fatalf("PollCommands() = %v, want [/discharge 800]", commands)
	}
	persisted, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(persisted) != "42\n" {
		t.Fatalf("persisted offset = %q, want %q", persisted, "42\\n")
	}

	restarted, err := New("token", "123", statePath)
	if err != nil {
		t.Fatalf("New() after restart error = %v", err)
	}
	if restarted.lastUpdateID != 42 {
		t.Fatalf("lastUpdateID after restart = %d, want 42", restarted.lastUpdateID)
	}
}

func TestPollCommandsAcceptsOnlyFreshPrivateChatCommands(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "telegram-update-offset")
	client, err := New("token", "123", statePath)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	now := time.Now()
	body := fmt.Sprintf(`{"ok":true,"result":[
		{"update_id":1,"message":{"from":{"id":123},"chat":{"id":123,"type":"private"},"date":%d,"text":"/status"}},
		{"update_id":2,"message":{"from":{"id":123},"chat":{"id":123,"type":"private"},"date":%d,"text":"/discharge"}},
		{"update_id":3,"message":{"from":{"id":999},"chat":{"id":123,"type":"group"},"date":%d,"text":"/discharge"}},
		{"update_id":4,"message":{"from":{"id":999},"chat":{"id":123,"type":"private"},"date":%d,"text":"/auto"}}
	]}`, now.Unix(), now.Add(-commandMaxAge-time.Second).Unix(), now.Unix(), now.Unix())
	client.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return telegramUpdatesResponse(body), nil
	})}

	commands, err := client.PollCommands(context.Background())
	if err != nil {
		t.Fatalf("PollCommands() error = %v", err)
	}
	if len(commands) != 1 || commands[0] != "/status" {
		t.Fatalf("PollCommands() = %v, want [/status]", commands)
	}
	if client.lastUpdateID != 4 {
		t.Fatalf("lastUpdateID = %d, want 4", client.lastUpdateID)
	}
}

func TestRegisterCommandsPublishesPrivateChatMenu(t *testing.T) {
	client, err := New("token", "123", filepath.Join(t.TempDir(), "telegram-update-offset"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	var request setMyCommandsRequest
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.Path != "/bottoken/setMyCommands" {
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL)
		}
		if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		return telegramUpdatesResponse(`{"ok":true,"result":true}`), nil
	})}

	if err := client.RegisterCommands(context.Background()); err != nil {
		t.Fatalf("RegisterCommands() error = %v", err)
	}
	if request.Scope.Type != "chat" || request.Scope.ChatID != "123" {
		t.Fatalf("scope = %+v, want configured private chat", request.Scope)
	}
	want := []botCommand{
		{Command: "status", Description: "Show battery and trading status"},
		{Command: "discharge", Description: "Start manual discharge (optional watts)"},
		{Command: "auto", Description: "Stop manual discharge and resume automatic control"},
	}
	if len(request.Commands) != len(want) {
		t.Fatalf("commands = %+v, want %+v", request.Commands, want)
	}
	for i := range want {
		if request.Commands[i] != want[i] {
			t.Fatalf("command %d = %+v, want %+v", i, request.Commands[i], want[i])
		}
	}
}

func TestRegisterCommandsReturnsTelegramAPIFailure(t *testing.T) {
	client, err := New("token", "123", filepath.Join(t.TempDir(), "telegram-update-offset"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	client.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return telegramUpdatesResponse(`{"ok":false,"description":"commands rejected"}`), nil
	})}

	err = client.RegisterCommands(context.Background())
	if err == nil || !strings.Contains(err.Error(), "commands rejected") {
		t.Fatalf("RegisterCommands() error = %v, want Telegram failure", err)
	}
}
