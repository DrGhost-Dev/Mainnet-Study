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
// This file is derived from quorum/consensus/istanbul/core/core.go (2018/06/04).
// Modified and improved for the klaytn development.
// Modified and improved for the Kaia development.

package core

import (
	"bytes"
	"math"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klaytn/klaytn/blockchain/types"
	"github.com/klaytn/klaytn/common"
	"github.com/klaytn/klaytn/common/prque"
	"github.com/klaytn/klaytn/consensus/istanbul"
	"github.com/klaytn/klaytn/event"
	"github.com/klaytn/klaytn/log"
	"github.com/rcrowley/go-metrics"
)

var logger = log.NewModuleLogger(log.ConsensusIstanbulCore)

// New creates an Istanbul consensus core
func New(backend istanbul.Backend, config *istanbul.Config) Engine {
	c := &core{
		config:             config,
		address:            backend.Address(),
		state:              StateAcceptRequest,
		handlerWg:          new(sync.WaitGroup),
		logger:             logger.NewWith("address", backend.Address()),
		backend:            backend,
		backlogs:           make(map[common.Address]*prque.Prque),
		backlogsMu:         new(sync.Mutex),
		pendingRequests:    prque.New(),
		pendingRequestsMu:  new(sync.Mutex),
		consensusTimestamp: time.Time{},

		roundMeter:         metrics.NewRegisteredMeter("consensus/istanbul/core/round", nil),
		currentRoundGauge:  metrics.NewRegisteredGauge("consensus/istanbul/core/currentRound", nil),
		sequenceMeter:      metrics.NewRegisteredMeter("consensus/istanbul/core/sequence", nil),
		consensusTimeGauge: metrics.NewRegisteredGauge("consensus/istanbul/core/timer", nil),
		councilSizeGauge:   metrics.NewRegisteredGauge("consensus/istanbul/core/councilSize", nil),
		committeeSizeGauge: metrics.NewRegisteredGauge("consensus/istanbul/core/committeeSize", nil),
		hashLockGauge:      metrics.NewRegisteredGauge("consensus/istanbul/core/hashLock", nil),
	}
	c.validateFn = c.checkValidatorSignature
	return c
}

// ----------------------------------------------------------------------------

type core struct {
	config  *istanbul.Config
	address common.Address
	state   State
	logger  log.Logger

	backend               istanbul.Backend
	events                *event.TypeMuxSubscription
	finalCommittedSub     *event.TypeMuxSubscription
	timeoutSub            *event.TypeMuxSubscription
	futurePreprepareTimer *time.Timer

	valSet                istanbul.ValidatorSet
	waitingForRoundChange bool
	validateFn            func([]byte, []byte) (common.Address, error)

	backlogs   map[common.Address]*prque.Prque
	backlogsMu *sync.Mutex

	current   *roundState
	handlerWg *sync.WaitGroup

	roundChangeSet    *roundChangeSet
	roundChangeTimer  atomic.Value //*time.Timer
	pendingRequests   *prque.Prque
	pendingRequestsMu *sync.Mutex

	consensusTimestamp time.Time
	// the meter to record the round change rate
	roundMeter metrics.Meter
	// the gauge to record the current round
	currentRoundGauge metrics.Gauge
	// the meter to record the sequence update rate
	sequenceMeter metrics.Meter
	// the gauge to record consensus duration (from accepting a preprepare to final committed stage)
	consensusTimeGauge metrics.Gauge
	// the gauge to record hashLock status (1 if hash-locked. 0 otherwise)
	hashLockGauge metrics.Gauge

	councilSizeGauge   metrics.Gauge
	committeeSizeGauge metrics.Gauge
}

