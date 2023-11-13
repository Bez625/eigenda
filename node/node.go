package node

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	"github.com/Layr-Labs/eigenda/common/pubip"
	gethcommon "github.com/ethereum/go-ethereum/common"

	"github.com/Layr-Labs/eigenda/api/grpc/node"
	"github.com/Layr-Labs/eigenda/common"
	"github.com/Layr-Labs/eigenda/common/geth"
	"github.com/Layr-Labs/eigenda/core"
	"github.com/Layr-Labs/eigenda/core/encoding"
	"github.com/Layr-Labs/eigenda/core/eth"
	"github.com/Layr-Labs/eigenda/core/indexer"
	"github.com/Layr-Labs/eigensdk-go/chainio/constructor"
	"github.com/Layr-Labs/eigensdk-go/metrics/collectors/economic"
	rpccalls "github.com/Layr-Labs/eigensdk-go/metrics/collectors/rpc_calls"
	"github.com/Layr-Labs/eigensdk-go/nodeapi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/gammazero/workerpool"
)

const (
	// The percentage of time in garbage collection in a GC cycle.
	gcPercentageTime = 0.1
)

type Node struct {
	Config                  *Config
	Logger                  common.Logger
	KeyPair                 *core.KeyPair
	Metrics                 *Metrics
	NodeApi                 *nodeapi.NodeApi
	Store                   *Store
	ChainState              core.ChainState
	Validator               core.ChunkValidator
	Transactor              core.Transactor
	PubIPProvider           pubip.Provider
	OperatorSocketsFilterer indexer.OperatorSocketsFilterer

	mu            sync.Mutex
	CurrentSocket string
}

// NewNode creates a new Node with the provided config.
func NewNode(config *Config, pubIPProvider pubip.Provider, logger common.Logger) (*Node, error) {
	// Setup metrics
	sdkClients, err := buildSdkClients(config, logger)
	if err != nil {
		return nil, err
	}
	metrics := NewMetrics(sdkClients.Metrics, sdkClients.PrometheusRegistry, logger, ":"+config.MetricsPort)
	rpcCallsCollector := rpccalls.NewCollector(AppName, sdkClients.PrometheusRegistry)

	// Generate BLS keys
	keyPair, err := core.MakeKeyPairFromString(config.PrivateBls)
	if err != nil {
		return nil, err
	}

	config.ID = keyPair.GetPubKeyG1().GetOperatorID()

	// Make sure config folder exists.
	err = os.MkdirAll(config.DbPath, os.ModePerm)
	if err != nil {
		return nil, fmt.Errorf("could not create db directory at %s: %w", config.DbPath, err)
	}

	client, err := geth.NewInstrumentedEthClient(config.EthClientConfig, rpcCallsCollector, logger)
	if err != nil {
		return nil, fmt.Errorf("cannot create chain.Client: %w", err)
	}

	// Create Transactor
	tx, err := eth.NewTransactor(logger, client, config.BLSOperatorStateRetrieverAddr, config.EigenDAServiceManagerAddr)
	if err != nil {
		return nil, err
	}

	// Create ChainState Client
	cst := eth.NewChainState(tx, client)

	// Setup Node Api
	nodeApi := nodeapi.NewNodeApi(AppName, SemVer, "localhost:"+config.NodeApiPort, logger)

	// Make validator
	enc, err := encoding.NewEncoder(config.EncoderConfig)
	if err != nil {
		return nil, err
	}
	asgn := &core.StdAssignmentCoordinator{}
	validator := core.NewChunkValidator(enc, asgn, cst, config.ID)

	// Create new store

	// Resolve the BLOCK_STALE_MEASURE and STORE_DURATION_BLOCKS.
	var blockStaleMeasure, storeDurationBlocks uint32
	if config.EnableTestMode && config.OverrideBlockStaleMeasure > 0 {
		blockStaleMeasure = uint32(config.OverrideBlockStaleMeasure)
	} else {
		staleMeasure, err := tx.GetBlockStaleMeasure(context.Background())
		if err != nil {
			return nil, fmt.Errorf("failed to get BLOCK_STALE_MEASURE: %w", err)
		}
		blockStaleMeasure = staleMeasure
	}
	if config.EnableTestMode && config.OverrideStoreDurationBlocks > 0 {
		storeDurationBlocks = uint32(config.OverrideStoreDurationBlocks)
	} else {
		storeDuration, err := tx.GetStoreDurationBlocks(context.Background())
		if err != nil {
			return nil, fmt.Errorf("failed to get STORE_DURATION_BLOCKS: %w", err)
		}
		storeDurationBlocks = storeDuration
	}
	store, err := NewLevelDBStore(config.DbPath+"/chunk", logger, metrics, blockStaleMeasure, storeDurationBlocks)
	if err != nil {
		return nil, fmt.Errorf("failed to create new store: %w", err)
	}

	eigenDAServiceManagerAddr := gethcommon.HexToAddress(config.EigenDAServiceManagerAddr)
	socketsFilterer, err := indexer.NewOperatorSocketsFilterer(eigenDAServiceManagerAddr, client)
	if err != nil {
		return nil, fmt.Errorf("failed to create new operator sockets filterer: %w", err)
	}

	return &Node{
		Config:                  config,
		Logger:                  logger,
		KeyPair:                 keyPair,
		Metrics:                 metrics,
		NodeApi:                 nodeApi,
		Store:                   store,
		ChainState:              cst,
		Transactor:              tx,
		Validator:               validator,
		PubIPProvider:           pubIPProvider,
		OperatorSocketsFilterer: socketsFilterer,
	}, nil
}

