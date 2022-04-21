package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/onflow/flow-go/engine/common/rpc/convert"
	"github.com/onflow/flow/protobuf/go/flow/access"
	"github.com/tav/flow-accesshash/pkg/api"
	"github.com/tav/flow-accesshash/pkg/log"
)

func findExecutionResultIDs(client access.AccessAPIClient, height uint64) {
	var (
		blockResp      *access.BlockResponse
		err            error
		execResultResp *access.ExecutionResultForBlockIDResponse
		hdrResp        *access.BlockHeaderResponse
		nextBlockID    []byte
		nextBlockResp  *access.BlockResponse
		results        []string
	)

	api.Retry(func(ctx context.Context) error {
		hdrResp, err = client.GetBlockHeaderByHeight(
			ctx,
			&access.GetBlockHeaderByHeightRequest{
				Height: height,
			},
		)
		return err
	}, "header for block at height %d", height)

	blockID := hdrResp.Block.Id
	log.Infof("Block %x found at height %d", blockID, height)

	api.Retry(func(ctx context.Context) error {
		execResultResp, err = client.GetExecutionResultForBlockID(
			ctx,
			&access.GetExecutionResultForBlockIDRequest{
				BlockId: blockID,
			},
		)
		return err
	}, "execution result for block %x", blockID)

	exec, err := convert.MessageToExecutionResult(execResultResp.ExecutionResult)
	if err != nil {
		log.Fatalf(
			"Failed to convert execution result response from the Access API: %s",
			err,
		)
	}

	results = append(results, fmt.Sprintf(
		"Execution result ID %s derived from GetExecutionResultForBlockID response for block %x at height %d",
		exec.ID(), blockID, height,
	))

	sealedHeight := height + 1
findseal:
	for {
		api.Retry(func(ctx context.Context) error {
			blockResp, err = client.GetBlockByHeight(
				ctx,
				&access.GetBlockByHeightRequest{
					FullBlockResponse: true,
					Height:            sealedHeight,
				},
			)
			return err
		}, "block at height %d", sealedHeight)
		if sealedHeight == height+1 {
			nextBlockID = blockResp.Block.Id
			nextBlockResp = blockResp
		}
		for _, seal := range blockResp.Block.BlockSeals {
			if bytes.Equal(seal.BlockId, blockID) {
				results = append(results, fmt.Sprintf(
					"Execution result ID %x found in seal in block %x at height %d",
					seal.ResultId, blockResp.Block.Id, blockResp.Block.Height,
				))
				break findseal
			}
		}
		sealedHeight++
	}

	api.Retry(func(ctx context.Context) error {
		execResultResp, err = client.GetExecutionResultForBlockID(
			ctx,
			&access.GetExecutionResultForBlockIDRequest{
				BlockId: nextBlockID,
			},
		)
		return err
	}, "execution result for block %x", nextBlockID)

	results = append(results, fmt.Sprintf(
		"Execution result ID %x specified as PreviousResultID in GetExecutionResultForBlockID response for block %x at height %d",
		execResultResp.ExecutionResult.PreviousResultId,
		nextBlockResp.Block.Id, nextBlockResp.Block.Height,
	))

	for _, result := range results {
		log.Infof("👀  %s", result)
	}
}

func main() {
	if len(os.Args) != 3 {
		fmt.Println("Usage: execution-result-ids <host-port-for-access-api-server> <block-height>")
		os.Exit(1)
	}
	client := api.NewClient(os.Args[1])
	height, err := strconv.ParseUint(os.Args[2], 10, 64)
	if err != nil {
		log.Fatalf("Failed to decode block height value %q: %s", os.Args[2], err)
	}
	findExecutionResultIDs(client, height)
}
