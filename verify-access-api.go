package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/onflow/flow-go/crypto"
	"github.com/onflow/flow-go/crypto/hash"
	"github.com/onflow/flow-go/model/fingerprint"
	"github.com/onflow/flow-go/model/flow"
	"github.com/onflow/flow-go/storage/merkle"
	"github.com/onflow/flow/protobuf/go/flow/access"
	"github.com/onflow/flow/protobuf/go/flow/entities"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	defaultTimeout    = 30 * time.Second
	eventsHeightRange = 250
	latestVersion     = 2
	maxMessageSize    = 100 << 20 // 100MiB
)

var (
	log *zap.SugaredLogger

	waitTimes = []time.Duration{
		time.Second,
		5 * time.Second,
		30 * time.Second,
		// NOTE(tav): We use a sentinel value of -1 to indicate the final wait
		// time.
		-1,
	}
)

// NOTE(tav): Keep these updated with new test cases once new sporks have been
// deployed to mainnet and testnet.
var mainnet = Network{
	ChainID: flow.Mainnet,
	Tests: []*TestCase{
		{
			APIServer:   "access-001.mainnet16.nodes.onflow.org:9000",
			BlockID:     "b785ac55bde24440d3dc77ee7cd820df48be5dbc4a13dee796be6cb1eb58ec64",
			BlockHeight: 27341468,
			ResultID:    "fc5e4a3383f68b0aa22fd5e7f9a6673d8e3fd1876ee3e7bcb8379aeaaef630d7",
			Spork:       16,
			Version:     2,
		},
		{
			APIServer:   "access-001.mainnet17.nodes.onflow.org:9000",
			BlockID:     "56454d4f3986b3883afa07a32b0e13301cea29e33233f1686077069a9f82642b",
			BlockHeight: 27416462,
			ResultID:    "b273956a0a27929f2d6df2227769cf6ed9a9d6dc31894f8f680aceadf7a11472",
			Spork:       17,
			Version:     2,
		},
	},
}

var testnet = Network{
	ChainID: flow.Testnet,
	Tests: []*TestCase{
		{
			APIServer:   "access-001.devnet33.nodes.onflow.org:9000",
			BlockID:     "1cf5668a631489833bd314f500156802843301af55848e676b60afb8b405c8b4",
			BlockHeight: 64904844,
			ResultID:    "6d57cc71d31e254eeab08a19bb10802f3571a2c796d01b77122c077f7ae16469",
			Spork:       33,
			Version:     2,
		},
		{
			APIServer:   "access-001.devnet34.nodes.onflow.org:9000",
			BlockID:     "097d5f0121d090c7b7efaf66493e28b8ecc5b8444427c2e851a3c40214e7c21a",
			BlockHeight: 65197152,
			ResultID:    "fd5b048c1fbe8aceb77aa02b845d2affb2eef646ad711274b935d4c512dc1be6",
			Spork:       34,
			Version:     2,
		},
	},
}

// Network represents the tests on a particular Flow network.
type Network struct {
	ChainID flow.ChainID
	Tests   []*TestCase
}

func (n Network) validateTestConfig() {
	start := n.Tests[0].Spork
	for i, test := range n.Tests {
		test.ChainID = n.ChainID
		if !test.Dynamic {
			if test.Spork == 0 {
				log.Fatalf(
					"Missing Spork value in the %s test at offset %d",
					n.ChainID, i,
				)
			}
			if test.Spork != start+i {
				log.Fatalf(
					"Invalid Spork value found in the test config %s: %d (does not follow from the previous spork)",
					test, test.Version,
				)
			}
		}
		if test.APIServer == "" {
			log.Fatalf(
				"Missing APIServer value in the test config %s",
				test,
			)
		}
		if test.BlockID == "" {
			log.Fatalf(
				"Missing BlockID value in the test config %s",
				test,
			)
		}
		if test.BlockHeight == 0 {
			log.Fatalf(
				"Missing BlockHeight value in the test config %s",
				test,
			)
		}
		if test.Version == 0 {
			log.Fatalf(
				"Missing Version value in the test config %s",
				test,
			)
		}
		if test.Version < 1 || test.Version > latestVersion {
			log.Fatalf(
				"Invalid Version value found in the test config %s: %d",
				test, test.Version,
			)
		}
	}
}