// Starts the Node. If the node is not registered, register it on chain, otherwise just
// update its socket on chain.
func (n *Node) Start(ctx context.Context) error {
	if n.Config.EnableMetrics {
		n.Metrics.Start()
		n.Logger.Info("Enabled metrics", "socket", n.Metrics.socketAddr)
	}
	if n.Config.EnableNodeApi {
		n.NodeApi.Start()
		n.Logger.Info("Enabled node api", "port", n.Config.NodeApiPort)
	}

	go n.expireLoop()

	// Build the socket based on the hostname/IP provided in the CLI
	socket := string(core.MakeOperatorSocket(n.Config.Hostname, n.Config.DispersalPort, n.Config.RetrievalPort))
	n.Logger.Info("Registering node with socket", "socket", socket)
	if n.Config.RegisterNodeAtStart {
		n.Logger.Debug("Registering node on chain with the following parameters:", "operatorId",
			n.Config.ID, "hostname", n.Config.Hostname, "dispersalPort", n.Config.DispersalPort,
			"retrievalPort", n.Config.RetrievalPort, "churnerUrl", n.Config.ChurnerUrl, "quorumIds", n.Config.QuorumIDList)
		socket := string(core.MakeOperatorSocket(n.Config.Hostname, n.Config.DispersalPort, n.Config.RetrievalPort))
		operator := &Operator{
			Socket:     socket,
			Timeout:    10 * time.Second,
			KeyPair:    n.KeyPair,
			OperatorId: n.Config.ID,
			QuorumIDs:  n.Config.QuorumIDList,
		}
		err := RegisterOperator(ctx, operator, n.Transactor, n.Config.ChurnerUrl, n.Logger)
		if err != nil {
			return fmt.Errorf("failed to register the operator: %w", err)
		}
	}

	n.CurrentSocket = socket
	// Start the Node IP updater only if the PUBLIC_IP_PROVIDER is greater than 0.
	if n.Config.PubIPCheckInterval > 0 {
		go n.checkRegisteredNodeIpOnChain(ctx)
		go n.checkCurrentNodeIp(ctx)
	}

	return nil
}

// The expireLoop is a loop that is run once per configured second(s) while the node
// is running. It scans for expirated batches and remove them from the local database.
func (n *Node) expireLoop() {
	ticker := time.NewTicker(time.Duration(n.Config.ExpirationPollIntervalSec) * time.Second)
	defer ticker.Stop()

	for {
		<-ticker.C

		// We cap the time the deletion function can run, to make sure there is no overlapping
		// between loops and the garbage collection doesn't take too much resource.
		// The heuristic is to cap the GC time to a percentage of the poll interval, but at
		// least have 1 second.
		timeLimitSec := uint64(math.Max(float64(n.Config.ExpirationPollIntervalSec)*gcPercentageTime, 1.0))
		numBatchesDeleted, err := n.Store.DeleteExpiredEntries(time.Now().Unix(), timeLimitSec)
		n.Logger.Info("GC cycle deleted", "num batches", numBatchesDeleted)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				n.Logger.Error("GC cycle ContextDeadlineExceeded", "time limit (sec)", timeLimitSec)
			} else {
				n.Logger.Error("GC cycle had failed to delete some expired entries", "err", err)
			}
		}
	}
}