func (c *core) finalizeMessage(msg *message) ([]byte, error) {
	var err error
	// Add sender address
	msg.Address = c.Address()

	// Add proof of consensus
	msg.CommittedSeal = []byte{}
	// Assign the CommittedSeal if it's a COMMIT message and proposal is not nil
	if msg.Code == msgCommit && c.current.Proposal() != nil {
		seal := PrepareCommittedSeal(c.current.Proposal().Hash())
		msg.CommittedSeal, err = c.backend.Sign(seal)
		if err != nil {
			return nil, err
		}
	}

	// Sign message
	data, err := msg.PayloadNoSig()
	if err != nil {
		return nil, err
	}
	msg.Signature, err = c.backend.Sign(data)
	if err != nil {
		return nil, err
	}

	// Convert to payload
	payload, err := msg.Payload()
	if err != nil {
		return nil, err
	}

	return payload, nil
}

func (c *core) broadcast(msg *message) {
	logger := c.logger.NewWith("state", c.state)

	payload, err := c.finalizeMessage(msg)
	if err != nil {
		logger.Error("Failed to finalize message", "msg", msg, "err", err)
		return
	}

	// Broadcast payload
	if err = c.backend.Broadcast(msg.Hash, c.valSet, payload); err != nil {
		logger.Error("Failed to broadcast message", "msg", msg, "err", err)
		return
	}
}

func (c *core) currentView() *istanbul.View {
	return &istanbul.View{
		Sequence: new(big.Int).Set(c.current.Sequence()),
		Round:    new(big.Int).Set(c.current.Round()),
	}
}

func (c *core) isProposer() bool {
	v := c.valSet
	if v == nil {
		return false
	}
	return v.IsProposer(c.backend.Address())
}

func (c *core) commit() {
	c.setState(StateCommitted)

	proposal := c.current.Proposal()
	if proposal != nil {
		committedSeals := make([][]byte, c.current.Commits.Size())
		for i, v := range c.current.Commits.Values() {
			committedSeals[i] = make([]byte, types.IstanbulExtraSeal)
			copy(committedSeals[i][:], v.CommittedSeal[:])
		}

		if err := c.backend.Commit(proposal, committedSeals); err != nil {
			c.current.UnlockHash() // Unlock block when insertion fails
			c.sendNextRoundChange("commit failure")
			return
		}

		if vrank != nil {
			vrank.HandleCommitted(proposal.Number())
		}
	} else {
		// TODO-Kaia never happen, but if proposal is nil, mining is not working.
		logger.Error("istanbul.core current.Proposal is NULL")
		c.current.UnlockHash() // Unlock block when insertion fails
		c.sendNextRoundChange("commit failure. proposal is nil")
		return
	}
}

