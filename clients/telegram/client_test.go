package telegram

import (
	"context"
	"encoding/json"
	"errors"
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
	if info, statErr := os.Stat(statePath); statErr != nil || info.Mode().Perm() != updateOffsetFileMode {
		t.Fatalf("offset file = %v, error = %v, want mode %04o", info, statErr, updateOffsetFileMode)
	}
	if info, statErr := os.Stat(filepath.Dir(statePath)); statErr != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("offset directory = %v, error = %v, want mode 0700", info, statErr)
	}

	restarted, err := New("token", "123", statePath)
	if err != nil {
		t.Fatalf("New() after restart error = %v", err)
	}
	if restarted.lastUpdateID != 42 {
		t.Fatalf("lastUpdateID after restart = %d, want 42", restarted.lastUpdateID)
	}
}

func TestPollCommandsPublishesFreshStateDirectoryParents(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(root, "nested", "data", "telegram-update-offset")
	client, err := New("token", "123", statePath)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	var synced []string
	client.syncDirectoryFn = func(path string) error {
		synced = append(synced, filepath.Clean(path))
		return nil
	}
	messageDate := time.Now().Unix()
	client.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return telegramUpdatesResponse(fmt.Sprintf(`{"ok":true,"result":[{"update_id":42,"message":{"from":{"id":123},"chat":{"id":123,"type":"private"},"date":%d,"text":"/status"}}]}`, messageDate)), nil
	})}

	if _, err := client.PollCommands(context.Background()); err != nil {
		t.Fatalf("PollCommands() error = %v", err)
	}
	want := []string{root, filepath.Join(root, "nested"), filepath.Join(root, "nested", "data")}
	if len(synced) != len(want) {
		t.Fatalf("synced directories = %v, want %v", synced, want)
	}
	for i := range want {
		if synced[i] != filepath.Clean(want[i]) {
			t.Fatalf("synced directories = %v, want %v", synced, want)
		}
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

func TestTransportErrorsRedactBotToken(t *testing.T) {
	const token = "123456:secret-token"
	client, err := New(token, "123", filepath.Join(t.TempDir(), "telegram-update-offset"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("dial failed for %s", req.URL)
	})}

	err = client.SendMessage(context.Background(), "test")
	if err == nil {
		t.Fatal("SendMessage() error = nil, want transport error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("SendMessage() leaked token in error: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("SendMessage() error = %v, want redaction marker", err)
	}
}

func TestSendErrorEscapesHTML(t *testing.T) {
	client, err := New("token", "123", filepath.Join(t.TempDir(), "telegram-update-offset"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body sendMessageRequest
		if decodeErr := json.NewDecoder(req.Body).Decode(&body); decodeErr != nil {
			return nil, decodeErr
		}
		if strings.Contains(body.Text, "<device>") || !strings.Contains(body.Text, "&lt;device&gt;") {
			return nil, errors.New("error message was not HTML escaped")
		}
		return telegramUpdatesResponse(`{"ok":true,"result":true}`), nil
	})}

	if err := client.SendError(context.Background(), "failed <device>"); err != nil {
		t.Fatalf("SendError() error = %v", err)
	}
}

