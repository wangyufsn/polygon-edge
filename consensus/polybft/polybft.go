// Package polybft implements PBFT consensus algorithm integration and bridge feature
package polybft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/0xPolygon/pbft-consensus"
	"github.com/0xPolygon/polygon-edge/consensus"
	"github.com/0xPolygon/polygon-edge/consensus/polybft/proto"
	"github.com/0xPolygon/polygon-edge/consensus/polybft/wallet"
	"github.com/0xPolygon/polygon-edge/helper/progress"
	"github.com/0xPolygon/polygon-edge/network"
	"github.com/0xPolygon/polygon-edge/secrets"
	"github.com/0xPolygon/polygon-edge/state"
	"github.com/0xPolygon/polygon-edge/syncer"
	"github.com/0xPolygon/polygon-edge/types"
	"github.com/hashicorp/go-hclog"
	"github.com/libp2p/go-libp2p/core/peer"
	"go.opentelemetry.io/otel"
)

const (
	minSyncPeers = 2
	pbftProto    = "/pbft/0.2"
	bridgeProto  = "/bridge/0.2"

	blockTimeKey           = "blockTime"
	epochSizeKey           = "epochSize"
	sprintSizeKey          = "sprintSize"
	validatorSetSizeKey    = "validatorSetSize"
	sidechainBridgeAddrKey = "sidechainBridgeAddr"
	validatorSetAddrKey    = "validatorSetAddr"
)

// polybftBackend is an interface defining polybft methods needed by fsm and sync tracker
type polybftBackend interface {
	// CheckIfStuck checks if state machine is stuck.
	CheckIfStuck(num uint64) (uint64, bool)

	// GetValidators retrieves validator set for the given block
	GetValidators(blockNumber uint64, parents []*types.Header) (AccountSet, error)
}

// Factory is the factory function to create a discovery consensus
func Factory(params *consensus.Params) (consensus.Consensus, error) {
	logger := params.Logger.Named("polybft")
	logger.Info("polybft factory", "params", params.Config.Params, "specific consensus params", params.Config)

	polybft := &Polybft{
		config:  params,
		closeCh: make(chan struct{}),
		logger:  logger,
	}
	polybft.initializeConsensusConfig()
	return polybft, nil
}

type Polybft struct {
	// close closes all the pbft consensus
	closeCh chan struct{}

	// pbft is the pbft engine
	pbft *pbft.Pbft

	// state is reference to the struct which encapsulates consensus data persistence logic
	state *State

	// consensus parametres
	config *consensus.Params

	// consensusConfig is genesis configuration for polybft consensus protocol
	consensusConfig *PolyBFTConfig

	// blockchain is a reference to the blockchain object
	blockchain blockchainBackend

	// runtime handles consensus runtime features like epoch, state and event management
	runtime *consensusRuntime

	// block time duration
	blockTime time.Duration

	// dataDir is the data directory to store the info
	dataDir string

	// reference to the syncer
	syncer syncer.Syncer

	// topic for pbft consensus
	pbftTopic *network.Topic

	// topic for pbft consensus
	bridgeTopic *network.Topic

	// key encapsulates ECDSA address and BLS signing logic
	key *wallet.Key

	// validatorsCache represents cache of validators snapshots
	validatorsCache *validatorsSnapshotCache

	// logger
	logger hclog.Logger
}

