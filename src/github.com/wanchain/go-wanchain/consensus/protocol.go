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

// Package consensus implements different Ethereum consensus engines.
//consensus package用来实现不同的ethereum consensusm engine.
package consensus

import (
	"github.com/wanchain/go-wanchain/common"
	"github.com/wanchain/go-wanchain/core/types"
)

// Constants to match up protocol versions and messages
const (
	Eth62 = 62	//以太坊协议版本.
	Eth63 = 63 //以太坊协议版本.
)

var (
	//以太坊协议定义. 不同的共识算法有不同的协议标识,此处的EthProtocol协议标识是给ethash算法那和clique算法使用的.
	//ibft算法有自己的协议标识.(全工程搜索EthProtocol会发现ethash和clique使用此处的EthProtocol作为共识协议标识.)
	EthProtocol = Protocol{
		Name:     "eth",	//协议名称.
		Versions: []uint{Eth62, Eth63},	//协议版本.
		Lengths:  []uint64{17, 8},	//不同的协议版本对应的消息编号.
	}
)

// Protocol defines the protocol of the consensus
type Protocol struct {		//定义了consensus的协议.
	// Official short name of the protocol used during capability negotiation.
	Name string		//协议名称.
	// Supported versions of the eth protocol (first is primary).
	Versions []uint		//协议版本.
	// Number of implemented message corresponding to different protocol versions.
	Lengths []uint64	//消息编号.
}

// Broadcaster defines the interface to enqueue blocks to fetcher and find peer
type Broadcaster interface {
	// Enqueue add a block into fetcher queue
	Enqueue(id string, block *types.Block)	//把一个block放入到fetcher队列.
	// FindPeers retrives peers by addresses
	FindPeers(map[common.Address]bool) map[common.Address]Peer	//通过地址列表查找peers.
}

// Peer defines the interface to communicate with peer
type Peer interface {		//peer定义了和对端通信的接口.
	// Send sends the message to this peer
	Send(msgcode uint64, data interface{}) error
}