// TestCase represents the config for testing a specific spork.
type TestCase struct {
	APIServer   string
	BlockID     string
	BlockHeight uint64
	ChainID     flow.ChainID // Automatically set from Network.ChainID
	Dynamic     bool         // Should only be set for the dynamically generated test case
	ResultID    string
	Spork       int
	Version     int
}

func (t TestCase) String() string {
	if t.Dynamic {
		if t.ChainID == "" {
			return "to generate the dynamic test case"
		}
		return fmt.Sprintf("for the dynamic test case in %s", t.ChainID)
	}
	return fmt.Sprintf("for spork %d in %s", t.Spork, t.ChainID)
}

func (t TestCase) convertExecutionResult(blockID []byte, result *entities.ExecutionResult) flowExecutionResult {
	exec := flowExecutionResult{
		BlockID:          toFlowIdentifier(result.BlockId),
		ExecutionDataID:  toFlowIdentifier(result.ExecutionDataId),
		PreviousResultID: toFlowIdentifier(result.PreviousResultId),
	}
	for _, chunk := range result.Chunks {
		exec.Chunks = append(exec.Chunks, &flow.Chunk{
			ChunkBody: flow.ChunkBody{
				BlockID:              toFlowIdentifier(result.BlockId),
				CollectionIndex:      uint(chunk.CollectionIndex),
				EventCollection:      toFlowIdentifier(chunk.EventCollection),
				NumberOfTransactions: uint64(chunk.NumberOfTransactions),
				StartState:           flow.StateCommitment(toFlowIdentifier(chunk.StartState)),
				TotalComputationUsed: chunk.TotalComputationUsed,
			},
			EndState: flow.StateCommitment(toFlowIdentifier(chunk.EndState)),
			Index:    chunk.Index,
		})
	}
	for _, ev := range result.ServiceEvents {
		switch ev.Type {
		case flow.ServiceEventSetup:
			setup := &flow.EpochSetup{}
			err := json.Unmarshal(ev.Payload, setup)
			if err != nil {
				log.Fatalf(
					"Failed to decode %q service event in block %x %s: %s",
					ev.Type, blockID, t, err,
				)
			}
			exec.ServiceEvents = append(exec.ServiceEvents, flow.ServiceEvent{
				Event: setup,
				Type:  ev.Type,
			})
		case flow.ServiceEventCommit:
			commit := &flow.EpochCommit{}
			err := json.Unmarshal(ev.Payload, commit)
			if err != nil {
				log.Fatalf(
					"Failed to decode %q service event in block %x %s: %s",
					ev.Type, blockID, t, err,
				)
			}
			exec.ServiceEvents = append(exec.ServiceEvents, flow.ServiceEvent{
				Event: commit,
				Type:  ev.Type,
			})
		default:
			log.Fatalf(
				"Unknown service event type in block %x %s: %q",
				blockID, t, ev.Type,
			)
		}
	}
	return exec
}

func (t TestCase) deriveBlockID(hdr flowHeader) flow.Identifier {
	if hdr.Timestamp.Location() != time.UTC {
		hdr.Timestamp = hdr.Timestamp.UTC()
	}
	dst := struct {
		ChainID            flow.ChainID
		ParentID           flow.Identifier
		Height             uint64
		PayloadHash        flow.Identifier
		Timestamp          uint64
		View               uint64
		ParentVoterIDs     []flow.Identifier
		ParentVoterSigData []byte
		ProposerID         flow.Identifier
	}{
		ChainID:            hdr.ChainID,
		ParentID:           hdr.ParentID,
		Height:             hdr.Height,
		PayloadHash:        hdr.PayloadHash,
		Timestamp:          uint64(hdr.Timestamp.UnixNano()),
		View:               hdr.View,
		ParentVoterIDs:     hdr.ParentVoterIDs,
		ParentVoterSigData: hdr.ParentVoterSigData,
		ProposerID:         hdr.ProposerID,
	}
	return flow.MakeID(dst)
}

