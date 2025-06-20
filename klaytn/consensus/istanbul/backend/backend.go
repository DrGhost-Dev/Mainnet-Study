// Modifications Copyright 2024 The Kaia Authors
// Modifications Copyright 2018 The klaytn Authors
// Copyright 2017 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.
//
// This file is derived from quorum/consensus/istanbul/backend/backend.go (2018/06/04).
// Modified and improved for the klaytn development.
// Modified and improved for the Kaia development.

package backend

import (
	"crypto/ecdsa"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru"
	"github.com/klaytn/klaytn/blockchain"
	"github.com/klaytn/klaytn/blockchain/types"
	"github.com/klaytn/klaytn/common"
	"github.com/klaytn/klaytn/consensus"
	"github.com/klaytn/klaytn/consensus/istanbul"
	istanbulCore "github.com/klaytn/klaytn/consensus/istanbul/core"
	"github.com/klaytn/klaytn/consensus/istanbul/validator"
	"github.com/klaytn/klaytn/crypto"
	"github.com/klaytn/klaytn/crypto/bls"
	"github.com/klaytn/klaytn/event"
	"github.com/klaytn/klaytn/governance"
	"github.com/klaytn/klaytn/log"
	"github.com/klaytn/klaytn/reward"
	"github.com/klaytn/klaytn/storage/database"
)

const (
	// fetcherID is the ID indicates the block is from Istanbul engine
	fetcherID = "istanbul"
)

var logger = log.NewModuleLogger(log.ConsensusIstanbulBackend)

type BackendOpts struct {
	IstanbulConfig    *istanbul.Config // Istanbul consensus core config
	Rewardbase        common.Address
	PrivateKey        *ecdsa.PrivateKey // Consensus message signing key
	BlsSecretKey      bls.SecretKey     // Randao signing key. Required since Randao fork
	DB                database.DBManager
	Governance        governance.Engine // Governance parameter provider
	BlsPubkeyProvider BlsPubkeyProvider // If not nil, override the default BLS public key provider
	NodeType          common.ConnType
}

func New(opts *BackendOpts) consensus.Istanbul {
	recents, _ := lru.NewARC(inmemorySnapshots)
	recentMessages, _ := lru.NewARC(inmemoryPeers)
	knownMessages, _ := lru.NewARC(inmemoryMessages)
	backend := &backend{
		config:            opts.IstanbulConfig,
		istanbulEventMux:  new(event.TypeMux),
		privateKey:        opts.PrivateKey,
		address:           crypto.PubkeyToAddress(opts.PrivateKey.PublicKey),
		blsSecretKey:      opts.BlsSecretKey,
		logger:            logger.NewWith(),
		db:                opts.DB,
		commitCh:          make(chan *types.Result, 1),
		recents:           recents,
		candidates:        make(map[common.Address]bool),
		coreStarted:       false,
		recentMessages:    recentMessages,
		knownMessages:     knownMessages,
		rewardbase:        opts.Rewardbase,
		governance:        opts.Governance,
		blsPubkeyProvider: opts.BlsPubkeyProvider,
		nodetype:          opts.NodeType,
		rewardDistributor: reward.NewRewardDistributor(opts.Governance),
	}
	if backend.blsPubkeyProvider == nil {
		backend.blsPubkeyProvider = newChainBlsPubkeyProvider()
	}

	backend.currentView.Store(&istanbul.View{Sequence: big.NewInt(0), Round: big.NewInt(0)})
	backend.core = istanbulCore.New(backend, backend.config)
	return backend
}

// ----------------------------------------------------------------------------

