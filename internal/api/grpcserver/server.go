package grpcserver

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"

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
	CancelOrder(ctx context.Context, instrument, orderID string) (*orderbook.Order, error)
}

// Server wires the exchange engine to the generated protobuf service.
type Server struct {
	exchangev1.UnimplementedExchangeServiceServer

	engine     Engine
	validator  *auth.TokenValidator
	events     <-chan Event
	watcherSeq atomic.Int64
	watchers   sync.Map // id -> chan *exchangev1.StreamUpdate
}

// New returns a ready-to-register gRPC handler.
func New(engine Engine, validator *auth.TokenValidator, events <-chan Event) *Server {
	srv := &Server{engine: engine, validator: validator, events: events}
	if events != nil {
		go srv.consumeEvents()
	}
	return srv
}

func (s *Server) consumeEvents() {
	for evt := range s.events {
		update := evt.ToUpdate()
		if update == nil {
			continue
		}
		s.watchers.Range(func(key, value interface{}) bool {
			ch := value.(chan *exchangev1.StreamUpdate)
			select {
			case ch <- update:
			default:
			}
			return true
		})
	}
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
	ord := orderFromProto(req.GetOrder())
	if ord == nil {
		return nil, fmt.Errorf("order payload required")
	}
	token := tokenFromContext(ctx)
	if token == "" {
		token = req.GetToken()
	}
	if err := s.validator.Validate(token); err != nil {
		return nil, err
	}
	var (
		trades []*orderbook.Trade
		snap   *orderbook.Snapshot
		err    error
	)
	if ord.IsMarket {
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

// StreamOrders multiplexes client commands with real-time book updates.
func (s *Server) StreamOrders(stream exchangev1.ExchangeService_StreamOrdersServer) error {
	ctx := stream.Context()
	id, updates := s.subscribe()
	defer s.unsubscribe(id)

	cmdCh := make(chan *exchangev1.CommandEnvelope, 8)
	errCh := make(chan error, 1)
	go func() {
		for {
			req, err := stream.Recv()
			if err != nil {
				errCh <- err
				close(cmdCh)
				return
			}
			cmdCh <- req
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			if err == nil || err == io.EOF {
				return nil
			}
			return err
		case update, ok := <-updates:
			if !ok {
				return nil
			}
			if update == nil {
				continue
			}
			if err := stream.Send(update); err != nil {
				return err
			}
		case cmd, ok := <-cmdCh:
			if !ok {
				return nil
			}
			if err := s.handleCommand(ctx, stream, cmd); err != nil {
				return err
			}
		}
	}
}

func (s *Server) handleCommand(ctx context.Context, stream exchangev1.ExchangeService_StreamOrdersServer, env *exchangev1.CommandEnvelope) error {
	if env == nil {
		return stream.Send(errorUpdate("empty command"))
	}
	token := tokenFromContext(ctx)
	if token == "" {
		token = env.GetToken()
	}
	if err := s.validator.Validate(token); err != nil {
		return stream.Send(errorUpdate("unauthorized"))
	}
	switch payload := env.Payload.(type) {
	case *exchangev1.CommandEnvelope_Order:
		return s.handleOrderCommand(ctx, stream, payload.Order)
	case *exchangev1.CommandEnvelope_Cancel:
		return s.handleCancelCommand(ctx, stream, payload.Cancel)
	case *exchangev1.CommandEnvelope_RequestSnapshot:
		snap, err := s.engine.Snapshot(payload.RequestSnapshot)
		if err != nil {
			return stream.Send(errorUpdate(err.Error()))
		}
		return stream.Send(&exchangev1.StreamUpdate{Payload: &exchangev1.StreamUpdate_Snapshot{Snapshot: toProtoSnapshot(snap)}})
	default:
		return stream.Send(errorUpdate("unknown command"))
	}
}

func (s *Server) handleOrderCommand(ctx context.Context, stream exchangev1.ExchangeService_StreamOrdersServer, cmd *exchangev1.OrderCommand) error {
	ord := orderFromProto(cmd)
	if ord == nil {
		return stream.Send(errorUpdate("invalid order"))
	}
	var (
		trades []*orderbook.Trade
		snap   *orderbook.Snapshot
		err    error
	)
	if ord.IsMarket {
		trades, snap, err = s.engine.PlaceMarket(ctx, ord)
	} else {
		trades, snap, err = s.engine.PlaceLimit(ctx, ord)
	}
	if err != nil {
		return stream.Send(errorUpdate(err.Error()))
	}
	if err := stream.Send(ackUpdate(fmt.Sprintf("order:%s", ord.ID))); err != nil {
		return err
	}
	for _, tr := range trades {
		if err := stream.Send(&exchangev1.StreamUpdate{Payload: &exchangev1.StreamUpdate_Trade{Trade: toProtoTrade(tr)}}); err != nil {
			return err
		}
	}
	if snap != nil {
		return stream.Send(&exchangev1.StreamUpdate{Payload: &exchangev1.StreamUpdate_Snapshot{Snapshot: toProtoSnapshot(snap)}})
	}
	return nil
}

func (s *Server) handleCancelCommand(ctx context.Context, stream exchangev1.ExchangeService_StreamOrdersServer, cmd *exchangev1.CancelCommand) error {
	if cmd == nil || cmd.GetInstrument() == "" || cmd.GetOrderId() == "" {
		return stream.Send(errorUpdate("invalid cancel"))
	}
	ord, err := s.engine.CancelOrder(ctx, cmd.GetInstrument(), cmd.GetOrderId())
	if err != nil {
		return stream.Send(errorUpdate(err.Error()))
	}
	event := &exchangev1.OrderEvent{Type: "cancel", OrderId: cmd.GetOrderId(), Instrument: cmd.GetInstrument()}
	if ord != nil && ord.Instrument != "" {
		event.Instrument = ord.Instrument
	}
	return stream.Send(&exchangev1.StreamUpdate{Payload: &exchangev1.StreamUpdate_OrderEvent{OrderEvent: event}})
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

func (s *Server) subscribe() (int64, chan *exchangev1.StreamUpdate) {
	ch := make(chan *exchangev1.StreamUpdate, 64)
	id := s.watcherSeq.Add(1)
	s.watchers.Store(id, ch)
	return id, ch
}

func (s *Server) unsubscribe(id int64) {
	if value, ok := s.watchers.Load(id); ok {
		if ch, ok := value.(chan *exchangev1.StreamUpdate); ok {
			close(ch)
		}
		s.watchers.Delete(id)
	}
}

func ackUpdate(msg string) *exchangev1.StreamUpdate {
	return &exchangev1.StreamUpdate{Payload: &exchangev1.StreamUpdate_Ack{Ack: msg}}
}

func errorUpdate(msg string) *exchangev1.StreamUpdate {
	return &exchangev1.StreamUpdate{Payload: &exchangev1.StreamUpdate_Error{Error: msg}}
}

func orderFromProto(cmd *exchangev1.OrderCommand) *orderbook.Order {
	if cmd == nil {
		return nil
	}
	return &orderbook.Order{
		ID:         cmd.GetClientId(),
		Instrument: cmd.GetInstrument(),
		Price:      cmd.GetPrice(),
		Volume:     cmd.GetVolume(),
		Side:       orderbook.Side(cmd.GetSide()),
		IsMarket:   cmd.GetMarket(),
	}
}
