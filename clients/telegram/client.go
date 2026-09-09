package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	sendMessageAPI       = "https://api.telegram.org/bot%s/sendMessage"
	getUpdatesAPI        = "https://api.telegram.org/bot%s/getUpdates"
	setMyCommandsAPI     = "https://api.telegram.org/bot%s/setMyCommands"
	commandMaxAge        = 30 * time.Second
	updateOffsetFileMode = 0o600
	messageTextLimit     = 4096
)

// Client is a Telegram bot client.
type Client struct {
	botToken        string
	chatID          string
	statePath       string
	httpClient      *http.Client
	enabled         bool
	lastUpdateID    int64
	syncDirectoryFn func(string) error
	pendingDirSyncs []string
}

// New creates a new Telegram client.
// If botToken or chatID is empty, the client will be disabled (no-op).
func New(botToken, chatID, statePath string) (*Client, error) {
	c := &Client{
		botToken:  botToken,
		chatID:    chatID,
		statePath: statePath,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		enabled: botToken != "" && chatID != "",
	}
	if !c.enabled {
		return c, nil
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, fmt.Errorf("read Telegram update offset: %w", err)
	}
	c.lastUpdateID, err = strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse Telegram update offset: %w", err)
	}
	return c, nil
}

// sendMessageRequest is the Telegram API request body.
type sendMessageRequest struct {
	ChatID    string `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode,omitempty"`
}

type botCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

type botCommandScope struct {
	Type   string `json:"type"`
	ChatID string `json:"chat_id"`
}

type setMyCommandsRequest struct {
	Commands []botCommand    `json:"commands"`
	Scope    botCommandScope `json:"scope"`
}

type setMyCommandsResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
}

