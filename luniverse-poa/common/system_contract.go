package common

import "math/big"

// Virtual sender address used in system contract for `block reward` and `gas delegation`
var VirtualMinerAddress = "0x00000000000000000000000000000000000003e7" // 999

//=============================================================================================
// Addresses of system contracts

// var MultiSigExecutorContractAddress = "0x00000000000000000000000000000000000001f3" // 499
// var InitialAllocationContractAddress = "0x00000000000000000000000000000000000001f4" // 500
var ConsensusContractAddress = "0x00000000000000000000000000000000000001f9"  // 505
var PreStakingContractAddress = "0x00000000000000000000000000000000000001fa" // 506
var StakingContractAddress = "0x00000000000000000000000000000000000001fb"    // 507

//var ContributionPoolContractAddress = "0x00000000000000000000000000000000000001fe" // 510
//var SustainabilityPoolContractAddress = "0x00000000000000000000000000000000000001ff" // 511

var BoardMemberContractAddress = "0x0000000000000000000000000000000000000213" // 531
//=============================================================================================

// Block rewrad information (This information is retrieved from contract)
type RewardPool struct {
	Addr  *string `json:"addr"`
	Ratio *uint16 `json:"ratio"`
	Name  *string `json:"name"`
}

type ConsensusConfig struct {
	// 네트워크의 발전 단계에 따라 보상 분배와 같은 주요 거버넌스 로직을 다르게 적용하기 위한 '단계별 스위치' 역할
	AuthorityGovernanceStage *uint32 `json:"authorityGovernanceStage"`
	BlockPeriod              *uint64 `json:"blockPeriod"`
	// 블록체인 네트워크가 장기적으로 유지하려는 '목표' 가스 사용량, 네트워크 수요에 따라 탄력적으로 변하도록 설계
	TargetGasLimit *big.Int `json:"targetGasLimit"`
	GasLimit       *big.Int `json:"gasLimit"`
	GasPrice       *big.Int `json:"gasPrice"`

	// 해당 블록을 생성한 검증자(Signer/Coinbase)에게 직접 지급될 보상의 비율
	Reward        *big.Int `json:"reward"`
	CoinbaseRatio *uint16  `json:"coinbaseRatio"`
	// 나머지 보상을 분배할 주소 목록입니다. 검증자에게 주고 남은 보상을 미리 정해진 다른 주소(Pool)들로 특정 비율에 맞게 나누어 보낼 수 있습니다.
	RewardPools []RewardPool `json:"rewardPools"`
}

var GetConsensusConfig = "7122b0fd" // ABI to invoke LuniverseConsensus.getAllSystemConfig()
