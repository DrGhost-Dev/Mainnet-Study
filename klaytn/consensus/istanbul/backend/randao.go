// Copyright 2024 The Kaia Authors
// This file is part of the Kaia library.
//
// The Kaia library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The Kaia library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the Kaia library. If not, see <http://www.gnu.org/licenses/>.
package backend

import (
	"bytes"
	"errors"
	"math/big"

	lru "github.com/hashicorp/golang-lru"
	"github.com/klaytn/klaytn/accounts/abi/bind/backends"
	"github.com/klaytn/klaytn/blockchain/system"
	"github.com/klaytn/klaytn/blockchain/types"
	"github.com/klaytn/klaytn/common"
	"github.com/klaytn/klaytn/common/hexutil"
	"github.com/klaytn/klaytn/consensus"
	"github.com/klaytn/klaytn/crypto"
	"github.com/klaytn/klaytn/crypto/bls"
	"github.com/klaytn/klaytn/params"
)

// For testing without KIP-113 contract setup
type BlsPubkeyProvider interface {
	// num should be the header number of the block to be verified.
	// Thus, since the state of num does not exist, the state of num-1 must be used.
	GetBlsPubkey(chain consensus.ChainReader, proposer common.Address, num *big.Int) (bls.PublicKey, error)
	ResetBlsCache()
}

type ChainBlsPubkeyProvider struct {
	cache *lru.ARCCache // Cached BlsPublicKeyInfos
}

func newChainBlsPubkeyProvider() *ChainBlsPubkeyProvider {
	cache, _ := lru.NewARC(128)
	return &ChainBlsPubkeyProvider{
		cache: cache,
	}
}

// The default implementation for BlsPubkeyFunc.
// Queries KIP-113 contract and verifies the PoP.
func (p *ChainBlsPubkeyProvider) GetBlsPubkey(chain consensus.ChainReader, proposer common.Address, num *big.Int) (bls.PublicKey, error) {
	infos, err := p.getAllCached(chain, num)
	if err != nil {
		return nil, err
	}

	info, ok := infos[proposer]
	if !ok {
		return nil, errNoBlsPub
	}
	if info.VerifyErr != nil {
		return nil, info.VerifyErr
	}
	return bls.PublicKeyFromBytes(info.PublicKey)
}

func (p *ChainBlsPubkeyProvider) getAllCached(chain consensus.ChainReader, num *big.Int) (system.BlsPublicKeyInfos, error) {
	if item, ok := p.cache.Get(num.Uint64()); ok {
		logger.Trace("BlsPublicKeyInfos cache hit", "number", num.Uint64())
		return item.(system.BlsPublicKeyInfos), nil
	}

	backend := backends.NewBlockchainContractBackend(chain, nil, nil)
	if common.Big0.Cmp(num) == 0 {
		return nil, errors.New("num cannot be zero")
	}
	parentNum := new(big.Int).Sub(num, common.Big1)

	var kip113Addr common.Address
	// Because the system contract Registry is installed at Finalize() of RandaoForkBlock,
	// it is not possible to read KIP113 address from the Registry at RandaoForkBlock.
	// Hence the ChainConfig fallback.
	if chain.Config().IsRandaoForkBlock(num) {
		var ok bool
		kip113Addr, ok = chain.Config().RandaoRegistry.Records[system.Kip113Name]
		if !ok {
			return nil, errors.New("KIP113 address not set in ChainConfig")
		}
	} else if chain.Config().IsRandaoForkEnabled(num) {
		// If no state exist at block number `parentNum`,
		// return the error `consensus.ErrPrunedAncestor`
		pHeader := chain.GetHeaderByNumber(parentNum.Uint64())
		if pHeader == nil {
			return nil, consensus.ErrUnknownAncestor
		}
		_, err := chain.StateAt(pHeader.Root)
		if err != nil {
			return nil, consensus.ErrPrunedAncestor
		}
		kip113Addr, err = system.ReadActiveAddressFromRegistry(backend, system.Kip113Name, parentNum)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, errors.New("Cannot read KIP113 address from registry before Randao fork")
	}

	infos, err := system.ReadKip113All(backend, kip113Addr, parentNum)
	if err != nil {
		return nil, err
	}
	logger.Trace("BlsPublicKeyInfos cache miss", "number", num.Uint64())
	p.cache.Add(num.Uint64(), infos)

	return infos, nil
}