// RegisterCommands publishes the command menu for the configured private chat.
func (c *Client) RegisterCommands(ctx context.Context) error {
	if !c.enabled {
		return nil
	}

	reqBody := setMyCommandsRequest{
		Commands: []botCommand{
			{Command: "status", Description: "Show battery and trading status"},
			{Command: "discharge", Description: "Start manual discharge (optional watts)"},
			{Command: "auto", Description: "Stop manual discharge and resume automatic control"},
		},
		Scope: botCommandScope{
			Type:   "chat",
			ChatID: c.chatID,
		},
	}
	data, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal Telegram commands: %w", err)
	}

	url := fmt.Sprintf(setMyCommandsAPI, c.botToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return c.redactedError("create Telegram command request", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return c.redactedError("register Telegram commands", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("register Telegram commands: status %d", resp.StatusCode)
	}

	var result setMyCommandsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode Telegram command response: %w", err)
	}
	if !result.OK {
		return fmt.Errorf("register Telegram commands: %s", result.Description)
	}
	return nil
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
		return c.redactedError("create Telegram message request", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return c.redactedError("send Telegram message", err)
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
func (c *Client) SendTradeEnd(ctx context.Context, action string, energyKWh float64, avgPriceEUR float64, endSOC int) error {
	totalValue := energyKWh * avgPriceEUR
	text := fmt.Sprintf(
		"<b>%s completed</b>\nEnergy: %.2f kWh\nAvg price: %.4f EUR/kWh\nTotal value: %.4f EUR\nSOC: %d%%",
		action, energyKWh, avgPriceEUR, totalValue, endSOC,
	)
	return c.SendMessage(ctx, text)
}

// SendError sends an error notification.
func (c *Client) SendError(ctx context.Context, errMsg string) error {
	text := fmt.Sprintf("⚠️ <b>Error</b>\n%s", html.EscapeString(errMsg))
	return c.SendMessage(ctx, text)
}

// EfficiencyData contains measured operational AC round-trip efficiency data.
type EfficiencyData struct {
	Percent     *float64
	Cycles      int
	WindowHours float64
}

func renderMeasuredEfficiency(data EfficiencyData) string {
	const label = "<b>Measured AC round-trip efficiency:</b>"
	if data.Percent == nil {
		return label + " unavailable (waiting for valid measurement; includes standby)"
	}
	return fmt.Sprintf(
		"%s %.1f%% (%d accepted windows, %.1f measurement hours; includes standby)",
		label, *data.Percent, data.Cycles, data.WindowHours,
	)
}

// DailySummaryData contains all data for the daily summary notification.
type DailySummaryData struct {
	Date               time.Time
	PnLEUR             float64
	ChargedKWh         float64
	DischargedKWh      float64
	ChargeCycles       int
	DischargeCycles    int
	SolarChargedKWh    float64
	SolarChargeCycles  int
	AvgChargePrice     float64
	AvgDischargePrice  float64
	MinChargePrice     float64
	MaxDischargePrice  float64
	TotalPnLEUR        float64 // cumulative P&L
	UnpricedKWh        float64
	PnLIncomplete      bool
	TotalPnLIncomplete bool
	MeasuredEfficiency EfficiencyData
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
	dailyPnLLabel := "Today's P&L"
	if data.PnLIncomplete {
		dailyPnLLabel = "Today's known cash flow"
	}
	totalPnLLabel := "Cumulative P&L"
	if data.TotalPnLIncomplete {
		totalPnLLabel = "Cumulative known cash flow"
	}

	var text string
	if data.ChargeCycles == 0 && data.DischargeCycles == 0 && data.SolarChargeCycles == 0 &&
		data.ChargedKWh == 0 && data.DischargedKWh == 0 && data.SolarChargedKWh == 0 && data.PnLEUR == 0 && data.UnpricedKWh == 0 {
		text = fmt.Sprintf(
			"%s <b>Daily Summary - %s</b>\n\n"+
				"No trades today.\n\n"+
				"💰 <b>%s:</b> %s%.4f EUR",
			pnlEmoji,
			data.Date.Format("02 Jan 2006"),
			totalPnLLabel,
			totalSign, data.TotalPnLEUR,
		)
	} else if data.ChargeCycles == 0 && data.DischargeCycles == 0 &&
		data.DischargedKWh == 0 && data.ChargedKWh == data.SolarChargedKWh && data.SolarChargedKWh > 0 {
		text = fmt.Sprintf(
			"%s <b>Daily Summary - %s</b>\n\n"+
				"☀️ <b>Solar charged:</b> %.2f kWh (%d sessions)\n\n"+
				"💰 <b>%s:</b> %s%.4f EUR\n\n"+
				"📊 <b>%s:</b> %s%.4f EUR",
			pnlEmoji,
			data.Date.Format("02 Jan 2006"),
			data.SolarChargedKWh, data.SolarChargeCycles,
			dailyPnLLabel,
			pnlSign, data.PnLEUR,
			totalPnLLabel,
			totalSign, data.TotalPnLEUR,
		)
	} else {
		text = fmt.Sprintf(
			"%s <b>Daily Summary - %s</b>\n\n"+
				"💰 <b>%s:</b> %s%.4f EUR\n\n"+
				"🔋 <b>Charged:</b> %.2f kWh (%d cycles)\n"+
				"   Avg price: %.4f EUR/kWh\n"+
				"   Best price: %.4f EUR/kWh\n",
			pnlEmoji,
			data.Date.Format("02 Jan 2006"),
			dailyPnLLabel,
			pnlSign, data.PnLEUR,
			data.ChargedKWh, data.ChargeCycles,
			data.AvgChargePrice,
			data.MinChargePrice,
		)

		if data.SolarChargeCycles > 0 || data.SolarChargedKWh > 0 {
			text += fmt.Sprintf(
				"\n☀️ <b>Solar charged:</b> %.2f kWh (%d sessions)\n",
				data.SolarChargedKWh, data.SolarChargeCycles,
			)
		}

		text += fmt.Sprintf(
			"\n⚡ <b>Discharged:</b> %.2f kWh (%d cycles)\n"+
				"   Avg price: %.4f EUR/kWh\n"+
				"   Best price: %.4f EUR/kWh\n\n"+
				"📊 <b>%s:</b> %s%.4f EUR",
			data.DischargedKWh, data.DischargeCycles,
			data.AvgDischargePrice,
			data.MaxDischargePrice,
			totalPnLLabel,
			totalSign, data.TotalPnLEUR,
		)
	}
	if data.UnpricedKWh > 0 {
		text += fmt.Sprintf("\n\n⚠️ %.2f kWh could not be priced; cash-flow totals are incomplete.", data.UnpricedKWh)
	}
	text += "\n\n" + renderMeasuredEfficiency(data.MeasuredEfficiency)

	return c.SendMessage(ctx, text)
}

// SendStartup sends a startup notification.
func (c *Client) SendStartup(ctx context.Context, serviceName string) error {
	text := fmt.Sprintf("🚀 <b>%s started</b>", serviceName)
	return c.SendMessage(ctx, text)
}

// StatusData contains current status for the /status command.
type StatusData struct {
	State              string
	BatteryAvailable   bool
	BatterySOC         int
	BatteryPowerW      float64
	CurrentPrice       float64
	CurrentPriceKnown  bool
	NextAction         string
	TodayPnL           float64
	TotalPnL           float64
	TodayPnLIncomplete bool
	TotalPnLIncomplete bool
	MeasuredEfficiency EfficiencyData
}

// SendStatus sends the current status.
func (c *Client) SendStatus(ctx context.Context, data StatusData) error {
	stateEmoji := "⏸️"
	switch data.State {
	case "charging":
		stateEmoji = "🔋"
	case "solar_charging":
		stateEmoji = "☀️"
	case "discharging", "manual_discharging":
		stateEmoji = "⚡"
	}
	batterySOC := "unavailable"
	batteryPower := "unavailable"
	if data.BatteryAvailable {
		batterySOC = fmt.Sprintf("%d%%", data.BatterySOC)
		batteryPower = fmt.Sprintf("%.0f W", data.BatteryPowerW)
	}
	price := "unavailable"
	if data.CurrentPriceKnown {
		price = fmt.Sprintf("%.4f EUR/kWh", data.CurrentPrice)
	}
	todayPnLLabel := "Today P&L"
	if data.TodayPnLIncomplete {
		todayPnLLabel = "Today's known cash flow"
	}
	totalPnLLabel := "Total P&L"
	if data.TotalPnLIncomplete {
		totalPnLLabel = "Total known cash flow"
	}

	text := fmt.Sprintf(
		"%s <b>Current Status</b>\n\n"+
			"<b>State:</b> %s\n"+
			"<b>Battery:</b> %s\n"+
			"<b>Battery power:</b> %s\n"+
			"<b>Price:</b> %s\n"+
			"<b>Next:</b> %s\n\n"+
			"<b>%s:</b> %.4f EUR\n"+
			"<b>%s:</b> %.4f EUR",
		stateEmoji,
		data.State,
		batterySOC,
		batteryPower,
		price,
		data.NextAction,
		todayPnLLabel,
		data.TodayPnL,
		totalPnLLabel,
		data.TotalPnL,
	)
	text += "\n\n" + renderMeasuredEfficiency(data.MeasuredEfficiency)

	return c.SendMessage(ctx, text)
}

// Update represents a Telegram update.
type Update struct {
	UpdateID int64   `json:"update_id"`
	Message  Message `json:"message"`
}

// Message represents a Telegram message.
type Message struct {
	From User   `json:"from"`
	Chat Chat   `json:"chat"`
	Text string `json:"text"`
	Date int64  `json:"date"`
}

// User represents a Telegram message sender.
type User struct {
	ID int64 `json:"id"`
}

// Chat represents a Telegram chat.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// getUpdatesResponse is the Telegram API response for getUpdates.
type getUpdatesResponse struct {
	OK     bool     `json:"ok"`
	Result []Update `json:"result"`
}

// TradingPlanCycle represents a single charge/discharge cycle.
type TradingPlanCycle struct {
	ChargeStart    string
	ChargeEnd      string
	ChargePrice    float64
	DischargeStart string
	DischargeEnd   string
	DischargePrice float64
	ProfitPerKWh   float64
}

// TradingPlanData contains data for trading plan notifications.
type TradingPlanData struct {
	Day                string // Planning-horizon label, currently "horizon".
	Date               time.Time
	SlotsTotal         int
	SlotsAnalyzed      int
	PriceMin           float64
	PriceMax           float64
	IsProfitable       bool
	Cycles             []TradingPlanCycle
	Reason             string // Only shown when the plan is not profitable.
	MinExpectedProfit  float64
	MeasuredEfficiency EfficiencyData
	PlanRetained       bool
	DischargeOnly      bool
	DischargeStart     string
	DischargeEnd       string
	DischargePrice     float64
}

// SendTradingPlan sends a trading plan notification.
func (c *Client) SendTradingPlan(ctx context.Context, data TradingPlanData) error {
	var text string
	efficiencyText := "\n\n" + renderMeasuredEfficiency(data.MeasuredEfficiency)

	dateStr := data.Date.Format("02 Jan 2006")
	dayLabel := "📅"

	if data.DischargeOnly {
		text = fmt.Sprintf(
			"%s <b>Retained Discharge Obligation - %s</b>\n"+
				"<i>%s</i>\n\n"+
				"Grid charging is disabled under the current profitability floor.\n\n"+
				"Discharge: %s - %s @ %.4f EUR/kWh\n"+
				"Configured minimum net profit: %.4f EUR/kWh",
			dayLabel, data.Day, dateStr,
			data.DischargeStart, data.DischargeEnd, data.DischargePrice,
			data.MinExpectedProfit,
		)
		if data.PlanRetained {
			text += "\n\n<i>Active committed-cycle plan retained; refreshed plan pending.</i>"
		}
	} else if !data.IsProfitable {
		text = fmt.Sprintf(
			"%s <b>Trading Plan - %s</b>\n"+
				"<i>%s</i>\n\n"+
				"❌ <b>No profitable opportunities</b>\n\n"+
				"Plan prices: %.4f - %.4f EUR/kWh\n"+
				"Slots analyzed: %d of %d\n\n"+
				"<i>%s</i>\n"+
				"Configured minimum net profit: %.4f EUR/kWh",
			dayLabel, data.Day, dateStr,
			data.PriceMin, data.PriceMax,
			data.SlotsAnalyzed, data.SlotsTotal,
			data.Reason,
			data.MinExpectedProfit,
		)
		if data.PlanRetained {
			text += "\n\n<i>Active committed-cycle plan retained; refreshed plan pending.</i>"
		}
	} else {
		text = fmt.Sprintf(
			"%s <b>Trading Plan - %s</b>\n"+
				"<i>%s</i>\n\n"+
				"✅ <b>%d profitable cycle(s) found</b>\n\n"+
				"Plan prices: %.4f - %.4f EUR/kWh\n"+
				"Configured minimum net profit: %.4f EUR/kWh\n",
			dayLabel, data.Day, dateStr,
			len(data.Cycles),
			data.PriceMin, data.PriceMax,
			data.MinExpectedProfit,
		)
		if data.PlanRetained {
			text += "\n\n<i>Active committed-cycle plan retained; refreshed plan pending.</i>"
		}

		for i, cycle := range data.Cycles {
			cycleText := fmt.Sprintf(
				"\n<b>Cycle %d:</b>\n"+
					"🔋 Charge: %s - %s @ %.4f EUR/kWh\n"+
					"⚡ Discharge: %s - %s @ %.4f EUR/kWh\n"+
					"💰 Expected profit: %.4f EUR/kWh\n",
				i+1,
				cycle.ChargeStart, cycle.ChargeEnd, cycle.ChargePrice,
				cycle.DischargeStart, cycle.DischargeEnd, cycle.DischargePrice,
				cycle.ProfitPerKWh,
			)
			if i == len(data.Cycles)-1 && len(text)+len(cycleText)+len(efficiencyText) <= messageTextLimit {
				text += cycleText
				continue
			}
			remaining := len(data.Cycles) - i
			cycleLabel := "cycles"
			if remaining == 1 {
				cycleLabel = "cycle"
			}
			omittedText := fmt.Sprintf("\n<i>%d %s omitted from this message.</i>", remaining, cycleLabel)
			if len(text)+len(cycleText)+len(omittedText)+len(efficiencyText) > messageTextLimit {
				if len(text)+len(omittedText)+len(efficiencyText) <= messageTextLimit {
					text += omittedText
				}
				break
			}
			text += cycleText
		}
	}
	text += efficiencyText
	return c.SendMessage(ctx, text)
}

// PollCommands checks for new commands and returns them.
// Commands are accepted only from the configured private chat.
func (c *Client) PollCommands(ctx context.Context) ([]string, error) {
	if !c.enabled {
		return nil, nil
	}

	url := fmt.Sprintf(getUpdatesAPI+"?offset=%d&timeout=1", c.botToken, c.lastUpdateID+1)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, c.redactedError("create Telegram updates request", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, c.redactedError("poll Telegram updates", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get Telegram updates: status %d", resp.StatusCode)
	}

	var result getUpdatesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if !result.OK {
		return nil, fmt.Errorf("telegram getUpdates returned ok=false")
	}

	maxUpdateID := c.lastUpdateID
	for _, update := range result.Result {
		maxUpdateID = max(maxUpdateID, update.UpdateID)
	}
	if maxUpdateID > c.lastUpdateID {
		if err := c.persistLastUpdateID(maxUpdateID); err != nil {
			return nil, err
		}
		c.lastUpdateID = maxUpdateID
	}

	now := time.Now()
	var commands []string
	for _, update := range result.Result {
		message := update.Message
		if fmt.Sprintf("%d", message.Chat.ID) != c.chatID ||
			message.Chat.Type != "private" ||
			message.From.ID != message.Chat.ID {
			continue
		}
		sentAt := time.Unix(message.Date, 0)
		if message.Date <= 0 || now.Sub(sentAt) > commandMaxAge {
			continue
		}
		if strings.HasPrefix(message.Text, "/") {
			commands = append(commands, message.Text)
		}
	}

	return commands, nil
}

func (c *Client) redactedError(action string, err error) error {
	message := err.Error()
	if c.botToken != "" {
		message = strings.ReplaceAll(message, c.botToken, "[REDACTED]")
	}
	return fmt.Errorf("%s: %s", action, message)
}

func (c *Client) persistLastUpdateID(updateID int64) error {
	stateDir := filepath.Dir(c.statePath)
	if err := c.flushPendingDirectorySyncs(); err != nil {
		return err
	}
	var missing []string
	for path := filepath.Clean(stateDir); path != "."; path = filepath.Dir(path) {
		info, err := os.Stat(path)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("create Telegram state directory: %s is not a directory", path)
			}
			break
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("inspect Telegram state directory %s: %w", path, err)
		}
		missing = append(missing, path)
		if filepath.Dir(path) == path {
			break
		}
	}
	for i := len(missing) - 1; i >= 0; i-- {
		c.pendingDirSyncs = append(c.pendingDirSyncs, filepath.Dir(missing[i]))
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("create Telegram state directory: %w", err)
	}
	if stateDir != "." && filepath.Dir(stateDir) != stateDir {
		if err := os.Chmod(stateDir, 0o700); err != nil {
			return fmt.Errorf("protect Telegram state directory: %w", err)
		}
	}
	if err := c.flushPendingDirectorySyncs(); err != nil {
		return err
	}
	tmpPath := c.statePath + ".tmp"
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, updateOffsetFileMode)
	if err != nil {
		return fmt.Errorf("open Telegram update offset: %w", err)
	}
	if err := file.Chmod(updateOffsetFileMode); err != nil {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("protect Telegram update offset: %w", err)
	}
	_, writeErr := file.WriteString(strconv.FormatInt(updateID, 10) + "\n")
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("persist Telegram update offset contents: %w", err)
	}
	if err := os.Rename(tmpPath, c.statePath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("persist Telegram update offset: %w", err)
	}
	if err := c.syncDirectory(stateDir); err != nil {
		return fmt.Errorf("sync Telegram state directory: %w", err)
	}
	return nil
}

func (c *Client) flushPendingDirectorySyncs() error {
	for len(c.pendingDirSyncs) > 0 {
		path := c.pendingDirSyncs[0]
		if err := c.syncDirectory(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				c.pendingDirSyncs = c.pendingDirSyncs[1:]
				continue
			}
			return fmt.Errorf("publish Telegram state directory through %s: %w", path, err)
		}
		c.pendingDirSyncs = c.pendingDirSyncs[1:]
	}
	return nil
}

func (c *Client) syncDirectory(path string) error {
	if c.syncDirectoryFn != nil {
		return c.syncDirectoryFn(path)
	}
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open directory %s for sync: %w", path, err)
	}
	return errors.Join(dir.Sync(), dir.Close())
}
