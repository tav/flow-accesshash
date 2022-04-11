package api

import (
	"context"
	"fmt"
	"time"

	"github.com/onflow/flow/protobuf/go/flow/access"
	"github.com/tav/flow-accesshash/pkg/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	defaultTimeout = 30 * time.Second
	maxMessageSize = 100 << 20 // 100MiB
)

var (
	waitTimes = []time.Duration{
		time.Second,
		5 * time.Second,
		30 * time.Second,
		// NOTE(tav): We use a sentinel value of -1 to indicate the final wait
		// time.
		-1,
	}
)

func NewClient(addr string) access.AccessAPIClient {
	conn, err := grpc.DialContext(
		context.Background(),
		addr,
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMessageSize)),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		log.Fatalf("Failed to dial Access API server %s: %s", addr, err)
	}
	return access.NewAccessAPIClient(conn)
}

func Retry(runner func(ctx context.Context) error, format string, a ...interface{}) {
	i := 0
	msg := fmt.Sprintf(format, a...)
	for {
		log.Infof("Making request to fetch %s", msg)
		ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
		err := runner(ctx)
		cancel()
		if err == nil {
			break
		}
		wait := waitTimes[i]
		if wait == -1 {
			log.Fatalf(
				"Failed to fetch %s: %s",
				msg, err,
			)
		} else {
			log.Errorf(
				"Failed to fetch %s: %s (retrying after %s)",
				msg, err, wait,
			)
		}
		time.Sleep(wait)
		i++
	}
}