type backend struct {
	// consensus 설정 값
	config           *istanbul.Config
	istanbulEventMux *event.TypeMux
	privateKey       *ecdsa.PrivateKey
	address          common.Address
	// BLS 서명 알고리즘을 위한 개인키
	blsSecretKey bls.SecretKey
	// 합의 과정에서 발생하는 다양한 이벤트(예: 새로운 블록 제안, 합의 도달 등)를 구독하고 처리하기 위한 이벤트 중개자
	core istanbulCore.Engine
	// 노드의 현재 상태나 에러 등을 기록하기 위한 로거(Logger) 객체
	logger       log.Logger
	db           database.DBManager
	chain        consensus.ChainReader
	currentBlock func() *types.Block
	hasBadBlock  func(hash common.Hash) bool

	// the channels for istanbul engine notifications
	commitCh chan *types.Result
	// 현재 라운드에서 내가 제안했거나 다른 노드로부터 제안받은 블록의 해시
	proposedBlockHash common.Hash
	sealMu            sync.Mutex
	// 엔진이 시작되었는지 여부를 나타내는 플래그
	coreStarted bool
	coreMu      sync.RWMutex

	// Current list of candidates we are pushing
	// 현재 내가 다음 블록에 포함시키려고 하는 트랜잭션들의 후보 목록
	candidates map[common.Address]bool
	// Protects the signer fields
	candidatesLock sync.RWMutex
	// Snapshots for recent block to speed up reorgs
	recents *lru.ARCCache

	// event subscription for ChainHeadEvent event
	// 생성된 블록이나 합의 메시지를 다른 노드들에게 전파(broadcast)하는 역할을 담당
	broadcaster consensus.Broadcaster

	// 다른 노드들로부터 최근에 받은 합의 메시지를 저장하는 캐시
	recentMessages *lru.ARCCache // the cache of peer's messages
	// 내가 생성해서 다른 노드들에게 보낸 합의 메시지를 저장하는 캐시
	knownMessages *lru.ARCCache // the cache of self messages

	// 블록 생성에 대한 보상을 받을 주소
	rewardbase common.Address
	// 현재 합의 라운드의 뷰(View) 정보를 담고 있습니다. 뷰는 (라운드 번호, 시퀀스 번호)의 조합으로, 어떤 블록을 누가 제안할 차례인지를 나타냄
	currentView atomic.Value //*istanbul.View

	// Reference to the governance.Engine
	// Klaytn 거버넌스와 관련된 로직을 처리하는 엔진입니다. 검증인(validator) 추가/제거, 시스템 설정 변경 등 온체인 거버넌스 제안이 발생했을 때,
	// 합의 엔진이 이를 인지하고 처리하도록 연동하는 역할
	governance governance.Engine

	// Reference to BlsPubkeyProvider
	// 다른 검증인들의 BLS 공개키를 가져오는 역할을 하는 인터페이스입니다. 다른 노드가 보낸 BLS 서명을 검증하려면 해당 노드의 공개키를 알아야 하기 때문에 필요함
	blsPubkeyProvider BlsPubkeyProvider

	// 블록 생성 보상을 어떻게 분배할지 결정하는 로직
	rewardDistributor *reward.RewardDistributor

	// Node type
	// 현재 노드의 타입. 합의 노드(CN), 프록시 노드(PN), 엔드포인트 노드(EN)
	nodetype common.ConnType

	isRestoringSnapshots atomic.Bool
}

func (sb *backend) NodeType() common.ConnType {
	return sb.nodetype
}

func (sb *backend) GetRewardBase() common.Address {
	return sb.rewardbase
}

func (sb *backend) SetCurrentView(view *istanbul.View) {
	sb.currentView.Store(view)
}

// Address implements istanbul.Backend.Address
func (sb *backend) Address() common.Address {
	return sb.address
}

// Validators implements istanbul.Backend.Validators
func (sb *backend) Validators(proposal istanbul.Proposal) istanbul.ValidatorSet {
	return sb.getValidators(proposal.Number().Uint64(), proposal.Hash())
}

// Broadcast implements istanbul.Backend.Broadcast
func (sb *backend) Broadcast(prevHash common.Hash, valSet istanbul.ValidatorSet, payload []byte) error {
	// send to others
	// TODO Check gossip again in event handle
	// sb.Gossip(valSet, payload)
	// send to self
	msg := istanbul.MessageEvent{
		Hash:    prevHash,
		Payload: payload,
	}
	go sb.istanbulEventMux.Post(msg)
	return nil
}

// Broadcast implements istanbul.Backend.Gossip
func (sb *backend) Gossip(valSet istanbul.ValidatorSet, payload []byte) error {
	hash := istanbul.RLPHash(payload)
	sb.knownMessages.Add(hash, true)

	if sb.broadcaster != nil {
		ps := sb.broadcaster.GetCNPeers()
		for addr, p := range ps {
			ms, ok := sb.recentMessages.Get(addr)
			var m *lru.ARCCache
			if ok {
				m, _ = ms.(*lru.ARCCache)
				if _, k := m.Get(hash); k {
					// This peer had this event, skip it
					continue
				}
			} else {
				m, _ = lru.NewARC(inmemoryMessages)
			}

			m.Add(hash, true)
			sb.recentMessages.Add(addr, m)

			cmsg := &istanbul.ConsensusMsg{
				PrevHash: common.Hash{},
				Payload:  payload,
			}

			// go p.Send(IstanbulMsg, payload)
			go p.Send(IstanbulMsg, cmsg)
		}
	}
	return nil
}