// startNewRound starts a new round. if round equals to 0, it means to starts a new sequence
func (c *core) startNewRound(round *big.Int) {
	var logger log.Logger
	if c.current == nil {
		logger = c.logger.NewWith("old_round", -1, "old_seq", 0)
	} else {
		logger = c.logger.NewWith("old_round", c.current.Round(), "old_seq", c.current.Sequence())
	}
	roundChange := false
	// Try to get last proposal
	// blockHeader, signer를 가지고 옴
	lastProposal, lastProposer := c.backend.LastProposal()
	//if c.valSet != nil && c.valSet.IsSubSet() {
	//	c.current = nil
	//} else {
	if c.current == nil {
		logger.Trace("Start to the initial round")
	} else if lastProposal.Number().Cmp(c.current.Sequence()) >= 0 {
		// --- 뒤처진(Lagging) 상태인가? (따라잡기, Catch-up) ---
		// lastProposal: 내 노드가 알고 있는 가장 최신 블록
		// c.current.Sequence(): 내 합의 엔진이 지금 만들려고 하는 블록의 높이
		// 최신 블록 높이가 내가 만들려는 블록 높이보다 크거나 같다면,
		// 다른 노드들이 이미 블록을 만들고 앞서나갔다는 의미입니다.
		diff := new(big.Int).Sub(lastProposal.Number(), c.current.Sequence())
		// sequenceMeter: 블록 높이가 얼마나 점프했는지 성능 지표(metric)를 기록합니다.
		c.sequenceMeter.Mark(new(big.Int).Add(diff, common.Big1).Int64())

		// 만약 이전 합의가 진행 중이었다면, 얼마나 시간이 걸렸는지 기록하고 타이머를 초기화합니다.
		if !c.consensusTimestamp.IsZero() {
			c.consensusTimeGauge.Update(int64(time.Since(c.consensusTimestamp)))
			c.consensusTimestamp = time.Time{}
		}
		logger.Trace("Catch up latest proposal", "number", lastProposal.Number().Uint64(), "hash", lastProposal.Hash())
	} else if lastProposal.Number().Cmp(big.NewInt(c.current.Sequence().Int64()-1)) == 0 {
		// --- 정상(Normal) 또는 재시도(Round Change) 상태인가? ---
		// 내가 만들려는 블록 높이(c.current.Sequence())가 최신 블록 높이보다 정확히 1만큼 큰 경우.
		// 즉, 네트워크와 동기화가 잘 되어 있고, 다음 블록을 만들 준비가 된 정상적인 상태입니다.
		if round.Cmp(common.Big0) == 0 {
			// same seq and round, don't need to start new round
			return
		} else if round.Cmp(c.current.Round()) < 0 {
			logger.Warn("New round should not be smaller than current round", "seq", lastProposal.Number().Int64(), "new_round", round, "old_round", c.current.Round())
			return
		}
		// 위의 두 조건에 해당하지 않는다면, 이는 합의 실패로 인해
		// 같은 블록 높이에서 다음 라운드로 넘어가는 '재시도' 상황임을 의미합니다.
		// roundChange 플래그를 true로 설정하여 뒷부분 로직이 이를 인지하도록 합니다.
		roundChange = true
	} else {
		logger.Warn("New sequence should be larger than current sequence", "new_seq", lastProposal.Number().Int64())
		return
	}

	var newView *istanbul.View
	if roundChange {
		// 라운드만 증가
		newView = &istanbul.View{
			Sequence: new(big.Int).Set(c.current.Sequence()),
			Round:    new(big.Int).Set(round),
		}
	} else {
		newView = &istanbul.View{
			Sequence: new(big.Int).Add(lastProposal.Number(), common.Big1),
			Round:    new(big.Int), // 라운드는 0으로 초기화
		}
		// 해당 블록 높이에 맞는 검증자 집합(CN 그룹)을 가져옴
		c.valSet = c.backend.Validators(lastProposal)
		// 전체 합의 노드(CN)의 총 수를 계산
		councilSize := int64(c.valSet.Size())
		// 이번 라운드에 실제 블록 검증에 참여할 '위원회'의 크기를 계산
		committeeSize := int64(c.valSet.SubGroupSize())
		if committeeSize > councilSize {
			committeeSize = councilSize
		}
		// 계산된 전체 CN 수와 위원회 수를 모니터링 시스템의 게이지에 업데이트
		c.councilSizeGauge.Update(councilSize)
		c.committeeSizeGauge.Update(committeeSize)
	}
	c.backend.SetCurrentView(newView)

	// Update logger
	logger = logger.NewWith("old_proposer", c.valSet.GetProposer())
	// Clear invalid ROUND CHANGE messages
	// 라운드 변경 메시지 수집함 초기화
	// 합의 실패 시 다른 노드로부터 받는 '라운드 변경(ROUND CHANGE)' 메시지를 수집하는 공간을 새로 만듭니다.
	// 이전 라운드의 낡은 메시지들은 모두 비워집니다.
	c.roundChangeSet = newRoundChangeSet(c.valSet)
	// New snapshot for new round
	// 현재 라운드 상태 객체 생성
	// 이번 라운드(newView)의 모든 상태(검증자 목록, 잠긴 블록 등)를 관리할 새로운 roundState 객체를 생성합니다.
	c.updateRoundState(newView, c.valSet, roundChange)
	// Calculate new proposer
	// 새로운 제안자 계산 (가장 핵심적인 부분)
	// 이전 제안자(lastProposer)와 새로운 라운드 번호(newView.Round)를 기반으로,
	// 이번 라운드를 이끌어갈 새로운 제안자를 수학적 알고리즘에 따라 계산하고 결정합니다.
	c.valSet.CalcProposer(lastProposer, newView.Round.Uint64())
	c.waitingForRoundChange = false
	c.setState(StateAcceptRequest)
	// 이 조건문은 '합의 실패로 인한 재시도 라운드(roundChange=true)'이고, '내가 바로 그 새로운 제안자(c.isProposer())'일 때만 실행됩니다.
	if roundChange && c.isProposer() && c.current != nil {
		// If it is locked, propose the old proposal
		// If we have pending request, propose pending request
		if c.current.IsHashLocked() {
			// 일관성을 위해 반드시 이전에 지지했던 바로 그 블록(locked proposal)을 다시 제안해야 합니다.
			r := &istanbul.Request{
				Proposal: c.current.Proposal(), // c.current.Proposal would be the locked proposal by previous proposer, see updateRoundState
			}
			c.sendPreprepare(r) // 잠겨있던 블록을 담아 PRE-PREPARE 메시지 전송
			// 잠겨있지는 않지만, 처리 대기 중인 트랜잭션 요청이 있다면,
		} else if c.current.pendingRequest != nil {
			// 그 요청을 담아 새로운 블록을 제안합니다.
			c.sendPreprepare(c.current.pendingRequest)
		}
	}
	// 타임아웃 타이머 시작
	// 새로운 라운드 변경 타이머를 시작합니다.
	// 만약 이번에 선출된 제안자가 일정 시간 안에 블록을 제안하지 않으면(PRE-PREPARE 메시지를 보내지 않으면),
	// 이 타이머가 만료되어 합의가 또 실패했다고 판단하고 다음 라운드로 넘어가는 절차를 시작합니다.
	c.newRoundChangeTimer()

	logger.Debug("New round", "new_round", newView.Round, "new_seq", newView.Sequence, "new_proposer", c.valSet.GetProposer(), "isProposer", c.isProposer())
	logger.Trace("New round", "new_round", newView.Round, "new_seq", newView.Sequence, "size", c.valSet.Size(), "valSet", c.valSet.List())
}

