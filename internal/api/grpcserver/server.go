package grpcserver

import (
	"context"
	"fmt"
	"net"

	"github.com/zeta1999/crypto-exchange-golang/grpc/exchangev1"
	"github.com/zeta1999/crypto-exchange-golang/internal/orderbook"
	"github.com/zeta1999/crypto-exchange-golang/pkg/auth"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// Engine defines the subset of the matching engine the gRPC surface needs.
type Engine interface {
	PlaceLimit(ctx context.Context, ord *orderbook.Order) ([]*orderbook.Trade, *orderbook.Snapshot, error)
	PlaceMarket(ctx context.Context, ord *orderbook.Order) ([]*orderbook.Trade, *orderbook.Snapshot, error)
	Snapshot(symbol string) (*orderbook.Snapshot, error)
}

// Server wires the exchange engine to the generated protobuf service.
type Server struct {
	exchangev1.UnimplementedExchangeServiceServer

	engine    Engine
	validator *auth.TokenValidator
}

// New returns a ready-to-register gRPC handler.
func New(engine Engine, validator *auth.TokenValidator) *Server {
	return &Server{engine: engine, validator: validator}
}

// ListenAndServe runs the gRPC server on the provided address.
func ListenAndServe(ctx context.Context, addr string, srv *Server) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen grpc: %w", err)
	}
	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(srv.authorizeUnary))
	exchangev1.RegisterExchangeServiceServer(grpcServer, srv)
	go func() {
		<-ctx.Done()
		grpcServer.GracefulStop()
	}()
	return grpcServer.Serve(lis)
}

func (s *Server) authorizeUnary(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	token := tokenFromContext(ctx)
	if v, ok := req.(interface{ GetToken() string }); ok && token == "" {
		token = v.GetToken()
	}
	if err := s.validator.Validate(token); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func tokenFromContext(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("authorization"); len(vals) > 0 {
			return vals[0]
		}
		if vals := md.Get("x-api-token"); len(vals) > 0 {
			return vals[0]
		}
	}
	return ""
}

// SubmitOrder handles both limit and market order creation.
func (s *Server) SubmitOrder(ctx context.Context, req *exchangev1.SubmitOrderRequest) (*exchangev1.SubmitOrderResponse, error) {
	ord := &orderbook.Order{
		ID:         req.GetClientId(),
		Instrument: req.GetInstrument(),
		Price:      req.GetPrice(),
		Volume:     req.GetVolume(),
		Side:       orderbook.Side(req.GetSide()),
		IsMarket:   req.GetMarket(),
	}
	var (
		trades []*orderbook.Trade
		snap   *orderbook.Snapshot
		err    error
	)
	if req.GetMarket() {
		trades, snap, err = s.engine.PlaceMarket(ctx, ord)
	} else {
		trades, snap, err = s.engine.PlaceLimit(ctx, ord)
	}
	if err != nil {
		return nil, err
	}
	protoTrades := make([]*exchangev1.Trade, 0, len(trades))
	for _, tr := range trades {
		protoTrades = append(protoTrades, toProtoTrade(tr))
	}
	return &exchangev1.SubmitOrderResponse{
		Trades:   protoTrades,
		Snapshot: toProtoSnapshot(snap),
	}, nil
}

// GetSnapshot exposes market depth for the requested instrument.
func (s *Server) GetSnapshot(ctx context.Context, req *exchangev1.GetSnapshotRequest) (*exchangev1.GetSnapshotResponse, error) {
	snap, err := s.engine.Snapshot(req.GetInstrument())
	if err != nil {
		return nil, err
	}
	return &exchangev1.GetSnapshotResponse{Snapshot: toProtoSnapshot(snap)}, nil
}

func toProtoTrade(t *orderbook.Trade) *exchangev1.Trade {
	if t == nil {
		return nil
	}
	return &exchangev1.Trade{
		BuyOrderId:     t.BuyOrderID,
		SellOrderId:    t.SellOrderID,
		Instrument:     t.Instrument,
		Price:          t.Price,
		Volume:         t.Volume,
		ExecutedAtUnix: t.ExecutedAt.Unix(),
	}
}

func toProtoSnapshot(snap *orderbook.Snapshot) *exchangev1.Snapshot {
	if snap == nil {
		return nil
	}
	proto := &exchangev1.Snapshot{
		Instrument: snap.Instrument,
		BestBid:    snap.BestBid,
		BestAsk:    snap.BestAsk,
	}
	for _, lvl := range snap.Bids {
		proto.Bids = append(proto.Bids, &exchangev1.Level{Price: lvl.Price, Volume: lvl.Volume})
	}
	for _, lvl := range snap.Asks {
		proto.Asks = append(proto.Asks, &exchangev1.Level{Price: lvl.Price, Volume: lvl.Volume})
	}
	if snap.LastTrade != nil {
		proto.LastTrade = toProtoTrade(snap.LastTrade)
	}
	return proto
}