// checkInSubList checks if the node is in a sublist
func (sb *backend) checkInSubList(prevHash common.Hash, valSet istanbul.ValidatorSet) bool {
	return valSet.CheckInSubList(prevHash, sb.currentView.Load().(*istanbul.View), sb.Address())
}

// getTargetReceivers returns a map of nodes which need to receive a message
func (sb *backend) getTargetReceivers(prevHash common.Hash, valSet istanbul.ValidatorSet) map[common.Address]bool {
	targets := make(map[common.Address]bool)

	cv, ok := sb.currentView.Load().(*istanbul.View)
	if !ok {
		logger.Error("Failed to assert type from sb.currentView!!", "cv", cv)
		return nil
	}
	view := &istanbul.View{
		Round:    big.NewInt(cv.Round.Int64()),
		Sequence: big.NewInt(cv.Sequence.Int64()),
	}

	proposer := valSet.GetProposer()
	for i := 0; i < 2; i++ {
		committee := valSet.SubListWithProposer(prevHash, proposer.Address(), view)
		for _, val := range committee {
			if val.Address() != sb.Address() {
				targets[val.Address()] = true
			}
		}
		view.Round = view.Round.Add(view.Round, common.Big1)
		proposer = valSet.Selector(valSet, common.Address{}, view.Round.Uint64())
	}
	return targets
}

// GossipSubPeer implements istanbul.Backend.Gossip
func (sb *backend) GossipSubPeer(prevHash common.Hash, valSet istanbul.ValidatorSet, payload []byte) map[common.Address]bool {
	if !sb.checkInSubList(prevHash, valSet) {
		return nil
	}

	hash := istanbul.RLPHash(payload)
	sb.knownMessages.Add(hash, true)

	targets := sb.getTargetReceivers(prevHash, valSet)

	if sb.broadcaster != nil && len(targets) > 0 {
		ps := sb.broadcaster.FindCNPeers(targets)
		for addr, p := range ps {
			ms, ok := sb.recentMessages.Get(addr)
			var m *lru.ARCCache
			if ok {
				m, _ = ms.(*lru.ARCCache)
				if _, k := m.Get(hash); k {
					// This peer had this event, skip it
					continue
				}
			} else {
				m, _ = lru.NewARC(inmemoryMessages)
			}

			m.Add(hash, true)
			sb.recentMessages.Add(addr, m)

			cmsg := &istanbul.ConsensusMsg{
				PrevHash: prevHash,
				Payload:  payload,
			}

			go p.Send(IstanbulMsg, cmsg)
		}
	}
	return targets
}

// Commit implements istanbul.Backend.Commit
func (sb *backend) Commit(proposal istanbul.Proposal, seals [][]byte) error {
	// Check if the proposal is a valid block
	block, ok := proposal.(*types.Block)
	if !ok {
		sb.logger.Error("Invalid proposal, %v", proposal)
		return errInvalidProposal
	}
	h := block.Header()
	round := sb.currentView.Load().(*istanbul.View).Round.Int64()
	h = types.SetRoundToHeader(h, round)
	// Append seals into extra-data
	err := writeCommittedSeals(h, seals)
	if err != nil {
		return err
	}
	// update block's header
	block = block.WithSeal(h)

	sb.logger.Info("Committed", "number", proposal.Number().Uint64(), "hash", proposal.Hash(), "address", sb.Address())
	// - if the proposed and committed blocks are the same, send the proposed hash
	//   to commit channel, which is being watched inside the engine.Seal() function.
	// - otherwise, we try to insert the block.
	// -- if success, the ChainHeadEvent event will be broadcasted, try to build
	//    the next block and the previous Seal() will be stopped.
	// -- otherwise, a error will be returned and a round change event will be fired.
	if sb.proposedBlockHash == block.Hash() {
		// feed block hash to Seal() and wait the Seal() result
		sb.commitCh <- &types.Result{Block: block, Round: round}
		return nil
	}

	if sb.broadcaster != nil {
		sb.broadcaster.Enqueue(fetcherID, block)
	}
	return nil
}