func (p *ChainBlsPubkeyProvider) ResetBlsCache() {
	p.cache.Purge()
}

// Calculate KIP-114 Randao header fields
// https://github.com/klaytn/kips/blob/kip114/KIPs/kip-114.md
// KIP-114 제안에 따라 클레이튼 블록체인에 온체인 난수(On-chain Randomness)를 생성하는 기능의 일부입니다.
// 이더리움의 Randao 메커니즘과 유사하며, 블록 생성자가 예측하거나 조작하기 어려운 난수를 블록 헤더에 포함시키기 위해 사용
func (sb *backend) CalcRandao(number *big.Int, prevMixHash []byte) ([]byte, []byte, error) {
	// BLS 개인키 확인
	if sb.blsSecretKey == nil {
		return nil, nil, errNoBlsKey
	}
	// mixHash 유효성 검사
	if len(prevMixHash) != 32 {
		logger.Error("invalid prevMixHash", "number", number.Uint64(), "prevMixHash", hexutil.Encode(prevMixHash))
		return nil, nil, errInvalidRandaoFields
	}

	// block_num_to_bytes() = num.to_bytes(32, byteorder="big")
	// 서명할 메시지 생성
	msg := calcRandaoMsg(number)

	// calc_random_reveal() = sign(privateKey, headerNumber)
	// 난수 공개 값 계산
	randomReveal := bls.Sign(sb.blsSecretKey, msg[:]).Marshal()

	// calc_mix_hash() = xor(prevMixHash, keccak256(randomReveal))
	// 새로운 mixHash 계산
	mixHash := calcMixHash(randomReveal, prevMixHash)

	return randomReveal, mixHash, nil
}

func (sb *backend) VerifyRandao(chain consensus.ChainReader, header *types.Header, prevMixHash []byte) error {
	if header.Number.Sign() == 0 {
		return nil // Do not verify genesis block
	}

	proposer, err := sb.Author(header)
	if err != nil {
		return err
	}

	// [proposerPubkey, proposerPop] = get_proposer_pubkey_pop()
	// if not pop_verify(proposerPubkey, proposerPop): return False
	proposerPub, err := sb.blsPubkeyProvider.GetBlsPubkey(chain, proposer, header.Number)
	if err != nil {
		return err
	}

	// if not verify(proposerPubkey, newHeader.number, newHeader.randomReveal): return False
	sig := header.RandomReveal
	msg := calcRandaoMsg(header.Number)
	ok, err := bls.VerifySignature(sig, msg, proposerPub)
	if err != nil {
		return err
	} else if !ok {
		return errInvalidRandaoFields
	}

	// if not newHeader.mixHash == calc_mix_hash(prevMixHash, newHeader.randomReveal): return False
	mixHash := calcMixHash(header.RandomReveal, prevMixHash)
	if !bytes.Equal(header.MixHash, mixHash) {
		return errInvalidRandaoFields
	}

	return nil
}

// block_num_to_bytes() = num.to_bytes(32, byteorder="big")
func calcRandaoMsg(number *big.Int) common.Hash {
	return common.BytesToHash(number.Bytes())
}

// calc_mix_hash() = xor(prevMixHash, keccak256(randomReveal))
func calcMixHash(randomReveal, prevMixHash []byte) []byte {
	mixHash := make([]byte, 32)
	revealHash := crypto.Keccak256(randomReveal)
	for i := 0; i < 32; i++ {
		mixHash[i] = prevMixHash[i] ^ revealHash[i]
	}
	return mixHash
}

// At the fork block's parent, pretend that prevMixHash is ZeroMixHash.
func headerMixHash(chain consensus.ChainReader, header *types.Header) []byte {
	if chain.Config().IsRandaoForkBlockParent(header.Number) {
		return params.ZeroMixHash
	} else {
		return header.MixHash
	}
}
