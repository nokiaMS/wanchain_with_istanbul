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
	"github.com/wanchain/go-wanchain/common"
	"github.com/wanchain/go-wanchain/consensus/istanbul"
)

//基于addr创建一个validator结构体.
//		addr: 	validator的地址.
func New(addr common.Address) istanbul.Validator {
	return &defaultValidator{
		address: addr,
	}
}

//从地址addrs创建一个validator的集合.
func NewSet(addrs []common.Address, policy istanbul.ProposerPolicy) istanbul.ValidatorSet {
	return newDefaultSet(addrs, policy)
}

//从validator集合中转化出validator的address列表.
func ExtractValidators(extraData []byte) []common.Address {
	// get the validator addresses
	addrs := make([]common.Address, (len(extraData) / common.AddressLength))
	for i := 0; i < len(addrs); i++ {
		copy(addrs[i][:], extraData[i*common.AddressLength:])
	}

	return addrs
}

//检测extraData是否有效.
// Check whether the extraData is presented in prescribed form
func ValidExtraData(extraData []byte) bool {
	return len(extraData)%common.AddressLength == 0
}