// EventMux implements istanbul.Backend.EventMux
func (sb *backend) EventMux() *event.TypeMux {
	return sb.istanbulEventMux
}

// Verify implements istanbul.Backend.Verify
func (sb *backend) Verify(proposal istanbul.Proposal) (time.Duration, error) {
	// Check if the proposal is a valid block
	block, ok := proposal.(*types.Block)
	if !ok {
		sb.logger.Error("Invalid proposal, %v", proposal)
		return 0, errInvalidProposal
	}

	// check bad block
	if sb.HasBadProposal(block.Hash()) {
		return 0, blockchain.ErrBlacklistedHash
	}

	// check block body
	txnHash := types.DeriveSha(block.Transactions(), block.Number())
	if txnHash != block.Header().TxHash {
		return 0, errMismatchTxhashes
	}

	// verify the header of proposed block
	err := sb.VerifyHeader(sb.chain, block.Header(), false)
	// ignore errEmptyCommittedSeals error because we don't have the committed seals yet
	if err == nil || err == errEmptyCommittedSeals {
		return 0, nil
	} else if err == consensus.ErrFutureBlock {
		return time.Unix(block.Header().Time.Int64(), 0).Sub(now()), consensus.ErrFutureBlock
	}
	return 0, err
}

// Sign implements istanbul.Backend.Sign
func (sb *backend) Sign(data []byte) ([]byte, error) {
	hashData := crypto.Keccak256([]byte(data))
	return crypto.Sign(hashData, sb.privateKey)
}

// CheckSignature implements istanbul.Backend.CheckSignature
func (sb *backend) CheckSignature(data []byte, address common.Address, sig []byte) error {
	signer, err := cacheSignatureAddresses(data, sig)
	if err != nil {
		logger.Error("Failed to get signer address", "err", err)
		return err
	}
	// Compare derived addresses
	if signer != address {
		return errInvalidSignature
	}
	return nil
}

// HasPropsal implements istanbul.Backend.HashBlock
func (sb *backend) HasPropsal(hash common.Hash, number *big.Int) bool {
	return sb.chain.GetHeader(hash, number.Uint64()) != nil
}

// GetProposer implements istanbul.Backend.GetProposer
func (sb *backend) GetProposer(number uint64) common.Address {
	if h := sb.chain.GetHeaderByNumber(number); h != nil {
		a, _ := sb.Author(h)
		return a
	}
	return common.Address{}
}

// ParentValidators implements istanbul.Backend.GetParentValidators
func (sb *backend) ParentValidators(proposal istanbul.Proposal) istanbul.ValidatorSet {
	if block, ok := proposal.(*types.Block); ok {
		return sb.getValidators(block.Number().Uint64()-1, block.ParentHash())
	}

	// TODO-Kaia-Governance The following return case should not be called. Refactor it to error handling.
	return validator.NewValidatorSet(nil, nil,
		istanbul.ProposerPolicy(sb.chain.Config().Istanbul.ProposerPolicy),
		sb.chain.Config().Istanbul.SubGroupSize,
		sb.chain)
}

func (sb *backend) getValidators(number uint64, hash common.Hash) istanbul.ValidatorSet {
	snap, err := sb.snapshot(sb.chain, number, hash, nil, false)
	if err != nil {
		logger.Error("Snapshot not found.", "err", err)
		// TODO-Kaia-Governance The following return case should not be called. Refactor it to error handling.
		return validator.NewValidatorSet(nil, nil,
			istanbul.ProposerPolicy(sb.chain.Config().Istanbul.ProposerPolicy),
			sb.chain.Config().Istanbul.SubGroupSize,
			sb.chain)
	}
	return snap.ValSet
}

func (sb *backend) LastProposal() (istanbul.Proposal, common.Address) {
	block := sb.currentBlock()

	var proposer common.Address
	if block.Number().Cmp(common.Big0) > 0 {
		var err error
		// 민팅된 블록에서 klaytn 주소(signer)를 불러옴
		proposer, err = sb.Author(block.Header())
		if err != nil {
			sb.logger.Error("Failed to get block proposer", "err", err)
			return nil, common.Address{}
		}
	}

	// Return header only block here since we don't need block body
	return block, proposer
}

func (sb *backend) HasBadProposal(hash common.Hash) bool {
	if sb.hasBadBlock == nil {
		return false
	}
	return sb.hasBadBlock(hash)
}