// Initialize initializes the consensus (e.g. setup data)
func (p *Polybft) Initialize() error {
	p.logger.Info("initializing polybft...")

	// read account
	account, err := wallet.GenerateNewAccountFromSecret(
		p.config.SecretsManager, secrets.ValidatorBLSKey)
	if err != nil {
		return fmt.Errorf("failed to read account data. Error: %v", err)
	}

	// set key
	p.key = wallet.NewKey(account)

	// create and set syncer
	p.syncer = syncer.NewSyncer(
		p.config.Logger.Named("syncer"),
		p.config.Network,
		p.config.Blockchain,
		time.Duration(p.config.BlockTime)*3*time.Second,
	)

	// set blockchain backend
	p.blockchain = &blockchainWrapper{
		blockchain: p.config.Blockchain,
		executor:   p.config.Executor,
	}

	// initialize pbft engine
	opts := []pbft.ConfigOption{
		pbft.WithLogger(p.logger.Named("Pbft").
			StandardLogger(&hclog.StandardLoggerOptions{}),
		),
		pbft.WithTracer(otel.Tracer("Pbft")),
	}
	p.pbft = pbft.New(p.key, &pbftTransportWrapper{topic: p.pbftTopic}, opts...)

	// create pbft topic
	pbftTopic, err := p.config.Network.NewTopic(pbftProto, &proto.GossipMessage{})
	if err != nil {
		return fmt.Errorf("failed to create pbft topic. Error: %w", err)
	}

	// check pbft topic - listen for transport messages and relay them to pbft
	err = pbftTopic.Subscribe(func(obj interface{}, from peer.ID) {
		gossipMsg, _ := obj.(*proto.GossipMessage)

		var msg *pbft.MessageReq
		if err := json.Unmarshal(gossipMsg.Data, &msg); err != nil {
			panic(err)
		}

		p.pbft.PushMessage(msg)
	})

	if err != nil {
		return fmt.Errorf("Topic subscription failed: %v", err)
	}

	// set pbft topic
	p.pbftTopic = pbftTopic

	// create bridge topic
	bridgeTopic, err := p.config.Network.NewTopic(bridgeProto, &proto.TransportMessage{})
	if err != nil {
		return fmt.Errorf("failed to create bridge topic. Error: %v", err)
	}
	// set pbft topic, it will be check if/when the bridge is enabled
	p.bridgeTopic = bridgeTopic

	// set block time  Nemanja - not sure if I am going to need it
	p.blockTime = time.Duration(p.config.BlockTime)

	// initialize polybft consensus data directory
	p.dataDir = filepath.Join(p.config.Config.Path, "polybft")
	// create the data dir if not exists
	if err := os.MkdirAll(p.dataDir, 0755); err != nil {
		return fmt.Errorf("failed to create data directory. Error: %v", err)
	}

	stt, err := newState(filepath.Join(p.dataDir, stateFileName))
	if err != nil {
		return fmt.Errorf("failed to create state instance. Error: %v", err)
	}

	p.state = stt
	p.validatorsCache = newValidatorsSnapshotCache(p.config.Logger, stt, p.consensusConfig.EpochSize, p.blockchain)

	return nil
}

// Start starts the consensus and servers
func (p *Polybft) Start() error {
	p.logger.Info("starting polybft consensus")

	// start syncer
	if err := p.startSyncing(); err != nil {
		return err
	}

	// start consensus
	return p.startSealing()
}

// startSyncing starts the synchroniser
func (p *Polybft) startSyncing() error {
	if err := p.syncer.Start(); err != nil {
		return fmt.Errorf("failed to start syncer. Error: %v", err)
	}

	go func() {
		nullHandler := func(b *types.Block) bool {
			return false
		}

		if err := p.syncer.Sync(nullHandler); err != nil {
			panic(fmt.Errorf("failed to sync blocks. Error: %v", err))
			// TO DO Nemanja - should we only log here as ibft, it seems to me that we should panic
			// p.logger.Error("watch sync failed", "err", err)
		}
	}()

	return nil
}

// startSealing is executed if the PolyBFT protocol is running in sealing mode.
func (p *Polybft) startSealing() error {
	p.logger.Info("Using signer", "address", p.key.String())

	if err := p.startRuntime(); err != nil {
		return fmt.Errorf("Runtime startup failed: %v", err)
	}

	go func() {
		// start the pbft process
		p.startPbftProcess()
	}()

	return nil
}

// initializeConsensusConfig populates consensus configuration
func (p *Polybft) initializeConsensusConfig() {
	customConfigGeneric := p.config.Config.Config
	blockTime, _ := customConfigGeneric[blockTimeKey].(time.Duration)
	epochSize, _ := customConfigGeneric[epochSizeKey].(uint64)
	sprintSize, _ := customConfigGeneric[sprintSizeKey].(uint64)
	sidechainBridgeAddr, _ := customConfigGeneric[sidechainBridgeAddrKey].(types.Address)
	validatorSetAddr, _ := customConfigGeneric[validatorSetAddrKey].(types.Address)
	activeValidatorsSize, _ := customConfigGeneric[validatorSetSizeKey].(int)

	// TODO: Bridge, validators configuration
	p.consensusConfig = &PolyBFTConfig{
		BlockTime:           blockTime,
		EpochSize:           epochSize,
		SprintSize:          sprintSize,
		SidechainBridgeAddr: sidechainBridgeAddr,
		ValidatorSetAddr:    validatorSetAddr,
		ValidatorSetSize:    activeValidatorsSize,
	}
}

// startRuntime starts consensus runtime
func (p *Polybft) startRuntime() error {
	runtimeConfig := &runtimeConfig{
		PolyBFTConfig: p.consensusConfig,
		Key:           p.key,
		DataDir:       p.dataDir,
		Transport: &bridgeTransportWrapper{
			topic:  p.bridgeTopic,
			logger: p.logger.Named("bridge_transport"),
		},
		State:          p.state,
		blockchain:     p.blockchain,
		polybftBackend: p,
	}

	runtime, err := newConsensusRuntime(p.logger, runtimeConfig)
	if err != nil {
		return err
	}

	p.runtime = runtime

	if runtime.IsBridgeEnabled() {
		err := p.bridgeTopic.Subscribe(func(obj interface{}, from peer.ID) {
			msg, _ := obj.(*proto.TransportMessage)
			var transportMsg *TransportMessage
			if err := json.Unmarshal(msg.Data, &transportMsg); err != nil {
				panic(err)
			}
			if _, err := p.runtime.deliverMessage(transportMsg); err != nil {
				p.logger.Warn(fmt.Sprintf("Failed to deliver message. Error: %s", err))
			}
		})
		if err != nil {
			return fmt.Errorf("topic subscription failed:%w", err)
		}
	}

	return nil
}