func (t TestCase) deriveEventsHash(events []flowEvent) flow.Identifier {
	switch t.Version {
	case 1:
		return deriveEventsHashV1(events)
	case 2:
		return deriveEventsHashV2(events)
	}
	panic("unreachable code")
}

func (t TestCase) deriveExecutionResult(exec flowExecutionResult) flow.Identifier {
	switch t.Version {
	case 1:
		return deriveExecutionResultV1(exec)
	case 2:
		return deriveExecutionResultV2(exec)
	}
	panic("unreachable code")
}

func (t TestCase) getEventHashes(client access.AccessAPIClient, block *entities.Block) ([]flow.Identifier, bool) {
	var err error
	eventHashes := []flow.Identifier{}
	hasEvents := false
	txnIndex := -1
	for _, col := range block.CollectionGuarantees {
		var colResp *access.CollectionResponse
		t.retry(func(ctx context.Context) error {
			colResp, err = client.GetCollectionByID(
				ctx,
				&access.GetCollectionByIDRequest{
					Id: col.CollectionId,
				},
			)
			return err
		}, "collection %x", col.CollectionId)
		colEvents := []flowEvent{}
		for _, txn := range colResp.Collection.TransactionIds {
			var (
				txnResp       *access.TransactionResponse
				txnResultResp *access.TransactionResultResponse
			)
			txnIndex++
			t.retry(func(ctx context.Context) error {
				txnResp, err = client.GetTransaction(
					ctx,
					&access.GetTransactionRequest{
						Id: txn,
					},
				)
				return err
			}, "transaction %x", txn)
			if len(txnResp.Transaction.Payer) == 0 {
				log.Fatalf(
					"Missing payer from transaction %x in block %x within %s",
					txn, block.Id, t,
				)
			}
			t.retry(func(ctx context.Context) error {
				txnResultResp, err = client.GetTransactionResultByIndex(
					ctx,
					&access.GetTransactionByIndexRequest{
						BlockId: block.Id,
						Index:   uint32(txnIndex),
					},
				)
				return err
			}, "transaction result %d", txnIndex)
			for _, event := range txnResultResp.Events {
				colEvents = append(colEvents, flowEvent{
					EventIndex:       event.EventIndex,
					Payload:          event.Payload,
					TransactionID:    toFlowIdentifier(txn),
					TransactionIndex: event.TransactionIndex,
					Type:             flow.EventType(event.Type),
				})
				hasEvents = true
			}
		}
		eventHashes = append(eventHashes, t.deriveEventsHash(colEvents))
	}
	// NOTE(tav): We index any events that might have been generated by the
	// service collection.
	txnIndex++
	var txnResultResp *access.TransactionResultResponse
	t.retry(func(ctx context.Context) error {
		txnResultResp, err = client.GetTransactionResultByIndex(
			ctx,
			&access.GetTransactionByIndexRequest{
				BlockId: block.Id,
				Index:   uint32(txnIndex),
			},
		)
		return err
	}, "transaction result %d", txnIndex)
	colEvents := []flowEvent{}
	for _, event := range txnResultResp.Events {
		colEvents = append(colEvents, flowEvent{
			EventIndex:       event.EventIndex,
			Payload:          event.Payload,
			TransactionID:    toFlowIdentifier(event.TransactionId),
			TransactionIndex: event.TransactionIndex,
			Type:             flow.EventType(event.Type),
		})
		hasEvents = true
	}
	eventHashes = append(eventHashes, t.deriveEventsHash(colEvents))
	return eventHashes, hasEvents
}

func (t TestCase) retry(runner func(ctx context.Context) error, format string, a ...interface{}) {
	i := 0
	msg := fmt.Sprintf(format, a...)
	for {
		log.Infof("Making request to fetch %s %s", msg, t)
		ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
		err := runner(ctx)
		cancel()
		if err == nil {
			break
		}
		wait := waitTimes[i]
		if wait == -1 {
			log.Fatalf(
				"Failed to fetch %s %s: %s",
				msg, t, err,
			)
		} else {
			log.Errorf(
				"Failed to fetch %s %s: %s (retrying after %s)",
				msg, t, err, wait,
			)
		}
		time.Sleep(wait)
		i++
	}
}

