package service

import (
	"context"

	"github.com/foae/marstek-energy-trading/clients/marstek"
	"github.com/foae/marstek-energy-trading/clients/nordpool"
)

// PriceProvider fetches energy prices.
type PriceProvider interface {
	FetchTodayPrices(ctx context.Context) ([]nordpool.Price, error)
	FetchTomorrowPrices(ctx context.Context) ([]nordpool.Price, error)
}

// BatteryController controls the battery and retrieves status.
type BatteryController interface {
	Connect() error
	Close() error
	Discover() (*marstek.DeviceInfo, error)
	GetBatteryStatus() (*marstek.BatteryStatus, error)
	GetESStatus() (*marstek.ESStatus, error)
	Charge(powerW int, timeoutS int) error
	Discharge(powerW int, timeoutS int) error
	SetPassiveMode(power int, cdTime int) error
	Idle() error
}

// Notifier sends notifications.
type Notifier interface {
	Enabled() bool
	SendStartup(ctx context.Context, serviceName string) error
	SendTradeStart(ctx context.Context, action string, price float64, soc int) error
	SendTradeEnd(ctx context.Context, action string, energyKWh float64, avgPrice float64) error
	SendError(ctx context.Context, msg string) error
	PollCommands(ctx context.Context) ([]string, error)
}
