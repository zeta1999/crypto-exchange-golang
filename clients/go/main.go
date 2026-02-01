package main

import (
	"context"
	"log"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/grpc/exchangev1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, "localhost:50051", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial grpc: %v", err)
	}
	defer conn.Close()

	client := exchangev1.NewExchangeServiceClient(conn)
	stream, err := client.StreamOrders(ctx)
	if err != nil {
		log.Fatalf("stream orders: %v", err)
	}

	go func() {
		for {
			update, err := stream.Recv()
			if err != nil {
				log.Printf("stream closed: %v", err)
				return
			}
			switch payload := update.Payload.(type) {
			case *exchangev1.StreamUpdate_Ack:
				log.Printf("ack: %s", payload.Ack)
			case *exchangev1.StreamUpdate_Trade:
				log.Printf("trade price=%.2f volume=%.4f", payload.Trade.GetPrice(), payload.Trade.GetVolume())
			case *exchangev1.StreamUpdate_Snapshot:
				snap := payload.Snapshot
				log.Printf("snapshot %s bid=%.2f ask=%.2f", snap.GetInstrument(), snap.GetBestBid(), snap.GetBestAsk())
			case *exchangev1.StreamUpdate_Error:
				log.Printf("error: %s", payload.Error)
			case *exchangev1.StreamUpdate_OrderEvent:
				evt := payload.OrderEvent
				log.Printf("order event: %s %s %s", evt.GetType(), evt.GetInstrument(), evt.GetOrderId())
			}
		}
	}()

	order := &exchangev1.OrderCommand{
		Instrument: "BTC-USD",
		Price:      42000,
		Volume:     0.1,
		Side:       "buy",
		ClientId:   "go-stream",
	}
	if err := stream.Send(&exchangev1.CommandEnvelope{
		Token:   "dev-secret-token",
		Payload: &exchangev1.CommandEnvelope_Order{Order: order},
	}); err != nil {
		log.Fatalf("send order: %v", err)
	}
	if err := stream.Send(&exchangev1.CommandEnvelope{
		Token:   "dev-secret-token",
		Payload: &exchangev1.CommandEnvelope_RequestSnapshot{RequestSnapshot: "BTC-USD"},
	}); err != nil {
		log.Fatalf("request snapshot: %v", err)
	}
	if err := stream.Send(&exchangev1.CommandEnvelope{
		Token: "dev-secret-token",
		Payload: &exchangev1.CommandEnvelope_Cancel{Cancel: &exchangev1.CancelCommand{
			Instrument: "BTC-USD",
			OrderId:    "go-stream",
		}},
	}); err != nil {
		log.Fatalf("send cancel: %v", err)
	}
	time.Sleep(3 * time.Second)
	stream.CloseSend()
}