func (c *core) catchUpRound(view *istanbul.View) {
	logger := c.logger.NewWith("old_round", c.current.Round(), "old_seq", c.current.Sequence(), "old_proposer", c.valSet.GetProposer())

	if view.Round.Cmp(c.current.Round()) > 0 {
		c.roundMeter.Mark(new(big.Int).Sub(view.Round, c.current.Round()).Int64())
	}
	c.waitingForRoundChange = true

	// Need to keep block locked for round catching up
	c.updateRoundState(view, c.valSet, true)
	c.roundChangeSet.Clear(view.Round)

	c.newRoundChangeTimer()
	logger.Warn("[RC] Catch up round", "new_round", view.Round, "new_seq", view.Sequence, "new_proposer", c.valSet.GetProposer())
}

// updateRoundState updates round state by checking if locking block is necessary
func (c *core) updateRoundState(view *istanbul.View, validatorSet istanbul.ValidatorSet, roundChange bool) {
	// Lock only if both roundChange is true and it is locked
	if roundChange && c.current != nil {
		if c.current.IsHashLocked() {
			c.current = newRoundState(view, validatorSet, c.current.GetLockedHash(), c.current.Preprepare, c.current.pendingRequest, c.backend.HasBadProposal)
		} else {
			c.current = newRoundState(view, validatorSet, common.Hash{}, nil, c.current.pendingRequest, c.backend.HasBadProposal)
		}
	} else {
		c.current = newRoundState(view, validatorSet, common.Hash{}, nil, nil, c.backend.HasBadProposal)
	}
	c.currentRoundGauge.Update(c.current.round.Int64())
	if c.current.IsHashLocked() {
		c.hashLockGauge.Update(1)
	} else {
		c.hashLockGauge.Update(0)
	}
}