func (p *Polybft) startPbftProcess() {
	// wait to have at least n peers connected. The 2 is just an initial heuristic value
	// Most likely we will parametrize this in the future.
	if !p.waitForNPeers() {
		return
	}

	// subscribe to new block events
	var (
		newBlockSub   = p.blockchain.SubscribeEvents()
		syncerBlockCh = make(chan uint64)
	)

	// Receive a notification every time syncer manages
	// to insert a valid block.
	go func() {
		eventCh := newBlockSub.GetEventCh()

		for {
			select {
			case ev := <-eventCh:
				currentBlockNum := p.blockchain.CurrentHeader().Number
				if ev.Source == "syncer" {
					if ev.NewChain[0].Number < currentBlockNum {
						continue
					}
				}
				if p.isSynced() {
					syncerBlockCh <- currentBlockNum
				}

			case <-p.closeCh:
				return
			}
		}
	}()

	defer newBlockSub.Close()

SYNC:
	if !p.isSynced() {
		<-syncerBlockCh
	}

	lastBlock := p.blockchain.CurrentHeader()

	currentValidators, err := p.GetValidators(lastBlock.Number, nil)
	if err != nil {
		p.logger.Error("failed to query current validator set", "block number", lastBlock.Number, "error", err)
	}

	p.runtime.setIsActiveValidator(currentValidators.ContainsNodeID(p.key.NodeID()))
	if !p.runtime.isActiveValidator() {
		// inactive validator is not part of the consensus protocol and it should just perform syncing
		goto SYNC
	}

	// we have to start the bridge snapshot when we have finished syncing
	if err := p.runtime.restartEpoch(lastBlock); err != nil {
		p.logger.Error("failed to restart epoch", "error", err)

		goto SYNC
	}

	for {
		if err := p.runCycle(); err != nil {
			if errors.Is(err, errNotAValidator) {
				p.logger.Info("Node is no longer in validator set")
			} else {
				p.logger.Error("an error occurred while running a state machine cycle.", "error", err)
			}

			goto SYNC
		}

		switch p.pbft.GetState() {
		case pbft.SyncState:
			// we need to go back to sync
			goto SYNC
		case pbft.DoneState:
			// everything worked, move to the next iteration
		default:
			// stopped
			return
		}
	}
}

// isSynced return true if the current header from the local storage corresponds to the highest block of syncer
func (p *Polybft) isSynced() bool {
	// TODO: Check could we change following condition to this:
	// p.syncer.GetSyncProgression().CurrentBlock >= p.syncer.GetSyncProgression().HighestBlock
	syncProgression := p.syncer.GetSyncProgression()
	return syncProgression == nil ||
		p.blockchain.CurrentHeader().Number >= syncProgression.HighestBlock
}

// runCycle runs a single cycle of the state machine and indicates if node should exit the consensus or keep on running
func (p *Polybft) runCycle() error {
	ff, err := p.runtime.FSM()
	if err != nil {
		return err
	}

	if err = p.pbft.SetBackend(ff); err != nil {
		return err
	}

	// this cancel is not sexy
	ctx, cancelFn := context.WithCancel(context.Background())

	go func() {
		<-p.closeCh
		cancelFn()
	}()

	p.pbft.Run(ctx)

	return nil
}

func (p *Polybft) waitForNPeers() bool {
	for {
		select {
		case <-p.closeCh:
			return false
		case <-time.After(2 * time.Second):
		}

		numPeers := len(p.config.Network.Peers())
		if numPeers >= minSyncPeers {
			break
		}
	}
	return true
}

// Close closes the connection
func (p *Polybft) Close() error {
	if p.syncer != nil {
		if err := p.syncer.Close(); err != nil {
			return err
		}
	}

	close(p.closeCh)
	return nil
}

// GetSyncProgression retrieves the current sync progression, if any
func (p *Polybft) GetSyncProgression() *progress.Progression {
	return p.syncer.GetSyncProgression()
}

