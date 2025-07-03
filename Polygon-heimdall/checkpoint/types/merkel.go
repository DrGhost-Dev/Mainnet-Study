package types

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/cbergoon/merkletree"
	"github.com/ethereum/go-ethereum/common"

	"github.com/tendermint/crypto/sha3"

	"github.com/maticnetwork/heimdall/helper"
	hmTypes "github.com/maticnetwork/heimdall/types"
)

// ValidateCheckpoint - Validates if checkpoint rootHash matches or not
func ValidateCheckpoint(start uint64, end uint64, rootHash hmTypes.HeimdallHash, checkpointLength uint64, contractCaller helper.IContractCaller, confirmations uint64) (bool, error) {
	// Check if blocks exist locally
	// 전제 조건 확인: "검증에 필요한 데이터가 로컬에 있는가?"
	// contractCaller를 통해 Bor 노드에 "end + confirmations 블록까지 로컬에 가지고 있습니까?"라고 물어봅니다.
	// 검증에 필요한 블록 데이터가 로컬에 없다면, 검증 자체가 불가능하므로 즉시 실패 처리합니다.
	if !contractCaller.CheckIfBlocksExist(end + confirmations) {
		return false, errors.New("blocks not found locally")
	}

	// Compare RootHash
	// 자체 머클 루트 계산 요청 (⭐ 핵심 로직)
	// contractCaller를 통해 Bor 노드에 "start부터 end까지의 머클 루트를 계산해서 알려줘"라고 요청합니다.
	// 이 GetRootHash 함수는 Bor 노드에서 실제 계산을 수행하고, 그 결과(root)만 반환받습니다.
	root, err := contractCaller.GetRootHash(start, end, checkpointLength)
	if err != nil {
		return false, err
	}

	// 최종 교차 검증 및 판정
	// Bor로부터 받은 '실제 계산 결과(root)'와
	// 제안서에 포함되어 있던 '주장된 결과(rootHash)'를
	// 바이트(byte) 단위로 100% 일치하는지 비교합니다.
	if bytes.Equal(root, rootHash.Bytes()) {
		return true, nil
	}

	return false, nil
}

// GetAccountRootHash returns roothash of Validator Account State Tree
func GetAccountRootHash(dividendAccounts []hmTypes.DividendAccount) ([]byte, error) {
	tree, err := GetAccountTree(dividendAccounts)
	if err != nil {
		return nil, err
	}

	return tree.Root.Hash, nil
}

// GetAccountTree returns roothash of Validator Account State Tree
func GetAccountTree(dividendAccounts []hmTypes.DividendAccount) (*merkletree.MerkleTree, error) {
	// Sort the dividendAccounts by ID
	dividendAccounts = hmTypes.SortDividendAccountByAddress(dividendAccounts)
	list := make([]merkletree.Content, len(dividendAccounts))

	for i := 0; i < len(dividendAccounts); i++ {
		list[i] = dividendAccounts[i]
	}

	tree, err := merkletree.NewTreeWithHashStrategy(list, sha3.NewLegacyKeccak256)
	if err != nil {
		return nil, err
	}

	return tree, nil
}

// GetAccountProof returns proof of dividend Account
func GetAccountProof(dividendAccounts []hmTypes.DividendAccount, userAddr hmTypes.HeimdallAddress) ([]byte, uint64, error) {
	// Sort the dividendAccounts by user address
	dividendAccounts = hmTypes.SortDividendAccountByAddress(dividendAccounts)

	var (
		list    = make([]merkletree.Content, len(dividendAccounts))
		account hmTypes.DividendAccount
	)

	index := uint64(0)

	for i := 0; i < len(dividendAccounts); i++ {
		list[i] = dividendAccounts[i]

		if dividendAccounts[i].User.Equals(userAddr) {
			account = dividendAccounts[i]
			if i < 0 {
				return nil, 0, fmt.Errorf("index value cannot be negative: %d", i)
			}
			index = uint64(i)
		}
	}

	tree, err := merkletree.NewTreeWithHashStrategy(list, sha3.NewLegacyKeccak256)
	if err != nil {
		return nil, 0, err
	}

	branchArray, _, err := tree.GetMerklePath(account)

	// concatenate branch array
	proof := appendBytes32(branchArray...)

	return proof, index, err
}

// VerifyAccountProof returns proof of dividend Account
func VerifyAccountProof(dividendAccounts []hmTypes.DividendAccount, userAddr hmTypes.HeimdallAddress, proofToVerify string) (bool, error) {
	proof, _, err := GetAccountProof(dividendAccounts, userAddr)
	if err != nil {
		// nolint: nilerr
		return false, nil
	}

	// check proof bytes
	if bytes.Equal(common.FromHex(proofToVerify), proof) {
		return true, nil
	}

	return false, nil
}

//nolint:unparam
func convertTo32(input []byte) (output [32]byte, err error) {
	l := len(input)
	if l > 32 || l == 0 {
		return
	}

	copy(output[32-l:], input[:])

	return
}

func appendBytes32(data ...[]byte) []byte {
	var result []byte

	for _, v := range data {
		paddedV, err := convertTo32(v)
		if err == nil {
			result = append(result, paddedV[:]...)
		}
	}

	return result
}
