// Copyright 2015 The go-ethereum Authors
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

package eth

import (
	"math/rand"
	"sync/atomic"
	"time"

	"github.com/wanchain/go-wanchain/common"
	"github.com/wanchain/go-wanchain/core/types"
	"github.com/wanchain/go-wanchain/eth/downloader"
	"github.com/wanchain/go-wanchain/log"
	"github.com/wanchain/go-wanchain/p2p/discover"
)

const (
	forceSyncCycle      = 10 * time.Second // Time interval to force syncs, even if few peers are available	强制同步周期，即使几乎没有peer可用也会执行同步过程。
	minDesiredPeerCount = 5                // Amount of peers desired to start syncing

	// This is the target size for the packs of transactions sent by txsyncLoop.
	// A pack can get larger than this if a single transactions exceeds this size.
	txsyncPackSize = 100 * 1024
)

type txsync struct {
	p   *peer
	txs []*types.Transaction
}

// syncTransactions starts sending all currently pending transactions to the given peer.
func (pm *ProtocolManager) syncTransactions(p *peer) {
	var txs types.Transactions
	pending, _ := pm.txpool.Pending()
	for _, batch := range pending {
		txs = append(txs, batch...)
	}
	if len(txs) == 0 {
		return
	}
	select {
	case pm.txsyncCh <- &txsync{p, txs}:
	case <-pm.quitSync:
	}
}

// txsyncLoop takes care of the initial transaction sync for each new
// connection. When a new peer appears, we relay all currently pending
// transactions. In order to minimise egress bandwidth usage, we send
// the transactions in small packs to one peer at a time.
func (pm *ProtocolManager) txsyncLoop() {
	var (
		pending = make(map[discover.NodeID]*txsync)
		sending = false               // whether a send is active
		pack    = new(txsync)         // the pack that is being sent
		done    = make(chan error, 1) // result of the send
	)

	// send starts a sending a pack of transactions from the sync.
	send := func(s *txsync) {
		// Fill pack with transactions up to the target size.
		size := common.StorageSize(0)
		pack.p = s.p
		pack.txs = pack.txs[:0]
		for i := 0; i < len(s.txs) && size < txsyncPackSize; i++ {
			pack.txs = append(pack.txs, s.txs[i])
			size += s.txs[i].Size()
		}
		// Remove the transactions that will be sent.
		s.txs = s.txs[:copy(s.txs, s.txs[len(pack.txs):])]
		if len(s.txs) == 0 {
			delete(pending, s.p.ID())
		}
		// Send the pack in the background.
		s.p.Log().Trace("Sending batch of transactions", "count", len(pack.txs), "bytes", size)
		sending = true
		go func() { done <- pack.p.SendTransactions(pack.txs) }()
	}

	// pick chooses the next pending sync.
	pick := func() *txsync {
		if len(pending) == 0 {
			return nil
		}
		n := rand.Intn(len(pending)) + 1
		for _, s := range pending {
			if n--; n == 0 {
				return s
			}
		}
		return nil
	}

	for {
		select {
		case s := <-pm.txsyncCh:
			pending[s.p.ID()] = s
			if !sending {
				send(s)
			}
		case err := <-done:
			sending = false
			// Stop tracking peers that cause send failures.
			if err != nil {
				pack.p.Log().Debug("Transaction send failed", "err", err)
				delete(pending, pack.p.ID())
			}
			// Schedule the next send.
			if s := pick(); s != nil {
				send(s)
			}
		case <-pm.quitSync:
			return
		}
	}
}

