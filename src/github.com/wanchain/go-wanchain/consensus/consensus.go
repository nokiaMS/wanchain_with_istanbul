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
package consensus

import (
	"github.com/wanchain/go-wanchain/common"
	"github.com/wanchain/go-wanchain/core/state"
	"github.com/wanchain/go-wanchain/core/types"
	"github.com/wanchain/go-wanchain/params"
	"github.com/wanchain/go-wanchain/rpc"
	"github.com/wanchain/go-wanchain/p2p"
)

// ChainReader defines a small collection of methods needed to access the local
// blockchain during header and/or uncle verification.
type ChainReader interface {
	// Config retrieves the blockchain's chain configuration.
	Config() *params.ChainConfig

	// CurrentHeader retrieves the current header from the local chain.
	CurrentHeader() *types.Header

	// GetHeader retrieves a block header from the database by hash and number.
	GetHeader(hash common.Hash, number uint64) *types.Header

	// GetHeaderByNumber retrieves a block header from the database by number.
	GetHeaderByNumber(number uint64) *types.Header

	// GetHeaderByHash retrieves a block header from the database by its hash.
	GetHeaderByHash(hash common.Hash) *types.Header

	// GetBlock retrieves a block from the database by hash and number.
	GetBlock(hash common.Hash, number uint64) *types.Block
}

// Engine is an algorithm agnostic consensus engine.
type Engine interface {
	// Author retrieves the Ethereum address of the account that minted the given
	// block, which may be different from the header's coinbase if a consensus
	// engine is based on signatures.
	Author(header *types.Header) (common.Address, error)

	// VerifyHeader checks whether a header conforms to the consensus rules of a
	// given engine. Verifying the seal may be done optionally here, or explicitly
	// via the VerifySeal method.
	VerifyHeader(chain ChainReader, header *types.Header, seal bool) error

	// VerifyHeaders is similar to VerifyHeader, but verifies a batch of headers
	// concurrently. The method returns a quit channel to abort the operations and
	// a results channel to retrieve the async verifications (the order is that of
	// the input slice).
	VerifyHeaders(chain ChainReader, headers []*types.Header, seals []bool) (chan<- struct{}, <-chan error)

	// VerifyUncles verifies that the given block's uncles conform to the consensus
	// rules of a given engine.
	VerifyUncles(chain ChainReader, block *types.Block) error

	// VerifySeal checks whether the crypto seal on a header is valid according to
	// the consensus rules of the given engine.
	VerifySeal(chain ChainReader, header *types.Header) error

	// Prepare initializes the consensus fields of a block header according to the
	// rules of a particular engine. The changes are executed inline.
	//共识算法调用之前的准备工作. 由各个共识算法进行重载.
	Prepare(chain ChainReader, header *types.Header, mining bool) error

	// Finalize runs any post-transaction state modifications (e.g. block rewards)
	// and assembles the final block.
	// Note: The block header and state database might be updated to reflect any
	// consensus rules that happen at finalization (e.g. block rewards).
	//Finalize函数用于对于交易执行完成之后对状态进行修改并且组装成最终的区块。在这个过程中区块头及状态数据库会发生变化来反映在finalization时的共识规则。
	Finalize(chain ChainReader, header *types.Header, state *state.StateDB, txs []*types.Transaction,
		uncles []*types.Header, receipts []*types.Receipt) (*types.Block, error)

	// Seal generates a new block for the given input block with the local miner's
	// seal place on top.
	Seal(chain ChainReader, block *types.Block, stop <-chan struct{}) (*types.Block, error)

	// APIs returns the RPC APIs this consensus engine provides.
	APIs(chain ChainReader) []rpc.API

	// Protocol returns the protocol for this consensus
	Protocol() Protocol	//返回共识算法支持的协议.
}

// Handler should be implemented is the consensus needs to handle and send peer's message
type Handler interface {
	// NewChainHead handles a new head block comes
	NewChainHead() error

	// HandleMsg handles a message from peer
	HandleMsg(address common.Address, data p2p.Msg) (bool, error)

	// SetBroadcaster sets the broadcaster to send message to peers
	SetBroadcaster(Broadcaster)
}

// PoW is a consensus engine based on proof-of-work.
type PoW interface {
	Engine

	// Hashrate returns the current mining hashrate of a PoW consensus engine.
	Hashrate() float64
}

// Istanbul is a consensus engine to avoid byzantine failure
//istanbul 是一个采用拜占庭容错算法的共识算法.
type Istanbul interface {
	Engine

	// Start starts the engine
	//启动ibft engine. 链操作函数集,当前块,
	Start(chain ChainReader, currentBlock func() *types.Block, hasBadBlock func(hash common.Hash) bool) error

	// Stop stops the engine
	//停止ibft engine.
	Stop() error
}