type flowEvent struct {
	EventIndex       uint32
	Payload          []byte
	TransactionID    flow.Identifier
	TransactionIndex uint32
	Type             flow.EventType
}

type flowExecutionResult struct {
	BlockID          flow.Identifier
	Chunks           flow.ChunkList
	ExecutionDataID  flow.Identifier
	PreviousResultID flow.Identifier
	ServiceEvents    flow.ServiceEventList
}

type flowHeader struct {
	ChainID            flow.ChainID
	Height             uint64
	ParentID           flow.Identifier
	ParentVoterIDs     []flow.Identifier
	ParentVoterSigData []byte
	PayloadHash        flow.Identifier
	ProposerID         flow.Identifier
	ProposerSigData    []byte
	Timestamp          time.Time
	View               uint64
}

func deriveEventsHashV1(events []flowEvent) flow.Identifier {
	hasher := hash.NewSHA3_256()
	for _, src := range events {
		dst := struct {
			TxID             []byte
			Index            uint32
			Type             string
			TransactionIndex uint32
			Payload          []byte
		}{
			TxID:             src.TransactionID[:],
			Index:            src.EventIndex,
			Type:             string(src.Type),
			TransactionIndex: src.TransactionIndex,
			Payload:          src.Payload,
		}
		_, err := hasher.Write(fingerprint.Fingerprint(dst))
		if err != nil {
			log.Fatalf("Failed to write to sha3-256 hasher: %s", err)
		}
	}
	return toFlowIdentifier(hasher.SumHash())
}

func deriveEventsHashV2(events []flowEvent) flow.Identifier {
	tree, err := merkle.NewTree(flow.IdentifierLen)
	if err != nil {
		log.Fatalf("Failed to instantiate merkle tree: %s", err)
	}
	for _, src := range events {
		dst := struct {
			TxID             []byte
			Index            uint32
			Type             string
			TransactionIndex uint32
			Payload          []byte
		}{
			TxID:             src.TransactionID[:],
			Index:            src.EventIndex,
			Type:             string(src.Type),
			TransactionIndex: src.TransactionIndex,
			Payload:          src.Payload,
		}
		fp := fingerprint.Fingerprint(dst)
		eventID := flow.MakeID(fp)
		_, err = tree.Put(eventID[:], fp)
		if err != nil {
			log.Fatalf("Failed to put event into the merkle tree: %s", err)
		}
	}
	var root flow.Identifier
	copy(root[:], tree.Hash())
	return root
}

func deriveExecutionResultV1(exec flowExecutionResult) flow.Identifier {
	dst := struct {
		PreviousResultID flow.Identifier
		BlockID          flow.Identifier
		Chunks           flow.ChunkList
		ServiceEvents    flow.ServiceEventList
	}{
		BlockID:          exec.BlockID,
		Chunks:           exec.Chunks,
		PreviousResultID: exec.PreviousResultID,
		ServiceEvents:    exec.ServiceEvents,
	}
	return flow.MakeID(dst)
}

func deriveExecutionResultV2(exec flowExecutionResult) flow.Identifier {
	dst := struct {
		PreviousResultID flow.Identifier
		BlockID          flow.Identifier
		Chunks           flow.ChunkList
		ServiceEvents    flow.ServiceEventList
		ExecutionDataID  flow.Identifier
	}{
		BlockID:          exec.BlockID,
		Chunks:           exec.Chunks,
		PreviousResultID: exec.PreviousResultID,
		ServiceEvents:    exec.ServiceEvents,
		ExecutionDataID:  exec.ExecutionDataID,
	}
	return flow.MakeID(dst)
}