// ProcessBatch validates the batch is correct, stores data into the node's Store, and then returns a signature for the entire batch.
//
// The batch will be itemized into batch header, header and chunks of each blob in the batch. These items will
// be stored atomically to the database.
//
// Notes:
//   - If the batch is stored already, it's no-op to store it more than once
//   - If the batch is stored, but the processing fails after that, these data items will not be rollback
//   - These data items will be garbage collected eventually when they become stale.
func (n *Node) ProcessBatch(ctx context.Context, header *core.BatchHeader, blobs []*core.BlobMessage, rawBlobs []*node.Blob) (*core.Signature, error) {
	start := time.Now()
	log := n.Logger

	// Measure num batches received and its size in bytes
	batchSize := 0
	for _, blob := range blobs {
		batchSize += blob.BlobHeader.EncodedSizeAllQuorums()
	}
	n.Metrics.AcceptBatches("received", batchSize)

	batchHeaderHash, err := header.GetBatchHeaderHash()
	if err != nil {
		return nil, err
	}

	// Store the batch.
	// Run this in a goroutine so we can parallelize the batch storing and batch
	// verifaction work.
	// This should be able to improve latency without needing more CPUs, because batch
	// storing is an IO operation.
	type storeResult struct {
		// Whether StoreBatch failed.
		err error

		// The keys that are stored to database for a single batch.
		// Undefined if the err is not nil or err is ErrBatchAlreadyExist.
		keys *[][]byte
	}
	storeChan := make(chan storeResult)
	go func(n *Node) {
		start := time.Now()
		keys, err := n.Store.StoreBatch(ctx, header, blobs, rawBlobs)
		if err != nil {
			// If batch already exists, we don't store it again, but we should not
			// error out in such case.
			if err == ErrBatchAlreadyExist {
				storeChan <- storeResult{err: nil, keys: nil}
			} else {
				storeChan <- storeResult{err: fmt.Errorf("failed to store batch: %w", err), keys: nil}
			}
			return
		}
		n.Metrics.AcceptBatches("stored", batchSize)
		n.Metrics.ObserveLatency("StoreChunks", "stored", float64(time.Since(start).Milliseconds()))
		n.Logger.Debug("Store batch took", "duration:", time.Since(start))
		storeChan <- storeResult{err: nil, keys: keys}
	}(n)

	// Validate batch.
	stageTimer := time.Now()
	err = n.ValidateBatch(ctx, header, blobs)
	if err != nil {
		// If we have already stored the batch into database, but it's not valid, we
		// revert all the keys for that batch.
		result := <-storeChan
		if result.keys != nil {
			if !n.Store.DeleteKeys(ctx, result.keys) {
				log.Error("Failed to delete the invalid batch that should be rolled back", "batchHeaderHash", batchHeaderHash)
			}
		}
		return nil, fmt.Errorf("failed to validate batch: %w", err)
	}
	n.Metrics.AcceptBatches("validated", batchSize)
	n.Metrics.ObserveLatency("StoreChunks", "validated", float64(time.Since(stageTimer).Milliseconds()))
	log.Debug("Validate batch took", "duration:", time.Since(stageTimer))

	// Before we sign the batch, we should first complete the batch storing successfully.
	result := <-storeChan
	if result.err != nil {
		return nil, err
	}

	// Sign batch header hash if all validation checks pass and data items are writen to database.
	stageTimer = time.Now()
	sig := n.KeyPair.SignMessage(batchHeaderHash)
	log.Trace("Signed batch header hash", "pubkey", hexutil.Encode(n.KeyPair.GetPubKeyG2().Serialize()))
	n.Metrics.AcceptBatches("signed", batchSize)
	n.Metrics.ObserveLatency("StoreChunks", "signed", float64(time.Since(stageTimer).Milliseconds()))
	log.Debug("Sign batch took", "duration", time.Since(stageTimer))

	log.Info("StoreChunks succeeded")

	log.Debug("Exiting process batch", "duration", time.Since(start))
	return sig, nil
}

func (n *Node) ValidateBatch(ctx context.Context, header *core.BatchHeader, blobs []*core.BlobMessage) error {
	operatorState, err := n.ChainState.GetOperatorStateByOperator(ctx, header.ReferenceBlockNumber, n.Config.ID)
	if err != nil {
		return err
	}

	pool := workerpool.New(n.Config.NumBatchValidators)
	out := make(chan error, len(blobs))
	for _, blob := range blobs {
		blob := blob
		pool.Submit(func() {
			n.validateBlob(ctx, blob, operatorState, out)
		})
	}

	for i := 0; i < len(blobs); i++ {
		err := <-out
		if err != nil {
			return err
		}
	}

	return nil

}