func TestSendTradingPlanExplainsNetProfitThreshold(t *testing.T) {
	client, err := New("token", "123", filepath.Join(t.TempDir(), "telegram-update-offset"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	var message sendMessageRequest
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&message); err != nil {
			return nil, err
		}
		return telegramUpdatesResponse(`{"ok":true,"result":true}`), nil
	})}

	err = client.SendTradingPlan(context.Background(), TradingPlanData{
		Day:               "horizon",
		Date:              time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC),
		PriceMin:          0.126,
		PriceMax:          0.4394,
		IsProfitable:      true,
		MinExpectedProfit: 0.05,
		BatteryEfficiency: 0.90,
		Cycles: []TradingPlanCycle{{
			ChargeStart:    "Mon 07 Sep 01:45",
			ChargeEnd:      "Mon 07 Sep 04:00",
			ChargePrice:    0.3104,
			DischargeStart: "Mon 07 Sep 06:30",
			DischargeEnd:   "Mon 07 Sep 08:45",
			DischargePrice: 0.3958,
			ProfitPerKWh:   0.0558,
		}},
	})
	if err != nil {
		t.Fatalf("SendTradingPlan() error = %v", err)
	}

	for _, want := range []string{
		"Configured minimum net profit: 0.0500 EUR/kWh",
		"Battery efficiency: 90.0%",
		"Mon 07 Sep 01:45",
		"Expected profit: 0.0558 EUR/kWh",
	} {
		if !strings.Contains(message.Text, want) {
			t.Errorf("message missing %q:\n%s", want, message.Text)
		}
	}

	err = client.SendTradingPlan(context.Background(), TradingPlanData{
		Day:               "horizon",
		Date:              time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC),
		SlotsTotal:        192,
		SlotsAnalyzed:     192,
		PriceMin:          0.126,
		PriceMax:          0.4394,
		Reason:            "Expected profit is below the configured minimum",
		MinExpectedProfit: 0.05,
		BatteryEfficiency: 0.855,
	})
	if err != nil {
		t.Fatalf("SendTradingPlan() non-profitable error = %v", err)
	}
	for _, want := range []string{
		"No profitable opportunities",
		"Configured minimum net profit: 0.0500 EUR/kWh",
		"Battery efficiency: 85.5%",
	} {
		if !strings.Contains(message.Text, want) {
			t.Errorf("non-profitable message missing %q:\n%s", want, message.Text)
		}
	}

	err = client.SendTradingPlan(context.Background(), TradingPlanData{
		Day:               "horizon",
		Date:              time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC),
		MinExpectedProfit: 0.05,
		BatteryEfficiency: 0.90,
		PlanRetained:      true,
		DischargeOnly:     true,
		DischargeStart:    "Mon 07 Sep 06:30",
		DischargeEnd:      "Mon 07 Sep 08:45",
		DischargePrice:    0.3958,
	})
	if err != nil {
		t.Fatalf("SendTradingPlan() discharge-only error = %v", err)
	}
	for _, want := range []string{
		"Retained Discharge Obligation",
		"Grid charging is disabled",
		"Discharge: Mon 07 Sep 06:30 - Mon 07 Sep 08:45 @ 0.3958 EUR/kWh",
		"Active committed-cycle plan retained; refreshed plan pending.",
	} {
		if !strings.Contains(message.Text, want) {
			t.Errorf("discharge-only message missing %q:\n%s", want, message.Text)
		}
	}
}

func TestSendStatusDistinguishesUnavailableAndZeroPrice(t *testing.T) {
	client, err := New("token", "123", "")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	var messages []sendMessageRequest
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var message sendMessageRequest
		if err := json.NewDecoder(req.Body).Decode(&message); err != nil {
			return nil, err
		}
		messages = append(messages, message)
		return telegramUpdatesResponse(`{"ok":true,"result":true}`), nil
	})}

	if err := client.SendStatus(context.Background(), StatusData{}); err != nil {
		t.Fatalf("SendStatus(unavailable) error = %v", err)
	}
	if err := client.SendStatus(context.Background(), StatusData{CurrentPriceKnown: true}); err != nil {
		t.Fatalf("SendStatus(zero) error = %v", err)
	}
	if len(messages) != 2 || !strings.Contains(messages[0].Text, "<b>Price:</b> unavailable") ||
		!strings.Contains(messages[1].Text, "<b>Price:</b> 0.0000 EUR/kWh") {
		t.Fatalf("status price messages = %+v", messages)
	}
}

func TestDailySummaryDisclosesUnpricedEnergy(t *testing.T) {
	client, err := New("token", "123", "")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	var message sendMessageRequest
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&message); err != nil {
			return nil, err
		}
		return telegramUpdatesResponse(`{"ok":true,"result":true}`), nil
	})}

	err = client.SendDailySummaryFull(context.Background(), DailySummaryData{
		Date:               time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC),
		DischargeCycles:    1,
		DischargedKWh:      1,
		UnpricedKWh:        .25,
		PnLIncomplete:      true,
		TotalPnLIncomplete: true,
	})
	if err != nil {
		t.Fatalf("SendDailySummaryFull() error = %v", err)
	}
	for _, want := range []string{"Today's known cash flow", "Cumulative known cash flow", "0.25 kWh could not be priced", "totals are incomplete"} {
		if !strings.Contains(message.Text, want) {
			t.Errorf("daily summary %q does not contain %q", message.Text, want)
		}
	}
}

