// Copyright 2016 The go-ethereum Authors
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

package miner

import (
	"container/ring"
	"sync"

	"github.com/wanchain/go-wanchain/common"
	"github.com/wanchain/go-wanchain/core/types"
	"github.com/wanchain/go-wanchain/log"
)

// headerRetriever is used by the unconfirmed block set to verify whether a previously
// mined block is part of the canonical chain or not.
//用来判断一个已经挖掘出来的块是否是主链的一部分。
type headerRetriever interface {
	// GetHeaderByNumber retrieves the canonical header associated with a block number.
	GetHeaderByNumber(number uint64) *types.Header	//在主链上按照number获得header。
}

// unconfirmedBlock is a small collection of metadata about a locally mined block
// that is placed into a unconfirmed set for canonical chain inclusion tracking.
//unconfirmedBlock代表一个本地挖掘出来的块，这个块被放在unconfirmed集合中准备进行主链包含检测。
type unconfirmedBlock struct {
	index uint64	//块的number.
	hash  common.Hash	//块的hash.
}

// unconfirmedBlocks implements a data structure to maintain locally mined blocks
// have have not yet reached enough maturity to guarantee chain inclusion. It is
// used by the miner to provide logs to the user when a previously mined block
// has a high enough guarantee to not be reorged out of te canonical chain.
//unconfirmedBlocks存储的是本地被挖出但是还没有达到足够成熟度来保证被主链包含的块.
//这个结构被miner用来在一个本地挖掘出的块有足够的保证被主链包含而不会再次被踢出主链的时候提供给用户日志信息(只是提供给用户看的日志信息,对代码逻辑没有影响)
type unconfirmedBlocks struct {
	chain  headerRetriever // Blockchain to verify canonical status through
	depth  uint            // Depth after which to discard previous blocks
	blocks *ring.Ring      // Block infos to allow canonical chain cross checks
	lock   sync.RWMutex    // Protects the fields from concurrent access
}

// newUnconfirmedBlocks returns new data structure to track currently unconfirmed blocks.
//未被确认块的集合。
func newUnconfirmedBlocks(chain headerRetriever, depth uint) *unconfirmedBlocks {
	return &unconfirmedBlocks{
		chain: chain,
		depth: depth,
	}
}

// Insert adds a new block to the set of unconfirmed ones.
//向unconfirmedBlocks集合中插入一个新的unconfirmed block.
//index:块的number; hash:块的hash.
func (set *unconfirmedBlocks) Insert(index uint64, hash common.Hash) {
	// If a new block was mined locally, shift out any old enough blocks
	//本地挖出了新块,那么在unconfirmed块集合中移除最老的块.
	set.Shift(index)

	// Create the new item as its own ring
	item := ring.New(1)	//创建一个元素的环形列表.
	item.Value = &unconfirmedBlock{		//向这个新的环形列表中插入这个新挖掘出的unconfirmed块.
		index: index,
		hash:  hash,
	}
	// Set as the initial ring or append to the end
	set.lock.Lock()
	defer set.lock.Unlock()

	//把块插入到ring中.
	if set.blocks == nil {
		set.blocks = item
	} else {
		set.blocks.Move(-1).Link(item)
	}
	// Display a log for the user to notify of a new mined block unconfirmed
	//此句日志的意思是一个新的块被挖掘出来了,但是还处于unconfirmed状态.
	log.Info("🔨 mined potential block", "number", index, "hash", hash)
}

// Shift drops all unconfirmed blocks from the set which exceed the unconfirmed sets depth
// allowance, checking them against the canonical chain for inclusion or staleness
// report.
//此函数完全操作的是set集合本身,对其外部数据结构没有影响.
func (set *unconfirmedBlocks) Shift(height uint64) {
	set.lock.Lock()
	defer set.lock.Unlock()

	for set.blocks != nil {
		// Retrieve the next unconfirmed block and abort if too fresh
		next := set.blocks.Value.(*unconfirmedBlock)
		if next.index+uint64(set.depth) > height {		//如果next.index + depth > height 说明ring有空间能够容纳这个新区块,也就没有必要再移除区块了,所以退出了循环.否则就走地下的逻辑移除区块.
			break
		}
		// Block seems to exceed depth allowance, check for canonical status
		header := set.chain.GetHeaderByNumber(next.index) 	//next.index即block的number，此函数通过number来确认block是否已经在主链中了。

		//此switch的作用只是打印了一个日志.
		switch {
		case header == nil:		//在主链中没有查到这个块。
			log.Warn("Failed to retrieve header of mined block", "number", next.index, "hash", next.hash)
		case header.Hash() == next.hash:	//块已经进入到了主链中。
			log.Info("🔗 block reached canonical chain", "number", next.index, "hash", next.hash)
		default:	//能进入到default说明在主链中查到了但是hash和这个next.index对应的块不一致,这就说明主链打包进了别的链产生的number是next.index的块,那么当前本节点挖掘出来的next也就只能是一个分叉了.
			log.Info("⑂ block  became a side fork", "number", next.index, "hash", next.hash)
		}
		// Drop the block out of the ring
		//从ring中移除最老的元素.
		if set.blocks.Value == set.blocks.Next().Value {
			set.blocks = nil
		} else {
			set.blocks = set.blocks.Move(-1)
			set.blocks.Unlink(1)
			set.blocks = set.blocks.Move(1)
		}
	}
}
