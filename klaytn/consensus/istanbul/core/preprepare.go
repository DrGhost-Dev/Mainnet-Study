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
// This file is derived from quorum/consensus/istanbul/core/preprepare.go (2018/06/04).
// Modified and improved for the klaytn development.
// Modified and improved for the Kaia development.

package core

import (
	"time"

	"github.com/klaytn/klaytn/blockchain/types"
	"github.com/klaytn/klaytn/consensus"
	"github.com/klaytn/klaytn/consensus/istanbul"
)

func (c *core) sendPreprepare(request *istanbul.Request) {
	logger := c.logger.NewWith("state", c.state)

	// 헤더에 현재 라운드 번호 기록
	// 제안된 블록 헤더에 현재 합의 라운드(c.currentView().Round) 번호를 명확히 기록합니다.
	// 이를 통해 모든 메시지가 어떤 라운드에 속하는지 명확히 구분할 수 있습니다.
	header := types.SetRoundToHeader(request.Proposal.Header(), c.currentView().Round.Int64())
	// 라운드 번호가 기록된 새 헤더로 블록을 업데이트합니다.
	request.Proposal = request.Proposal.WithSeal(header)

	// If I'm the proposer and I have the same sequence with the proposal
	// 자격 확인: "내가 정말로 제안할 자격이 있는가?"
	// 현재 노드가 제안할 블록의 높이(Sequence)와 일치하는 뷰를 가지고 있고,
	// 동시에 현재 라운드의 정식 제안자(isProposer)가 맞는지 최종적으로 확인합니다.
	if c.current.Sequence().Cmp(request.Proposal.Number()) == 0 && c.isProposer() {
		// PRE-PREPARE 메시지 생성
		curView := c.currentView()
		// Preprepare 구조체에 뷰 정보와 제안할 블록(Proposal)을 담아
		// RLP 인코딩하여 네트워크로 전송할 수 있는 바이트 형태로 만듭니다.
		preprepare, err := Encode(&istanbul.Preprepare{
			View:     curView,
			Proposal: request.Proposal,
		})
		if err != nil {
			logger.Error("Failed to encode", "view", curView)
			return
		}
		// 메시지 브로드캐스트 (전파)
		// 최종적으로 생성된 PRE-PREPARE 메시지를
		// 모든 합의 노드(Validator Set)에게 브로드캐스트합니다.
		c.broadcast(&message{
			Hash: request.Proposal.ParentHash(),
			Code: msgPreprepare,
			Msg:  preprepare,
		})
	}
}

