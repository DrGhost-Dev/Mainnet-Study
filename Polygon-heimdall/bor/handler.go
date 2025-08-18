package bor

import (
	"errors"
	"strconv"

	sdk "github.com/cosmos/cosmos-sdk/types"

	"github.com/maticnetwork/heimdall/bor/types"
	"github.com/maticnetwork/heimdall/common"
	"github.com/maticnetwork/heimdall/helper"
)

// NewHandler returns a handler for "bor" type messages.
func NewHandler(k Keeper) sdk.Handler {
	return func(ctx sdk.Context, msg sdk.Msg) sdk.Result {
		ctx = ctx.WithEventManager(sdk.NewEventManager())

		switch msg := msg.(type) {
		case types.MsgProposeSpan,
			types.MsgProposeSpanV2:
			return HandleMsgProposeSpan(ctx, msg, k)
		default:
			return sdk.ErrTxDecode("Invalid message in bor module").Result()
		}
	}
}

// HandleMsgProposeSpan handles proposeSpan msg
// HandleMsgProposeSpan 함수는 '스팬 제안' 메시지를 처리합니다.
// 검증자가 보낸 새 Span 제안을 파싱·검증
// ctx: 블록체인의 현재 상태(블록 높이, 시간 등) 정보
// msg: 검증인(노드)이 보낸 '스팬 제안' 메시지
// k:   블록체인 상태를 읽거나 쓸 수 있는 Keeper(관리자)
func HandleMsgProposeSpan(ctx sdk.Context, msg sdk.Msg, k Keeper) sdk.Result {
	// 메시지 버전 처리 (하드포크 호환성)
	// 앞으로 처리할 메시지를 담을 빈 변수를 선언
	var proposeMsg types.MsgProposeSpanV2
	switch msg := msg.(type) {
	case types.MsgProposeSpan:
		if ctx.BlockHeight() >= helper.GetDanelawHeight() {
			err := errors.New("msg span is not allowed after Danelaw hardfork height")
			k.Logger(ctx).Error(err.Error())
			return sdk.ErrTxDecode(err.Error()).Result()
		}
		proposeMsg = types.MsgProposeSpanV2{
			ID:         msg.ID,
			Proposer:   msg.Proposer,
			StartBlock: msg.StartBlock,
			EndBlock:   msg.EndBlock,
			ChainID:    msg.ChainID,
			Seed:       msg.Seed,
		}
	case types.MsgProposeSpanV2:
		// 현재 블록 높이가 'Danelaw' 하드포크 이전이라면, 신버전 메시지는 거부합니다.
		if ctx.BlockHeight() < helper.GetDanelawHeight() {
			err := errors.New("msg span v2 is not allowed before Danelaw hardfork height")
			k.Logger(ctx).Error(err.Error())
			return sdk.ErrTxDecode(err.Error()).Result()
		}
		proposeMsg = msg
	}

	k.Logger(ctx).Debug("✅ Validating proposed span msg",
		"proposer", proposeMsg.Proposer.String(),
		"spanId", proposeMsg.ID,
		"startBlock", proposeMsg.StartBlock,
		"endBlock", proposeMsg.EndBlock,
		"seed", proposeMsg.Seed.String(),
	)

	// chainManager params
	// 체인 ID 검증
	// 시스템에 설정된 체인 매개변수
	params := k.chainKeeper.GetParams(ctx)
	chainParams := params.ChainParams

	// check chain id
	// 메시지에 담긴 체인 ID가 시스템의 Bor 체인 ID와 일치하는지 확인합니다.
	if chainParams.BorChainID != proposeMsg.ChainID {
		k.Logger(ctx).Error("Invalid Bor chain id", "msgChainID", proposeMsg.ChainID)
		return common.ErrInvalidBorChainID(k.Codespace()).Result()
	}

	// check if last span is up or if greater diff than threshold is found between validator set
	// 스팬 연속성 검증
	// 블록체인에 저장된 가장 마지막 스팬 정보를 가져옵니다.
	lastSpan, err := k.GetLastSpan(ctx)
	if err != nil {
		k.Logger(ctx).Error("Unable to fetch last span", "Error", err)
		return common.ErrSpanNotFound(k.Codespace()).Result()
	}

	// Validate span continuity
	// 제안된 스팬이 이전 스팬에 바로 이어지는지 확인합니다.
	// 조건 1: 새 스팬 ID = 이전 스팬 ID + 1
	// 조건 2: 새 스팬 시작 블록 = 이전 스팬 종료 블록 + 1
	// 조건 3: 새 스팬 종료 블록 >= 새 스팬 시작 블록
	if lastSpan.ID+1 != proposeMsg.ID || proposeMsg.StartBlock != lastSpan.EndBlock+1 || proposeMsg.EndBlock < proposeMsg.StartBlock {
		k.Logger(ctx).Error("Blocks not in continuity",
			"lastSpanId", lastSpan.ID,
			"spanId", proposeMsg.ID,
			"lastSpanStartBlock", lastSpan.StartBlock,
			"lastSpanEndBlock", lastSpan.EndBlock,
			"spanStartBlock", proposeMsg.StartBlock,
			"spanEndBlock", proposeMsg.EndBlock,
		)

		return common.ErrSpanNotInContinuity(k.Codespace()).Result()
	}

	// Validate Span duration
	// 스팬 길이 검증
	// 시스템에 설정된 스팬의 고정 길이(블록 개수)를 가져옵니다.
	spanDuration := k.GetParams(ctx).SpanDuration
	// 제안된 스팬의 길이가 설정된 길이와 정확히 일치하는지 확인합니다.
	if spanDuration != (proposeMsg.EndBlock - proposeMsg.StartBlock + 1) {
		k.Logger(ctx).Error("Span duration of proposed span is wrong",
			"proposedSpanDuration", proposeMsg.EndBlock-proposeMsg.StartBlock+1,
			"paramsSpanDuration", spanDuration,
		)

		return common.ErrInvalidSpanDuration(k.Codespace()).Result()
	}

	// add events
	// 이벤트 발생
	// 모든 검증을 통과했으므로, "새로운 스팬이 성공적으로 제안되었다"는 이벤트를 발생시킵니다.
	// 다른 노드나 외부 시스템이 이 정보를 활용할 수 있습니다.
	// 이벤트 관리자를 불러옴
	ctx.EventManager().EmitEvents(sdk.Events{
		// 새로운 이벤트를 만듦
		sdk.NewEvent(
			types.EventTypeProposeSpan,
			sdk.NewAttribute(sdk.AttributeKeyModule, types.AttributeValueCategory),
			sdk.NewAttribute(types.AttributeKeySpanID, strconv.FormatUint(proposeMsg.ID, 10)),
			sdk.NewAttribute(types.AttributeKeySpanStartBlock, strconv.FormatUint(proposeMsg.StartBlock, 10)),
			sdk.NewAttribute(types.AttributeKeySpanEndBlock, strconv.FormatUint(proposeMsg.EndBlock, 10)),
		),
	})

	// draft result with events
	// 성공 결과 반환
	// 발생시킨 이벤트 정보를 포함하여 성공 결과를 반환합니다.
	return sdk.Result{
		Events: ctx.EventManager().Events(),
	}
}
