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

package validator

import (
	"math"
	"reflect"
	"sort"
	"sync"

	"github.com/wanchain/go-wanchain/common"
	"github.com/wanchain/go-wanchain/consensus/istanbul"
)

//默认的validator结构.
type defaultValidator struct {
	address common.Address 	//validator地址.
}

//返回validator地址.
func (val *defaultValidator) Address() common.Address {
	return val.address
}

//返回validator地址的字符串表示形式.
func (val *defaultValidator) String() string {
	return val.Address().String()
}

// ----------------------------------------------------------------------------

//validator的默认集合结构体.
type defaultSet struct {
	validators istanbul.Validators 	//validator集合.
	policy     istanbul.ProposerPolicy 	//proposer选取策略.

	proposer    istanbul.Validator	//当前的proposer.
	validatorMu sync.RWMutex	//锁.
	selector    istanbul.ProposalSelector	//proposer选举方法.
}

//创建default validator set.
//		addrs:	validators的地址.
//		policy:	proposer选举策略.
//		返回:	defaultSet结构.
func newDefaultSet(addrs []common.Address, policy istanbul.ProposerPolicy) *defaultSet {
	valSet := &defaultSet{}	//空defaultSet.

	valSet.policy = policy	//设置选举策略。

	/* 把validators设置到defaultSet中。 */
	// init validators
	valSet.validators = make([]istanbul.Validator, len(addrs))
	for i, addr := range addrs {
		valSet.validators[i] = New(addr)
	}
	// sort validator
	sort.Sort(valSet.validators)	//排序。

	//设置最初的proposer。
	// init proposer
	if valSet.Size() > 0 {
		//默认的proposer为排序后的validators的第一个.
		valSet.proposer = valSet.GetByIndex(0)
	}

	valSet.selector = roundRobinProposer //设置proposer选举方法为roundRobin.
	/* 如果策略是Sticky，那么设置 proposer selector为stickyProposer。*/
	if policy == istanbul.Sticky {
		valSet.selector = stickyProposer
	}

	return valSet 	//返回validator列表.
}

//返回validators个数.
func (valSet *defaultSet) Size() int {
	valSet.validatorMu.RLock()
	defer valSet.validatorMu.RUnlock()
	return len(valSet.validators)
}

//返回validators列表.
func (valSet *defaultSet) List() []istanbul.Validator {
	valSet.validatorMu.RLock()
	defer valSet.validatorMu.RUnlock()
	return valSet.validators
}

//按照index获得validator.
func (valSet *defaultSet) GetByIndex(i uint64) istanbul.Validator {
	valSet.validatorMu.RLock()
	defer valSet.validatorMu.RUnlock()
	if i < uint64(valSet.Size()) {
		return valSet.validators[i]
	}
	return nil
}

//按照address获得validator.
func (valSet *defaultSet) GetByAddress(addr common.Address) (int, istanbul.Validator) {
	for i, val := range valSet.List() {
		if addr == val.Address() {
			return i, val
		}
	}
	return -1, nil
}

//获得当前proposer.
func (valSet *defaultSet) GetProposer() istanbul.Validator {
	return valSet.proposer
}

//判断当前address是不是proposer.
func (valSet *defaultSet) IsProposer(address common.Address) bool {
	_, val := valSet.GetByAddress(address)
	return reflect.DeepEqual(valSet.GetProposer(), val)
}

//計算新的proposer.
//lastProposer: 当前proposer地址，
//round: 轮次
func (valSet *defaultSet) CalcProposer(lastProposer common.Address, round uint64) {
	valSet.validatorMu.RLock()	//加读锁。
	defer valSet.validatorMu.RUnlock()  //採用defer释放锁是一个好的方法，函数退出时释放锁。
	valSet.proposer = valSet.selector(valSet, lastProposer, round)	//选出新的proposer.选举策略在newDefaultSet()时就设置好了。
}

//计算proposer选举时候使用的seed.
func calcSeed(valSet istanbul.ValidatorSet, proposer common.Address, round uint64) uint64 {
	offset := 0
	if idx, val := valSet.GetByAddress(proposer); val != nil {
		offset = idx
	}
	return uint64(offset) + round
}

//判断地址是否为空，为空返回true，不为空返回false.
func emptyAddress(addr common.Address) bool {
	return addr == common.Address{}	//返回true/false.
}

//用roundRobin的方法选举proposer, 默认proposer选举的方法是使用的roundRobin.
//		valSet;	validator集合.
//		proposer:	当前proposer地址.
//		round:	round编号.
//		结果:	返回选举出来的proposer.
func roundRobinProposer(valSet istanbul.ValidatorSet, proposer common.Address, round uint64) istanbul.Validator {
	/* 没有validator，返回空。*/
	if valSet.Size() == 0 {
		return nil
	}

	/* 设置种子的数值。 */
	seed := uint64(0)
	if emptyAddress(proposer) {
		seed = round
	} else {
		seed = calcSeed(valSet, proposer, round) + 1
	}
	pick := seed % uint64(valSet.Size())
	return valSet.GetByIndex(pick)
}

//用stickyProposer的方法选举proposer, 默认为使用roundRobin方法.
func stickyProposer(valSet istanbul.ValidatorSet, proposer common.Address, round uint64) istanbul.Validator {
	if valSet.Size() == 0 {
		return nil
	}
	seed := uint64(0)
	if emptyAddress(proposer) {
		seed = round
	} else {
		seed = calcSeed(valSet, proposer, round)
	}
	pick := seed % uint64(valSet.Size())
	return valSet.GetByIndex(pick)
}

//增加一个validator.
func (valSet *defaultSet) AddValidator(address common.Address) bool {
	valSet.validatorMu.Lock()
	defer valSet.validatorMu.Unlock()
	for _, v := range valSet.validators {
		if v.Address() == address {
			return false
		}
	}
	valSet.validators = append(valSet.validators, New(address))
	// TODO: we may not need to re-sort it again
	// sort validator
	sort.Sort(valSet.validators)
	return true
}

//移除一个validator.
func (valSet *defaultSet) RemoveValidator(address common.Address) bool {
	valSet.validatorMu.Lock()
	defer valSet.validatorMu.Unlock()

	for i, v := range valSet.validators {
		if v.Address() == address {
			valSet.validators = append(valSet.validators[:i], valSet.validators[i+1:]...)
			return true
		}
	}
	return false
}

//获得一个新的validators的拷贝.
func (valSet *defaultSet) Copy() istanbul.ValidatorSet {
	valSet.validatorMu.RLock()
	defer valSet.validatorMu.RUnlock()

	addresses := make([]common.Address, 0, len(valSet.validators))
	for _, v := range valSet.validators {
		addresses = append(addresses, v.Address())
	}
	return NewSet(addresses, valSet.policy)
}

//F().
func (valSet *defaultSet) F() int { return int(math.Ceil(float64(valSet.Size())/3)) - 1 }

//获得选举策略.
func (valSet *defaultSet) Policy() istanbul.ProposerPolicy { return valSet.policy }
