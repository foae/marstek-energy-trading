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
	GetBatteryStatusContext(ctx context.Context) (*marstek.BatteryStatus, error)
	GetESStatus(ctx context.Context) (*marstek.ESStatus, error)
	GetBatteryPower(ctx context.Context) (float64, error)
	ChargeContext(ctx context.Context, powerW int, timeoutS int) error
	DischargeContext(ctx context.Context, powerW int, timeoutS int) error
	SetPassiveModeContext(ctx context.Context, power int, cdTime int) error
	IdleContext(ctx context.Context) error
}

// MeterReader reads power data from a smart meter.
type MeterReader interface {
	Enabled() bool
	GetActivePowerW() (float64, error)
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