func generateTest(addr string, height uint64) Network {
	client := newClient(addr)
	search := false
	test := &TestCase{
		APIServer: addr,
		Dynamic:   true,
		Version:   latestVersion,
	}

	var (
		blockResp  *access.BlockResponse
		eventsResp *access.EventsResponse
		hdrResp    *access.BlockHeaderResponse
		err        error
	)

	test.retry(func(ctx context.Context) error {
		hdrResp, err = client.GetLatestBlockHeader(
			ctx,
			&access.GetLatestBlockHeaderRequest{
				IsSealed: true,
			},
		)
		return err
	}, "latest block header")

	evtype := getEventType(flow.ChainID(hdrResp.Block.ChainId))
	if height == 0 {
		test.retry(func(ctx context.Context) error {
			blockResp, err = client.GetLatestBlock(
				ctx,
				&access.GetLatestBlockRequest{
					IsSealed: true,
				},
			)
			return err
		}, "latest block")
		height = blockResp.Block.Height - 1
		search = true
	}

blockloop:
	for search && height > 0 {
		start := height - eventsHeightRange + 1
		test.retry(func(ctx context.Context) error {
			eventsResp, err = client.GetEventsForHeightRange(
				ctx,
				&access.GetEventsForHeightRangeRequest{
					EndHeight:   height,
					StartHeight: start,
					Type:        evtype,
				},
			)
			return err
		}, "events in range %d-%d", start, height)
		sort.Slice(eventsResp.Results, func(a int, b int) bool {
			return eventsResp.Results[a].BlockHeight > eventsResp.Results[b].BlockHeight
		})
		for _, result := range eventsResp.Results {
			if len(result.Events) > 0 {
				height = result.BlockHeight
				test.BlockHeight = height
				test.BlockID = hex.EncodeToString(result.BlockId)
				break blockloop
			}
		}
		height -= eventsHeightRange
	}

	if !search {
		test.retry(func(ctx context.Context) error {
			blockResp, err = client.GetBlockByHeight(
				ctx,
				&access.GetBlockByHeightRequest{
					Height: height,
				},
			)
			return err
		}, "block %d", height)
		test.BlockHeight = height
		test.BlockID = hex.EncodeToString(blockResp.Block.Id)
	}

	test.retry(func(ctx context.Context) error {
		hdrResp, err = client.GetBlockHeaderByHeight(
			ctx,
			&access.GetBlockHeaderByHeightRequest{
				Height: height,
			},
		)
		return err
	}, "block header %d", height)

	log.Infof(
		"Found candidate block %s at height %d for the dynamic test case",
		test.BlockID, test.BlockHeight,
	)

	test.retry(func(ctx context.Context) error {
		blockResp, err = client.GetBlockByHeight(
			ctx,
			&access.GetBlockByHeightRequest{
				Height: height + 1,
			},
		)
		return err
	}, "descendant block %d", height+1)

	var execResultResp *access.ExecutionResultForBlockIDResponse
	test.retry(func(ctx context.Context) error {
		execResultResp, err = client.GetExecutionResultForBlockID(
			ctx,
			&access.GetExecutionResultForBlockIDRequest{
				BlockId: blockResp.Block.Id,
			},
		)
		return err
	}, "execution result for block %x at %d", blockResp.Block.Id, blockResp.Block.Height)

	test.ResultID = hex.EncodeToString(execResultResp.ExecutionResult.PreviousResultId)
	log.Infof(
		"Found execution result ID %s for block %x at %d",
		test.ResultID, blockResp.Block.Id, blockResp.Block.Height,
	)

	return Network{
		ChainID: flow.ChainID(hdrResp.Block.ChainId),
		Tests:   []*TestCase{test},
	}
}

func getEventType(chainID flow.ChainID) string {
	switch chainID {
	case flow.Mainnet:
		return "A.1654653399040a61.FlowToken.TokensDeposited"
	case flow.Testnet, flow.Canary:
		return "A.7e60df042a9c0868.FlowToken.TokensDeposited"
	default:
		log.Fatalf("Unsupported chain: %s", chainID)
	}
	panic("unreachable code")
}