// handlePreprepare는 PRE-PREPARE 메시지를 처리하는 함수입니다.
// 이 함수는 위원회 노드가 제안자로부터 제안 메시지를 받았을 때 호출됩니다.
func (c *core) handlePreprepare(msg *message, src istanbul.Validator) error {
	// 로깅을 위해 메시지 발신자(from)와 현재 상태(state) 정보를 포함한 로거를 생성합니다.
	logger := c.logger.NewWith("from", src, "state", c.state)

	// Decode PRE-PREPARE
	// 메시지 해석 (Decoding)
	// 네트워크로 받은 메시지를 Preprepare 구조체로 디코딩(해석)합니다.
	var preprepare *istanbul.Preprepare
	err := msg.Decode(&preprepare)
	if err != nil {
		logger.Error("Failed to decode message", "code", msg.Code, "err", err)
		return errInvalidMessage
	}

	// Ensure we have the same view with the PRE-PREPARE message
	// If it is old message, see if we need to broadcast COMMIT
	// --- 뷰(View) 검증: 지금 처리할 메시지인가? ---
	// 메시지에 포함된 블록 높이(Sequence)와 라운드(Round)가 현재 노드의 뷰와 일치하는지 확인합니다.
	if err := c.checkMessage(msgPreprepare, preprepare.View); err != nil {
		// 만약 낡은(Old) 메시지라면 (errOldMessage),
		if err == errOldMessage {
			// 이전에 이미 합의가 끝난 블록에 대한 제안일 수 있습니다.
			// 뒤처진 다른 노드를 위해, 해당 블록에 대한 COMMIT 메시지를 다시 전파해줍니다.
			// 먼저, 해당 블록 시점의 검증자 집합(valSet)과 제안자 정보를 계산합니다.
			// Get validator set for the given proposal
			valSet := c.backend.ParentValidators(preprepare.Proposal).Copy()
			previousProposer := c.backend.GetProposer(preprepare.Proposal.Number().Uint64() - 1)
			valSet.CalcProposer(previousProposer, preprepare.View.Round.Uint64())
			// Broadcast COMMIT if it is an existing block
			// 1. The proposer needs to be a proposer matches the given (Sequence + Round)
			// 2. The given block must exist
			// 메시지를 보낸 노드가 당시의 정식 제안자였고, 해당 블록이 실제로 존재한다면 COMMIT을 보냅니다.
			if valSet.IsProposer(src.Address()) && c.backend.HasPropsal(preprepare.Proposal.Hash(), preprepare.Proposal.Number()) {
				c.sendCommitForOldBlock(preprepare.View, preprepare.Proposal.Hash(), preprepare.Proposal.ParentHash())
				return nil
			}
		}
		return err
	}

	// Check if the message comes from current proposer
	// --- 제안자(Proposer) 검증: 올바른 사람이 보냈는가? ---
	// 메시지를 보낸 노드(src)가 현재 뷰의 정식 제안자가 맞는지 확인합니다.
	if !c.valSet.IsProposer(src.Address()) {
		// 제안자가 아닌 노드가 보낸 PRE-PREPARE 메시지는 규칙 위반이므로 무시합니다.
		logger.Warn("Ignore preprepare messages from non-proposer")
		return errNotFromProposer
	}

	// Verify the proposal we received
	// --- 블록(Proposal) 검증: 제안된 내용에 문제는 없는가? ---
	// 제안된 블록의 데이터(트랜잭션, 서명 등)가 유효한지 검증합니다.
	if duration, err := c.backend.Verify(preprepare.Proposal); err != nil {
		logger.Warn("Failed to verify proposal", "err", err, "duration", duration)
		// if it's a future block, we will handle it again after the duration
		// 만약 블록의 타임스탬프가 미래로 설정된 '미래 블록'이라면 (ErrFutureBlock),
		if err == consensus.ErrFutureBlock {
			// 지금 처리하지 않고, 해당 시간이 될 때까지 기다렸다가 다시 처리하도록 타이머를 설정합니다.
			c.stopFuturePreprepareTimer()
			c.futurePreprepareTimer = time.AfterFunc(duration, func() {
				c.sendEvent(backlogEvent{
					src:  src.Address(),
					msg:  msg,
					Hash: msg.Hash,
				})
			})
		} else {
			// 그 외의 검증 실패는 블록 자체에 문제가 있다는 의미이므로, 라운드 변경을 시작합니다.
			c.sendNextRoundChange("handlePreprepare. Proposal verification failure. Not ErrFutureBlock")
		}
		return err
	}

	// Here is about to accept the PRE-PREPARE
	// --- 최종 수락 결정: 이 제안을 받아들일 것인가? ---
	// 모든 검증을 통과하고, 노드가 요청을 수락할 수 있는 상태(StateAcceptRequest)일 때 아래 로직을 실행합니다.
	if c.state == StateAcceptRequest {
		// Send ROUND CHANGE if the locked proposal and the received proposal are different
		// CASE 1: 내가 이전에 다른 블록에 투표해서 '잠겨(Locked)'있는 상태일 경우
		if c.current.IsHashLocked() {
			// 현재 라운드 번호를 잠겨있는 블록 헤더에 설정합니다.
			header := types.SetRoundToHeader(c.current.Preprepare.Proposal.Header(), c.currentView().Round.Int64())
			c.current.Preprepare.Proposal = c.current.Preprepare.Proposal.WithSeal(header)

			// 만약 이번에 제안된 블록이 내가 잠겨있는 블록과 해시가 같다면,
			if preprepare.Proposal.Hash() == c.current.GetLockedHash() {
				logger.Warn("Received preprepare message of the hash locked proposal and change state to prepared")
				// Broadcast COMMIT and enters Prepared state directly
				// "내가 지지하던 블록이 제안되었으므로 강력하게 동의한다"는 의미로, PREPARE 단계를 건너뛰고 바로 COMMIT을 보냅니다.
				c.acceptPreprepare(preprepare)
				c.setState(StatePrepared) // 상태를 바로 'Prepared'로 변경
				c.sendCommit()            // COMMIT 메시지 전송

				if vrank != nil {
					vrank.Log()
				}
				vrank = NewVrank(*c.currentView(), c.valSet.SubList(preprepare.Proposal.ParentHash(), c.currentView()))
			} else {
				// Send round change
				// 잠겨있는 블록과 다른 블록이 제안되었다면, 합의가 깨진 것으로 보고 라운드 변경을 시작합니다.
				c.sendNextRoundChange("handlePreprepare. HashLocked, but received hash is different from locked hash")
			}
		} else {
			// Either
			//   1. the locked proposal and the received proposal match
			//   2. we have no locked proposal
			// CASE 2: 잠겨있지 않은 일반적인 경우
			// 제안을 수락합니다.
			c.acceptPreprepare(preprepare)
			// 상태를 'Preprepared'로 변경합니다. (제안을 받았고, 동의할 준비가 되었다는 의미)
			c.setState(StatePreprepared)
			// "나도 이 제안에 동의한다"는 의미의 PREPARE 메시지를 다른 모든 노드에게 전파합니다.
			c.sendPrepare()

			if vrank != nil {
				vrank.Log()
			}
			vrank = NewVrank(*c.currentView(), c.valSet.SubList(preprepare.Proposal.ParentHash(), c.currentView()))
		}
	}

	return nil
}

func (c *core) acceptPreprepare(preprepare *istanbul.Preprepare) {
	c.consensusTimestamp = time.Now()
	c.current.SetPreprepare(preprepare)
}