func (c *core) setState(state State) {
	if c.state != state {
		c.state = state
	}
	if state == StateAcceptRequest {
		c.processPendingRequests()
	}
	c.processBacklog()
}

func (c *core) Address() common.Address {
	return c.address
}

func (c *core) stopFuturePreprepareTimer() {
	if c.futurePreprepareTimer != nil {
		c.futurePreprepareTimer.Stop()
	}
}

func (c *core) stopTimer() {
	c.stopFuturePreprepareTimer()

	if c.roundChangeTimer.Load() != nil {
		c.roundChangeTimer.Load().(*time.Timer).Stop()
	}
}

func (c *core) newRoundChangeTimer() {
	c.stopTimer()

	// TODO-Kaia-Istanbul: Replace &istanbul.DefaultConfig.Timeout to c.config.Timeout
	// set timeout based on the round number
	timeout := time.Duration(atomic.LoadUint64(&istanbul.DefaultConfig.Timeout)) * time.Millisecond
	round := c.current.Round().Uint64()
	if round > 0 {
		timeout += time.Duration(math.Pow(2, float64(round))) * time.Second
	}

	current := c.current
	proposer := c.valSet.GetProposer()

	c.roundChangeTimer.Store(time.AfterFunc(timeout, func() {
		var loc, proposerStr string

		if round == 0 {
			loc = "startNewRound"
		} else {
			loc = "catchUpRound"
		}
		if proposer == nil {
			proposerStr = ""
		} else {
			proposerStr = proposer.String()
		}

		if c.backend.NodeType() == common.CONSENSUSNODE {
			// Write log messages for validator activities analysis
			preparesSize := current.Prepares.Size()
			commitsSize := current.Commits.Size()
			logger.Warn("[RC] timeoutEvent Sent!", "set by", loc, "sequence",
				current.sequence, "round", current.round, "proposer", proposerStr, "preprepare is nil?",
				current.Preprepare == nil, "len(prepares)", preparesSize, "len(commits)", commitsSize)

			if preparesSize > 0 {
				logger.Warn("[RC] Prepares:", "messages", current.Prepares.GetMessages())
			}
			if commitsSize > 0 {
				logger.Warn("[RC] Commits:", "messages", current.Commits.GetMessages())
			}
		}

		c.sendEvent(timeoutEvent{&istanbul.View{
			Sequence: current.sequence,
			Round:    new(big.Int).Add(current.round, common.Big1),
		}})
	}))

	logger.Debug("New RoundChangeTimer Set", "seq", c.current.Sequence(), "round", round, "timeout", timeout)
}

func (c *core) checkValidatorSignature(data []byte, sig []byte) (common.Address, error) {
	return istanbul.CheckValidatorSignature(c.valSet, data, sig)
}

// PrepareCommittedSeal returns a committed seal for the given hash
func PrepareCommittedSeal(hash common.Hash) []byte {
	var buf bytes.Buffer
	buf.Write(hash.Bytes())
	buf.Write([]byte{byte(msgCommit)})
	return buf.Bytes()
}

// Minimum required number of consensus messages to proceed
func RequiredMessageCount(valSet istanbul.ValidatorSet) int {
	var size uint64
	if valSet.IsSubSet() {
		size = valSet.SubGroupSize()
	} else {
		size = valSet.Size()
	}
	// For less than 4 validators, quorum size equals validator count.
	if size < 4 {
		return int(size)
	}
	// Adopted QBFT quorum implementation
	// https://github.com/Consensys/quorum/blob/master/consensus/istanbul/qbft/core/core.go#L312
	return int(math.Ceil(float64(2*size) / 3))
}