// syncer is responsible for periodically synchronising with the network, both
// downloading hashes and blocks as well as handling the announcement handler.
// syncer用于周期性的或者当有新节点加入的时候与peer进行同步.
func (pm *ProtocolManager) syncer() {
	// Start and ensure cleanup of sync mechanisms
	pm.fetcher.Start()	//启动fetcher.
	defer pm.fetcher.Stop()		//函数结束时停止fetcher。
	defer pm.downloader.Terminate()		//函数结束时停止downloader。

	// Wait for different events to fire synchronisation operations
	forceSync := time.NewTicker(forceSyncCycle)	//forceSync是一个强制同步的定时器，默认时间为10秒进行一次强制同步。
	defer forceSync.Stop()	//在函数退出的时候销毁forceSync定时器。

	for {	//无线循环处理事件。
		select {
		case <-pm.newPeerCh:	//新节点被添加进来会触发同步.
			// Make sure we have peers to select from, then sync
			if pm.peers.Len() < minDesiredPeerCount {	//如果peer个数小于5的话那么即使有新节点添加进来也不需要进行链同步.
				break
			}
			go pm.synchronise(pm.peers.BestPeer())

		case <-forceSync.C:		//响应forceSync定时器到期事件。
			// Force a sync even if not enough peers are present
			//启动一个协程进行强制同步。
			go pm.synchronise(pm.peers.BestPeer())

		case <-pm.noMorePeers:
			return
		}
	}
}

// synchronise tries to sync up our local block chain with a remote peer.
//本地链与peer同步。(与具有最大td的peer进行同步.)
func (pm *ProtocolManager) synchronise(peer *peer) {
	// Short circuit if no peers are available
	if peer == nil {	//没有Peer可用则返回.
		return
	}

	//获得当前块的指针及td,然后获得peer的当前块指针及td. 如果当前节点的td大于等于peer的td,那么不需要同步,直接退出.(此时本地节点是最新的.)
	// Make sure the peer's TD is higher than our own
	currentBlock := pm.blockchain.CurrentBlock()	//获得当前块(不需要查数据库就可以获得链当前头.)
	td := pm.blockchain.GetTd(currentBlock.Hash(), currentBlock.NumberU64())	//从当前块中获得total difficulty.(如果缓存中没有td则需要从数据库中获取td.)

	pHead, pTd := peer.Head()	//获得peer的head块指针及total difficulty.
	if pTd.Cmp(td) <= 0 {	//如果peer的td比本节点的td还小,说明本节点的链是最新的,不需要从peer同步.
		return
	}

	//首先选择同步模式,是fullSync mode还是fastSync mode.(一般情况下,如果本地链高度不为0,都使用fullSync模式.)
	// Otherwise try to sync with the downloader
	mode := downloader.FullSync	//默认同步模式设置为fullSync.
	if atomic.LoadUint32(&pm.fastSync) == 1 {
		// Fast sync was explicitly requested, and explicitly granted
		mode = downloader.FastSync		//如果允许fastSync,那么mode设置为fastSync.
	} else if currentBlock.NumberU64() == 0 && pm.blockchain.CurrentFastBlock().NumberU64() > 0 {		//如果当前本地链只有创世块并且fast block缓存的块号大于0,那么设置同步模式为fastSync模式.
		// The database seems empty as the current block is the genesis. Yet the fast
		// block is ahead, so fast sync was enabled for this node at a certain point.
		// The only scenario where this can happen is if the user manually (or via a
		// bad block) rolled back a fast sync node below the sync point. In this case
		// however it's safe to reenable fast sync.
		atomic.StoreUint32(&pm.fastSync, 1)
		mode = downloader.FastSync
	}
	// Run the sync cycle, and disable fast sync if we've went past the pivot block
	//调用downloader对象开始同步过程.
	err := pm.downloader.Synchronise(peer.id, pHead, pTd, mode)	//调用downloader对象开始同步.

	if atomic.LoadUint32(&pm.fastSync) == 1 {
		// Disable fast sync if we indeed have something in our chain
		if pm.blockchain.CurrentBlock().NumberU64() > 0 {
			log.Info("Fast sync complete, auto disabling")
			atomic.StoreUint32(&pm.fastSync, 0)
		}
	}
	if err != nil {
		return
	}
	atomic.StoreUint32(&pm.acceptTxs, 1) // Mark initial sync done
	if head := pm.blockchain.CurrentBlock(); head.NumberU64() > 0 {
		// We've completed a sync cycle, notify all peers of new state. This path is
		// essential in star-topology networks where a gateway node needs to notify
		// all its out-of-date peers of the availability of a new block. This failure
		// scenario will most often crop up in private and hackathon networks with
		// degenerate connectivity, but it should be healthy for the mainnet too to
		// more reliably update peers or the local TD state.
		go pm.BroadcastBlock(head, false)
	}
}
