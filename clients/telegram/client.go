package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const (
	sendMessageAPI = "https://api.telegram.org/bot%s/sendMessage"
	getUpdatesAPI  = "https://api.telegram.org/bot%s/getUpdates"
)

// Client is a Telegram bot client.
type Client struct {
	botToken     string
	chatID       string
	httpClient   *http.Client
	enabled      bool
	lastUpdateID int64
}

// New creates a new Telegram client.
// If botToken or chatID is empty, the client will be disabled (no-op).
func New(botToken, chatID string) *Client {
	return &Client{
		botToken: botToken,
		chatID:   chatID,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		enabled: botToken != "" && chatID != "",
	}
}

// sendMessageRequest is the Telegram API request body.
type sendMessageRequest struct {
	ChatID    string `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode,omitempty"`
}

// SendMessage sends a message to the configured chat.
// Returns nil if the client is disabled.
func (c *Client) SendMessage(ctx context.Context, text string) error {
	if !c.enabled {
		return nil
	}

	reqBody := sendMessageRequest{
		ChatID:    c.chatID,
		Text:      text,
		ParseMode: "HTML",
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf(sendMessageAPI, c.botToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send message: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return nil
}

// Enabled returns true if the client is configured and enabled.
func (c *Client) Enabled() bool {
	return c.enabled
}

// SendTradeStart sends a notification when a trade starts.
func (c *Client) SendTradeStart(ctx context.Context, action string, priceEUR float64, soc int) error {
	text := fmt.Sprintf(
		"<b>%s started</b>\nPrice: %.4f EUR/kWh\nSOC: %d%%",
		action, priceEUR, soc,
	)
	return c.SendMessage(ctx, text)
}

// SendTradeEnd sends a notification when a trade ends.
func (c *Client) SendTradeEnd(ctx context.Context, action string, energyKWh float64, avgPriceEUR float64) error {
	text := fmt.Sprintf(
		"<b>%s completed</b>\nEnergy: %.2f kWh\nAvg price: %.4f EUR/kWh",
		action, energyKWh, avgPriceEUR,
	)
	return c.SendMessage(ctx, text)
}

// SendError sends an error notification.
func (c *Client) SendError(ctx context.Context, errMsg string) error {
	text := fmt.Sprintf("⚠️ <b>Error</b>\n%s", errMsg)
	return c.SendMessage(ctx, text)
}

// DailySummaryData contains all data for the daily summary notification.
type DailySummaryData struct {
	Date              time.Time
	PnLEUR            float64
	ChargedKWh        float64
	DischargedKWh     float64
	ChargeCycles      int
	DischargeCycles   int
	AvgChargePrice    float64
	AvgDischargePrice float64
	MinChargePrice    float64
	MaxDischargePrice float64
	TotalPnLEUR       float64 // cumulative P&L
}

// SendDailySummary sends a daily P&L summary (simple version for backward compatibility).
func (c *Client) SendDailySummary(ctx context.Context, pnlEUR float64, chargedKWh, dischargedKWh float64) error {
	data := DailySummaryData{
		Date:          time.Now(),
		PnLEUR:        pnlEUR,
		ChargedKWh:    chargedKWh,
		DischargedKWh: dischargedKWh,
	}
	return c.SendDailySummaryFull(ctx, data)
}

// SendDailySummaryFull sends a comprehensive daily summary.
func (c *Client) SendDailySummaryFull(ctx context.Context, data DailySummaryData) error {
	pnlSign := ""
	pnlEmoji := "📊"
	if data.PnLEUR > 0 {
		pnlSign = "+"
		pnlEmoji = "📈"
	} else if data.PnLEUR < 0 {
		pnlEmoji = "📉"
	}

	totalSign := ""
	if data.TotalPnLEUR > 0 {
		totalSign = "+"
	}

	var text string
	if data.ChargeCycles == 0 && data.DischargeCycles == 0 {
		text = fmt.Sprintf(
			"%s <b>Daily Summary - %s</b>\n\n"+
				"No trades today.\n\n"+
				"💰 <b>Cumulative P&L:</b> %s%.4f EUR",
			pnlEmoji,
			data.Date.Format("02 Jan 2006"),
			totalSign, data.TotalPnLEUR,
		)
	} else {
		text = fmt.Sprintf(
			"%s <b>Daily Summary - %s</b>\n\n"+
				"💰 <b>Today's P&L:</b> %s%.4f EUR\n\n"+
				"🔋 <b>Charged:</b> %.2f kWh (%d cycles)\n"+
				"   Avg price: %.4f EUR/kWh\n"+
				"   Best price: %.4f EUR/kWh\n\n"+
				"⚡ <b>Discharged:</b> %.2f kWh (%d cycles)\n"+
				"   Avg price: %.4f EUR/kWh\n"+
				"   Best price: %.4f EUR/kWh\n\n"+
				"📊 <b>Cumulative P&L:</b> %s%.4f EUR",
			pnlEmoji,
			data.Date.Format("02 Jan 2006"),
			pnlSign, data.PnLEUR,
			data.ChargedKWh, data.ChargeCycles,
			data.AvgChargePrice,
			data.MinChargePrice,
			data.DischargedKWh, data.DischargeCycles,
			data.AvgDischargePrice,
			data.MaxDischargePrice,
			totalSign, data.TotalPnLEUR,
		)
	}

	return c.SendMessage(ctx, text)
}

// SendStartup sends a startup notification.
func (c *Client) SendStartup(ctx context.Context, serviceName string) error {
	text := fmt.Sprintf("🚀 <b>%s started</b>", serviceName)
	return c.SendMessage(ctx, text)
}

// StatusData contains current status for the /status command.
type StatusData struct {
	State        string
	BatterySOC   int
	CurrentPrice float64
	NextAction   string
	TodayPnL     float64
	TotalPnL     float64
}

// SendStatus sends the current status.
func (c *Client) SendStatus(ctx context.Context, data StatusData) error {
	stateEmoji := "⏸️"
	switch data.State {
	case "charging":
		stateEmoji = "🔋"
	case "discharging":
		stateEmoji = "⚡"
	}

	text := fmt.Sprintf(
		"%s <b>Current Status</b>\n\n"+
			"<b>State:</b> %s\n"+
			"<b>Battery:</b> %d%%\n"+
			"<b>Price:</b> %.4f EUR/kWh\n"+
			"<b>Next:</b> %s\n\n"+
			"<b>Today P&L:</b> %.4f EUR\n"+
			"<b>Total P&L:</b> %.4f EUR",
		stateEmoji,
		data.State,
		data.BatterySOC,
		data.CurrentPrice,
		data.NextAction,
		data.TodayPnL,
		data.TotalPnL,
	)
	return c.SendMessage(ctx, text)
}

// Update represents a Telegram update.
type Update struct {
	UpdateID int64   `json:"update_id"`
	Message  Message `json:"message"`
}

// Message represents a Telegram message.
type Message struct {
	Chat Chat   `json:"chat"`
	Text string `json:"text"`
}

// Chat represents a Telegram chat.
type Chat struct {
	ID int64 `json:"id"`
}

// getUpdatesResponse is the Telegram API response for getUpdates.
type getUpdatesResponse struct {
	OK     bool     `json:"ok"`
	Result []Update `json:"result"`
}

// PollCommands checks for new commands and returns them.
// Returns command strings (e.g., "/status") from the configured chat.
func (c *Client) PollCommands(ctx context.Context) ([]string, error) {
	if !c.enabled {
		return nil, nil
	}

	url := fmt.Sprintf(getUpdatesAPI+"?offset=%d&timeout=1", c.botToken, c.lastUpdateID+1)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result getUpdatesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	var commands []string
	for _, update := range result.Result {
		c.lastUpdateID = update.UpdateID
		// Only process messages from configured chat
		if fmt.Sprintf("%d", update.Message.Chat.ID) == c.chatID {
			if len(update.Message.Text) > 0 && update.Message.Text[0] == '/' {
				commands = append(commands, update.Message.Text)
			}
		}
	}

	return commands, nil
}
