package grpcserver

import (
	"github.com/zeta1999/crypto-exchange-golang/grpc/exchangev1"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
)

// Event represents an order book hook emitted to gRPC stream subscribers.
type Event struct {
	Name string
	Data interface{}
}

// ToUpdate converts known hook events into proto updates.
func (e Event) ToUpdate() *exchangev1.StreamUpdate {
	switch payload := e.Data.(type) {
	case *orderbook.Trade:
		return &exchangev1.StreamUpdate{Payload: &exchangev1.StreamUpdate_Trade{Trade: toProtoTrade(payload)}}
	case *orderbook.Order:
		if e.Name == "cancel" {
			return &exchangev1.StreamUpdate{Payload: &exchangev1.StreamUpdate_OrderEvent{OrderEvent: &exchangev1.OrderEvent{
				Type:       e.Name,
				OrderId:    payload.ID,
				Instrument: payload.Instrument,
			}}}
		}
	}
	return nil
}