// VerifyHeader implements consensus.Engine and checks whether a header conforms to the consensus rules
func (p *Polybft) VerifyHeader(header *types.Header) error {
	// Short circuit if the header is known
	header, ok := p.blockchain.GetHeaderByHash(header.HashF())
	if ok {
		return nil
	}

	parent, ok := p.blockchain.GetHeaderByHash(header.ParentHash)
	if !ok {
		return fmt.Errorf(
			"unable to get parent header for block number %d",
			header.Number,
		)
	}

	return p.verifyHeaderImpl(parent, header, nil)
}

func (p *Polybft) verifyHeaderImpl(parent, header *types.Header, parents []*types.Header) error {
	blockNumber := header.Number
	if blockNumber == 0 {
		// TODO: Remove, this was just for simplicity since I had started the chain already,
		//  add the mix hash into the genesis command
		return nil
	}

	// validate header fields
	if err := validateHeaderFields(parent, header); err != nil {
		return fmt.Errorf("failed to validate header for block %d. error = %v", blockNumber, err)
	}

	validators, err := p.GetValidators(blockNumber-1, parents)
	if err != nil {
		return fmt.Errorf("failed to validate header for block %d. could not retrieve block validators:%w", blockNumber, err)
	}

	// decode the extra field and validate the signatures
	extra, err := GetIbftExtra(header.ExtraData)
	if err != nil {
		return fmt.Errorf("failed to verify header for block %d. get extra error = %v", blockNumber, err)
	}
	if extra.Committed == nil {
		return fmt.Errorf(
			"failed to verify signatures for block %d because signatures are nil. Block hash: %v",
			blockNumber,
			header.HashF(),
		)
	}
	if err := extra.Committed.VerifyCommittedFields(validators, header.HashF()); err != nil {
		return fmt.Errorf("failed to verify signatures for block %d. Block hash: %v", blockNumber, header.HashF())
	}

	// validate the signatures for parent (skip block 1 because genesis does not have committed)
	if blockNumber > 1 {
		if extra.Parent == nil {
			return fmt.Errorf(
				"failed to verify signatures for parent of block %d because signatures are nil. Parent hash: %v",
				blockNumber,
				parent.HashF(),
			)
		}
		parentValidators, err := p.GetValidators(blockNumber-2, parents)
		if err != nil {
			return fmt.Errorf(
				"failed to validate header for block %d. could not retrieve parent validators:%w",
				blockNumber,
				err,
			)
		}
		if err := extra.Parent.VerifyCommittedFields(parentValidators, parent.HashF()); err != nil {
			return fmt.Errorf("failed to verify signatures for parent of block %d. Parent hash: %v", blockNumber, parent.HashF())
		}
	}

	return nil
}

func (p *Polybft) CheckIfStuck(num uint64) (uint64, bool) {
	if !p.isSynced() {
		// we are currently syncing new data, for sure we are stuck.
		// We can return 0 here at least for now since that value is only used
		// for the open telemetry tracing.
		return 0, true
	}

	// Now, we have to check if the current value of the round 'num' is lower
	// than our currently synced block.
	currentHeader := p.blockchain.CurrentHeader().Number
	if currentHeader > num {
		// at this point, it will exit the sync process and start the fsm round again
		// (or sync a small number of blocks) to start from the correct position.
		return currentHeader, true
	}
	return 0, false
}

func (p *Polybft) GetValidators(blockNumber uint64, parents []*types.Header) (AccountSet, error) {
	return p.validatorsCache.GetSnapshot(blockNumber, parents)
}

// ProcessHeaders updates the snapshot based on the verified headers
func (p *Polybft) ProcessHeaders(_ []*types.Header) error {
	// Not required
	return nil
}

// GetBlockCreator retrieves the block creator (or signer) given the block header
func (p *Polybft) GetBlockCreator(_ *types.Header) (types.Address, error) {
	panic("GetBlockCreator not implemented")
}

// PreCommitState a hook to be called before finalizing state transition on inserting block
func (p *Polybft) PreCommitState(_ *types.Header, _ *state.Transition) error {
	// Not required
	return nil
}

type pbftTransportWrapper struct {
	topic *network.Topic
}

func (p *pbftTransportWrapper) Gossip(msg *pbft.MessageReq) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	protoMsg := &proto.GossipMessage{
		Data: data,
	}
	return p.topic.Publish(protoMsg)
}

type bridgeTransportWrapper struct {
	topic  *network.Topic
	logger hclog.Logger
}

func (b *bridgeTransportWrapper) Gossip(msg interface{}) {
	data, err := json.Marshal(msg)
	if err != nil {
		b.logger.Warn(fmt.Sprintf("Failed to marshal bridge message:%s", err))
		return
	}
	protoMsg := &proto.GossipMessage{
		Data: data,
	}

	err = b.topic.Publish(protoMsg)
	if err != nil {
		b.logger.Warn(fmt.Sprintf("Failed to gossip bridge message:%s", err))
	}
}

var _ polybftBackend = &Polybft{}
