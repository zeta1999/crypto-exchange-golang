package main

import (
	"context"
	"log"
	"time"

	"github.com/zeta1999/crypto-exchange-golang/grpc/exchangev1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, "localhost:50051", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial grpc: %v", err)
	}
	defer conn.Close()

	client := exchangev1.NewExchangeServiceClient(conn)
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "dev-secret-token")
	resp, err := client.SubmitOrder(ctx, &exchangev1.SubmitOrderRequest{
		Instrument: "BTC-USD",
		Price:      42000,
		Volume:     0.1,
		Side:       "buy",
		ClientId:   "go-demo",
	})
	if err != nil {
		log.Fatalf("submit order: %v", err)
	}
	log.Printf("trades=%d bestBid=%.2f bestAsk=%.2f", len(resp.GetTrades()), resp.GetSnapshot().GetBestBid(), resp.GetSnapshot().GetBestAsk())
}