func (n *Node) updateSocketAddress(ctx context.Context, newSocketAddr string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if newSocketAddr == n.CurrentSocket {
		return
	}

	if err := n.Transactor.UpdateOperatorSocket(ctx, newSocketAddr); err != nil {
		n.Logger.Error("failed to update operator's socket", err)
		return
	}

	n.Logger.Info("Socket update", "old socket", n.CurrentSocket, "new socket", newSocketAddr)
	n.Metrics.RecordSocketAddressChange()
	n.CurrentSocket = newSocketAddr
}

func (n *Node) checkRegisteredNodeIpOnChain(ctx context.Context) {
	socketChan, err := n.OperatorSocketsFilterer.WatchOperatorSocketUpdate(ctx, n.Config.ID)
	if err != nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case socket := <-socketChan:
			n.mu.Lock()
			if socket != n.CurrentSocket {
				n.Logger.Info("Detected socket registered onchain which is different than the socket kept at the DA Node", "socket kept at DA Node", n.CurrentSocket, "socket registered onchain", socket, "the action taken", "update the socket kept at DA Node")
				n.CurrentSocket = socket
			}
			n.mu.Unlock()
		}
	}
}

func (n *Node) checkCurrentNodeIp(ctx context.Context) {
	t := time.NewTimer(n.Config.PubIPCheckInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			newSocketAddr, err := n.socketAddress(ctx)
			if err != nil {
				n.Logger.Error("failed to get socket address", "err", err)
				continue
			}
			n.updateSocketAddress(ctx, newSocketAddr)
		}
	}
}

func (n *Node) socketAddress(ctx context.Context) (string, error) {
	ip, err := n.PubIPProvider.PublicIPAddress(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get public ip address from IP provider: %w", err)
	}
	socket := core.MakeOperatorSocket(ip, n.Config.DispersalPort, n.Config.RetrievalPort)
	return socket.String(), nil
}

// we only need to build the sdk clients for eigenmetrics right now,
// but we might eventually want to move as much as possible to the sdk
func buildSdkClients(config *Config, logger common.Logger) (*constructor.Clients, error) {
	// we need to make a transactor just so we can get the registryCoordinatorAddr
	// to pass to the sdk config
	client, err := geth.NewClient(config.EthClientConfig, logger)
	if err != nil {
		return nil, fmt.Errorf("cannot create chain.Client: %w", err)
	}
	tx, err := eth.NewTransactor(logger, client, config.BLSOperatorStateRetrieverAddr, config.EigenDAServiceManagerAddr)
	if err != nil {
		return nil, err
	}
	registryCoordinatorAddr, err := tx.Bindings.EigenDAServiceManager.RegistryCoordinator(&bind.CallOpts{})
	if err != nil {
		return nil, err
	}
	sdkConfig := constructor.Config{
		EcdsaPrivateKeyString: config.EthClientConfig.PrivateKeyString,
		EthHttpUrl:            config.EthClientConfig.RPCURL,
		// setting as http url for now since eigenDA doesn't have a ws endpoint in its config
		// should be fine since we won't use subscriptions, but this will cause issues if we do by mistake..
		EthWsUrl:                      config.EthClientConfig.RPCURL,
		BlsRegistryCoordinatorAddr:    registryCoordinatorAddr.Hex(),
		BlsOperatorStateRetrieverAddr: config.BLSOperatorStateRetrieverAddr,
		AvsName:                       AppName,
		PromMetricsIpPortAddress:      ":" + config.MetricsPort,
	}
	sdkClients, err := constructor.BuildClients(sdkConfig, logger)
	if err != nil {
		return nil, err
	}
	// we also register the economicMetricsCollector with the registry
	economicMetricsCollector := economic.NewCollector(sdkClients.ElChainReader, sdkClients.AvsRegistryChainReader, AppName, logger, client.AccountAddress, QuorumNames)
	sdkClients.PrometheusRegistry.MustRegister(economicMetricsCollector)
	return sdkClients, nil
}

func (n *Node) validateBlob(ctx context.Context, blob *core.BlobMessage, operatorState *core.OperatorState, out chan error) {
	err := n.Validator.ValidateBlob(blob, operatorState)
	if err != nil {
		out <- err
		return
	}

	out <- nil
}