func TestSolarOnlyDailySummaryIncludesKnownCashFlow(t *testing.T) {
	client, err := New("token", "123", "")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	var message sendMessageRequest
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&message); err != nil {
			return nil, err
		}
		return telegramUpdatesResponse(`{"ok":true,"result":true}`), nil
	})}

	err = client.SendDailySummaryFull(context.Background(), DailySummaryData{
		Date:              time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC),
		SolarChargedKWh:   1.25,
		SolarChargeCycles: 1,
		PnLEUR:            -.02,
		TotalPnLEUR:       .50,
	})
	if err != nil {
		t.Fatalf("SendDailySummaryFull() error = %v", err)
	}
	for _, want := range []string{"Today's P&L:</b> -0.0200 EUR", "Cumulative P&L:</b> +0.5000 EUR"} {
		if !strings.Contains(message.Text, want) {
			t.Errorf("solar-only daily summary %q does not contain %q", message.Text, want)
		}
	}
}

func TestDailySummaryShowsCrossMidnightContinuationEnergy(t *testing.T) {
	client, err := New("token", "123", "")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	var message sendMessageRequest
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&message); err != nil {
			return nil, err
		}
		return telegramUpdatesResponse(`{"ok":true,"result":true}`), nil
	})}

	err = client.SendDailySummaryFull(context.Background(), DailySummaryData{
		Date:           time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC),
		ChargedKWh:     .5,
		PnLEUR:         -.05,
		AvgChargePrice: .10,
		MinChargePrice: .10,
	})
	if err != nil {
		t.Fatalf("SendDailySummaryFull() error = %v", err)
	}
	if strings.Contains(message.Text, "No trades today") || !strings.Contains(message.Text, "Charged:</b> 0.50 kWh") ||
		!strings.Contains(message.Text, "Today's P&L:</b> -0.0500 EUR") {
		t.Fatalf("cross-midnight continuation summary = %q", message.Text)
	}
}

func TestSendTradingPlanFitsTelegramMessageLimit(t *testing.T) {
	client, err := New("token", "123", filepath.Join(t.TempDir(), "telegram-update-offset"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	var message sendMessageRequest
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&message); err != nil {
			return nil, err
		}
		return telegramUpdatesResponse(`{"ok":true,"result":true}`), nil
	})}
	cycles := make([]TradingPlanCycle, 48)
	for i := range cycles {
		cycles[i] = TradingPlanCycle{
			ChargeStart:    "Mon 07 Sep 01:45",
			ChargeEnd:      "Mon 07 Sep 02:00",
			ChargePrice:    .10,
			DischargeStart: "Mon 07 Sep 06:30",
			DischargeEnd:   "Mon 07 Sep 06:45",
			DischargePrice: .40,
			ProfitPerKWh:   .26,
		}
	}

	err = client.SendTradingPlan(context.Background(), TradingPlanData{
		Day:               "horizon",
		Date:              time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC),
		PriceMin:          .10,
		PriceMax:          .40,
		IsProfitable:      true,
		MinExpectedProfit: .05,
		BatteryEfficiency: .90,
		PlanRetained:      true,
		Cycles:            cycles,
	})
	if err != nil {
		t.Fatalf("SendTradingPlan() error = %v", err)
	}
	if len(message.Text) > messageTextLimit {
		t.Fatalf("message length = %d, limit = %d", len(message.Text), messageTextLimit)
	}
	if !strings.Contains(message.Text, "cycles omitted from this message") {
		t.Fatalf("message did not report omitted cycles:\n%s", message.Text)
	}
	if !strings.Contains(message.Text, "Active committed-cycle plan retained") {
		t.Fatalf("message did not report retained plan:\n%s", message.Text)
	}
}

func TestDailySummaryIncludesMixedContinuationEnergy(t *testing.T) {
	client, err := New("token", "123", "")
	if err != nil {
		t.Fatal(err)
	}
	var message sendMessageRequest
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&message); err != nil {
			return nil, err
		}
		return telegramUpdatesResponse(`{"ok":true,"result":true}`), nil
	})}
	err = client.SendDailySummaryFull(context.Background(), DailySummaryData{
		Date:       time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
		ChargedKWh: .75, SolarChargedKWh: .25, SolarChargeCycles: 1,
		DischargedKWh: .5, PnLEUR: -.1,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Charged:</b> 0.75 kWh", "Solar charged:</b> 0.25 kWh", "Discharged:</b> 0.50 kWh"} {
		if !strings.Contains(message.Text, want) {
			t.Errorf("summary omitted %q:\n%s", want, message.Text)
		}
	}
}
