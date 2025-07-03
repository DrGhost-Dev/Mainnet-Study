package bor

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"

	"github.com/ethereum/go-ethereum/common"

	"github.com/maticnetwork/heimdall/bor/types"
	"github.com/maticnetwork/heimdall/helper"
	hmTypes "github.com/maticnetwork/heimdall/types"
)

// XXXSelectNextProducers selects producers for next span by converting power to tickets
func XXXSelectNextProducers(blkHash common.Hash, spanEligibleVals []hmTypes.Validator, producerCount uint64) (selectedIDs []uint64, err error) {
	if producerCount > math.MaxInt64 {
		return nil, fmt.Errorf("producer count value out of range for int: %d", producerCount)
	}
	if len(spanEligibleVals) <= int(producerCount) {
		for _, val := range spanEligibleVals {
			selectedIDs = append(selectedIDs, uint64(val.ID))
		}

		return
	}

	// extract seed from hash
	seed := helper.ToBytes32(blkHash.Bytes()[:32])
	validatorIndices := convertToSlots(spanEligibleVals)

	selectedIDs, err = ShuffleList(validatorIndices, seed)
	if err != nil {
		return
	}

	return selectedIDs[:producerCount], nil
}

// converts validator power to slots
// TODO remove 2nd loop
func convertToSlots(vals []hmTypes.Validator) (validatorIndices []uint64) {
	for _, val := range vals {
		for val.VotingPower >= types.SlotCost {
			validatorIndices = append(validatorIndices, uint64(val.ID))
			val.VotingPower = val.VotingPower - types.SlotCost
		}
	}

	return validatorIndices
}

//
// New selection algorithm
//

// SelectNextProducers selects producers for next span by converting power to tickets
func SelectNextProducers(blkHash common.Hash, spanEligibleValidators []hmTypes.Validator, producerCount uint64) ([]uint64, error) {
	selectedProducers := make([]uint64, 0)

	if producerCount > math.MaxInt64 {
		return nil, fmt.Errorf("producer count value out of range for int: %d", producerCount)
	}
	// 예외 처리: "추첨할 필요가 없는 경우"
	// 뽑아야 할 프로듀서 수(producerCount)가 후보자 수보다 많거나 같으면,
	// 모든 후보자를 프로듀서로 임명하고 즉시 종료합니다.
	if len(spanEligibleValidators) <= int(producerCount) {
		for _, validator := range spanEligibleValidators {
			selectedProducers = append(selectedProducers, uint64(validator.ID))
		}

		return selectedProducers, nil
	}

	// extract seed from hash
	// 추첨 시드(Seed) 생성: "예측 불가능성 확보"
	// 이전 블록의 해시값(blkHash)을 가져와서 무작위 추첨의 기준이 될 '시드(seed)'를 만듭니다.
	// 블록 해시는 예측이 불가능하므로, 누구도 다음 프로듀서를 미리 예측할 수 없게 됩니다.
	seedBytes := helper.ToBytes32(blkHash.Bytes()[:32])
	//nolint: gosec
	seed := int64(binary.BigEndian.Uint64(seedBytes[:]))
	// nolint: staticcheck
	rand.Seed(seed)

	// weighted range from validators' voting power
	// 응모권 준비: "스테이킹 지분에 따른 가중치 부여"
	// 각 검증인의 투표력(Voting Power, 즉 스테이킹 지분)을 리스트로 만듭니다.
	votingPower := make([]uint64, len(spanEligibleValidators))
	for idx, validator := range spanEligibleValidators {
		if validator.VotingPower < 0 {
			return nil, fmt.Errorf("voting power value is negative: %d", validator.VotingPower)
		}
		votingPower[idx] = uint64(validator.VotingPower)
	}

	// 추첨판 만들기
	// createWeightedRanges 함수를 호출하여 '누적 확률표'를 만듭니다.
	// 예를 들어, 지분이 [10, 30, 60] 이라면 -> [10, 40, 100] 이라는 추첨판을 만듭니다.
	// totalVotingPower는 모든 지분의 합계(여기서는 100)가 됩니다.
	weightedRanges, totalVotingPower := createWeightedRanges(votingPower)
	// select producers, with replacement
	for i := uint64(0); i < producerCount; i++ {
		/*
			random must be in [1, totalVotingPower] to avoid situation such as
			2 validators with 1 staking power each.
			Weighted range will look like (1, 2)
			Rolling inclusive will have a range of 0 - 2, making validator with staking power 1 chance of selection = 66%
		*/
		// 행운의 숫자 뽑기: 1부터 전체 지분 합계(totalVotingPower) 사이에서 무작위 숫자를 뽑습니다.
		targetWeight := randomRangeInclusive(1, totalVotingPower)
		// 당첨자 찾기: 뽑힌 숫자가 '누적 확률표(weightedRanges)'의 어느 구간에 속하는지
		// 이진 탐색(binarySearch)으로 매우 빠르게 찾아냅니다.
		// 예: 숫자가 35가 나왔다면, 40 구간에 속하므로 두 번째 검증인이 당첨됩니다.
		index := binarySearch(weightedRanges, targetWeight)
		// 당첨자 등록: 당첨된 검증인의 ID를 최종 목록에 추가합니다.
		selectedProducers = append(selectedProducers, spanEligibleValidators[index].ID.Uint64())
	}

	return selectedProducers[:producerCount], nil
}

func binarySearch(array []uint64, search uint64) int {
	if len(array) == 0 {
		return -1
	}

	l := 0
	r := len(array) - 1

	for l < r {
		mid := (l + r) / 2
		if array[mid] >= search {
			r = mid
		} else {
			l = mid + 1
		}
	}

	return l
}

// randomRangeInclusive produces unbiased pseudo random in the range [min, max]. Uses rand.Uint64() and can be seeded beforehand.
func randomRangeInclusive(minV uint64, maxV uint64) uint64 {
	if maxV <= minV {
		return maxV
	}

	rangeLength := maxV - minV + 1
	maxAllowedValue := math.MaxUint64 - math.MaxUint64%rangeLength - 1
	randomValue := rand.Uint64() //nolint

	// reject anything that is beyond the reminder to avoid bias
	for randomValue >= maxAllowedValue {
		randomValue = rand.Uint64() //nolint
	}

	return minV + randomValue%rangeLength
}

// createWeightedRanges converts array [1, 2, 3] into cumulative form [1, 3, 6]
func createWeightedRanges(weights []uint64) ([]uint64, uint64) {
	weightedRanges := make([]uint64, len(weights))

	totalWeight := uint64(0)
	for i := 0; i < len(weightedRanges); i++ {
		totalWeight += weights[i]
		weightedRanges[i] = totalWeight
	}

	return weightedRanges, totalWeight
}