func initLog() {
	enc := zap.NewDevelopmentEncoderConfig()
	enc.EncodeLevel = zapcore.CapitalColorLevelEncoder
	enc.EncodeTime = zapcore.RFC3339TimeEncoder
	cfg := zap.Config{
		DisableCaller:     true,
		DisableStacktrace: true,
		EncoderConfig:     enc,
		Encoding:          "console",
		ErrorOutputPaths:  []string{"stderr"},
		Level:             zap.NewAtomicLevelAt(zap.InfoLevel),
		OutputPaths:       []string{"stderr"},
		Sampling: &zap.SamplingConfig{
			Initial:    100,
			Thereafter: 100,
		},
	}
	logger, _ := cfg.Build()
	log = logger.Sugar()
	zap.RedirectStdLog(logger)
}

func newClient(addr string) access.AccessAPIClient {
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

func toFlowIdentifier(v []byte) flow.Identifier {
	id := flow.Identifier{}
	copy(id[:], v)
	return id
}

func toIdentifierSlice(v [][]byte) []flow.Identifier {
	xs := make([]flow.Identifier, len(v))
	for i, elem := range v {
		copy(xs[i][:], elem)
	}
	return xs
}

func toSignatureSlice(v [][]byte) []crypto.Signature {
	xs := make([]crypto.Signature, len(v))
	for i, elem := range v {
		sig := make(crypto.Signature, len(elem))
		copy(sig, elem)
		xs[i] = sig
	}
	return xs
}

func verifyBlockHashing(test *TestCase) {
	exitIf := func(err error, format string, a ...interface{}) {
		if err == nil {
			return
		}
		msg := fmt.Sprintf(format, a...)
		log.Fatalf(
			"Failed to %s %s: %s",
			msg, test, err,
		)
	}

	blockID, err := hex.DecodeString(test.BlockID)
	exitIf(err, "decode block ID")

	client := newClient(test.APIServer)

	var hdrResp *access.BlockHeaderResponse
	test.retry(func(ctx context.Context) error {
		hdrResp, err = client.GetBlockHeaderByID(
			ctx,
			&access.GetBlockHeaderByIDRequest{
				Id: blockID,
			},
		)
		return err
	}, "header for block %x", blockID)

	if !bytes.Equal(hdrResp.Block.Id, blockID) {
		log.Fatalf(
			"Mismatching block ID from block header %s: expected %x, got %x",
			test, blockID, hdrResp.Block.Id,
		)
	}

	var blockResp *access.BlockResponse
	test.retry(func(ctx context.Context) error {
		blockResp, err = client.GetBlockByID(
			ctx,
			&access.GetBlockByIDRequest{
				Id:                blockID,
				FullBlockResponse: true,
			},
		)
		return err
	}, "block %x", blockID)

	var execResultResp *access.ExecutionResultForBlockIDResponse
	test.retry(func(ctx context.Context) error {
		execResultResp, err = client.GetExecutionResultForBlockID(
			ctx,
			&access.GetExecutionResultForBlockIDRequest{
				BlockId: blockID,
			},
		)
		return err
	}, "execution result for block %x", blockID)

	eventHashes, _ := test.getEventHashes(client, blockResp.Block)
	result := execResultResp.ExecutionResult
	if len(result.Chunks) != len(eventHashes) {
		log.Fatalf(
			"Execution result for block %x %s contains %d chunks, expected %d",
			blockID, test, len(result.Chunks), len(eventHashes),
		)
	}

	for idx, eventHash := range eventHashes {
		chunk := result.Chunks[idx]
		if !bytes.Equal(chunk.EventCollection, eventHash[:]) {
			log.Fatalf(
				"Got mismatching event hash within chunk at offset %d of block %x %s: expected %x, got %x",
				idx, blockID, test, eventHash[:], chunk.EventCollection,
			)
		}
	}

	exec := test.convertExecutionResult(blockID, result)
	resultID := test.deriveExecutionResult(exec)
	expectedResultID, err := hex.DecodeString(test.ResultID)
	exitIf(err, "decode test result ID")

	if !bytes.Equal(resultID[:], expectedResultID) {
		log.Fatalf(
			"Mismatching result ID %s: expected %x, got %x",
			test, expectedResultID, resultID[:],
		)
	}

	hdr := flowHeader{
		ChainID:            test.ChainID,
		Height:             test.BlockHeight,
		ParentID:           toFlowIdentifier(hdrResp.Block.ParentId),
		ParentVoterIDs:     toIdentifierSlice(hdrResp.Block.ParentVoterIds),
		ParentVoterSigData: hdrResp.Block.ParentVoterSigData,
		PayloadHash:        toFlowIdentifier(hdrResp.Block.PayloadHash),
		ProposerID:         toFlowIdentifier(hdrResp.Block.ProposerId),
		ProposerSigData:    hdrResp.Block.ProposerSigData,
		Timestamp:          hdrResp.Block.Timestamp.AsTime(),
		View:               hdrResp.Block.View,
	}

	blockIDFromHeader := test.deriveBlockID(hdr)
	if !bytes.Equal(blockIDFromHeader[:], blockID) {
		log.Fatalf(
			"Mismatching block ID from header %s: expected %x, got %x for %#v",
			test, blockID, blockIDFromHeader[:], hdr,
		)
	}

	collectionIDs := []flow.Identifier{}
	for _, src := range blockResp.Block.CollectionGuarantees {
		collectionIDs = append(collectionIDs, toFlowIdentifier(src.CollectionId))
	}
	collectionHash := flow.MerkleRoot(collectionIDs...)

	sealIDs := []flow.Identifier{}
	for _, src := range blockResp.Block.BlockSeals {
		seal := &flow.Seal{
			AggregatedApprovalSigs: make([]flow.AggregatedSignature, len(src.AggregatedApprovalSigs)),
			BlockID:                toFlowIdentifier(src.BlockId),
			FinalState:             flow.StateCommitment(toFlowIdentifier(src.FinalState)),
			ResultID:               toFlowIdentifier(src.ResultId),
		}
		for i, sig := range src.AggregatedApprovalSigs {
			seal.AggregatedApprovalSigs[i] = flow.AggregatedSignature{
				SignerIDs:          toIdentifierSlice(sig.SignerIds),
				VerifierSignatures: toSignatureSlice(sig.VerifierSignatures),
			}
		}
		sealIDs = append(sealIDs, seal.ID())
	}
	sealHash := flow.MerkleRoot(sealIDs...)

	receiptIDs := []flow.Identifier{}
	for _, src := range blockResp.Block.ExecutionReceiptMetaList {
		receipt := flow.ExecutionReceiptMeta{
			ExecutorID:        toFlowIdentifier(src.ExecutorId),
			ResultID:          toFlowIdentifier(src.ResultId),
			ExecutorSignature: src.ExecutorSignature,
			Spocks:            toSignatureSlice(src.Spocks),
		}
		receiptIDs = append(receiptIDs, receipt.ID())
	}
	receiptHash := flow.MerkleRoot(receiptIDs...)

	resultIDs := []flow.Identifier{}
	for _, src := range blockResp.Block.ExecutionResultList {
		exec := test.convertExecutionResult(src.BlockId, src)
		resultIDs = append(resultIDs, test.deriveExecutionResult(exec))
	}
	resultHash := flow.MerkleRoot(resultIDs...)

	payloadHash := flow.ConcatSum(collectionHash, sealHash, receiptHash, resultHash)
	if payloadHash != hdr.PayloadHash {
		log.Fatalf(
			"Mismatching payload hash for block %x in %s: expected %x, got %x for %#v",
			blockID, test, hdr.PayloadHash[:], payloadHash[:], blockResp.Block,
		)
	}

	log.Infof(">> Successfully verified block hashing %s", test)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: verify-access-api <host-port-for-access-api-server> [<block-height>]")
		os.Exit(1)
	}
	initLog()
	height := uint64(0)
	if len(os.Args) > 2 {
		val, err := strconv.ParseUint(os.Args[2], 10, 64)
		if err != nil {
			log.Fatalf("Failed to decode block height value %q: %s", os.Args[2], err)
		}
		height = val
	}
	networks := []Network{
		generateTest(os.Args[1], height),
		mainnet,
		testnet,
	}
	for _, network := range networks {
		network.validateTestConfig()
		for _, test := range network.Tests {
			verifyBlockHashing(test)
		}
	}
}
