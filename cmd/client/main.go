// Command client demonstrates the native grpc-go client: it dials the
// headless Service through the custom order_random load-balancing policy,
// applies the retry service config, and places one order.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	orderv1 "github.com/acme/order-engine/gen/order/v1"
	"github.com/acme/order-engine/internal/grpcclient"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "client: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	target := flag.String("target", grpcclient.DefaultTarget,
		"gRPC target; the dns:/// headless-service form exposes every ready pod to the picker")
	token := flag.String("token", "token-123", "bearer token (dev/test static validator)")
	idempotencyKey := flag.String("idempotency-key", "",
		"idempotency key; keep it stable when retrying one logical order (random per run if empty)")
	amountMinor := flag.Int64("amount-minor", 1999, "order amount in minor currency units")
	currency := flag.String("currency", "USD", "ISO 4217 currency code")
	timeout := flag.Duration("timeout", 10*time.Second, "per-call deadline")
	flag.Parse()

	key := *idempotencyKey
	if key == "" {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return fmt.Errorf("generate idempotency key: %w", err)
		}
		key = "client-" + hex.EncodeToString(suffix[:])
	}

	conn, err := grpcclient.New(*target)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := orderv1.NewOrderServiceClient(conn)

	// Every call carries a deadline; retries happen within it.
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+*token)

	resp, err := client.CreateOrder(ctx, &orderv1.CreateOrderRequest{
		IdempotencyKey: key,
		AmountMinor:    *amountMinor,
		Currency:       *currency,
	})
	if err != nil {
		st := status.Convert(err)
		fmt.Fprintf(os.Stderr, "CreateOrder failed: code=%s message=%q\n", st.Code(), st.Message())
		for _, detail := range st.Details() {
			if info, ok := detail.(*errdetails.ErrorInfo); ok {
				fmt.Fprintf(os.Stderr, "  ErrorInfo: domain=%s reason=%s\n", info.GetDomain(), info.GetReason())
			}
		}
		return err
	}

	order := resp.GetOrder()
	fmt.Printf("order placed:\n  id:              %s\n  account:         %s\n  idempotency_key: %s\n  amount_minor:    %d %s\n  created_at:      %s\n",
		order.GetId(), order.GetAccountId(), order.GetIdempotencyKey(),
		order.GetAmountMinor(), order.GetCurrency(), order.GetCreatedAt().AsTime().Format(time.RFC3339Nano))
	return nil
}
