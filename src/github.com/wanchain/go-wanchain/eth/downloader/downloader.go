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

// Package downloader contains the manual full chain synchronisation.
package downloader

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	metrics "github.com/rcrowley/go-metrics"
	ethereum "github.com/wanchain/go-wanchain"
	"github.com/wanchain/go-wanchain/common"
	"github.com/wanchain/go-wanchain/core/types"
	"github.com/wanchain/go-wanchain/ethdb"
	"github.com/wanchain/go-wanchain/event"
	"github.com/wanchain/go-wanchain/log"
	"github.com/wanchain/go-wanchain/params"
)

var (
	MaxHashFetch    = 512 // Amount of hashes to be fetched per retrieval request
	MaxBlockFetch   = 128 // Amount of blocks to be fetched per retrieval request
	MaxHeaderFetch  = 192 // Amount of block headers to be fetched per retrieval request	每次请求获得的最多的header数量.
	MaxSkeletonSize = 128 // Number of header fetches to need for a skeleton assembly
	MaxBodyFetch    = 128 // Amount of block bodies to be fetched per retrieval request	//每个获取请求中包含的最大的body数量.
	MaxReceiptFetch = 256 // Amount of transaction receipts to allow fetching per request
	MaxStateFetch   = 384 // Amount of node state values to allow fetching per request

	MaxForkAncestry  = 3 * params.EpochDuration // Maximum chain reorganisation
	rttMinEstimate   = 2 * time.Second          // Minimum round-trip time to target for download requests	下载请求最小周期.2s.
	rttMaxEstimate   = 20 * time.Second         // Maximum rount-trip time to target for download requests	下载请求最大周期.20s.
	rttMinConfidence = 0.1                      // Worse confidence factor in our estimated RTT value
	ttlScaling       = 3                        // Constant scaling factor for RTT -> TTL conversion	rtt转换ttl的常量因子.
	ttlLimit         = time.Minute              // Maximum TTL allowance to prevent reaching crazy timeouts	//最大的ttl是1分钟.

	qosTuningPeers   = 5    // Number of peers to tune based on (best peers)	基于多少个节点来调制rtt值.
	qosConfidenceCap = 10   // Number of peers above which not to modify RTT confidence
	qosTuningImpact  = 0.25 // Impact that a new tuning target has on the previous value	新的调制目标对旧值的影响.

	maxQueuedHeaders  = 32 * 1024 // [eth/62] Maximum number of headers to queue for import (DOS protection)	//在queue中等待导入的最多的块数量。
	maxHeadersProcess = 2048      // Number of header download results to import at once into the chain
	maxResultsProcess = 2048      // Number of content download results to import at once into the chain		//一次上鏈塊數的最大數.

	fsHeaderCheckFrequency = 100        // Verification frequency of the downloaded headers during fast sync
	fsHeaderSafetyNet      = 2048       // Number of headers to discard in case a chain violation is detected
	fsHeaderForceVerify    = 24         // Number of headers to verify before and after the pivot to accept it
	fsPivotInterval        = 256        // Number of headers out of which to randomize the pivot point
	fsMinFullBlocks        = 64         // Number of blocks to retrieve fully even in fast sync	//即使在fastSync模式也是需要部分块进行fullSync同步的.此值是需要进行fullSync的块的最少数量.
	fsCriticalTrials       = uint32(32) // Number of times to retry in the cricical section before bailing	//fastSync同步失败最大尝试次数,超过此次数后转换为fullSync.
)

var (
	errBusy                    = errors.New("busy")
	errUnknownPeer             = errors.New("peer is unknown or unhealthy")
	errBadPeer                 = errors.New("action from bad peer ignored")
	errStallingPeer            = errors.New("peer is stalling")
	errNoPeers                 = errors.New("no peers to keep download active")
	errTimeout                 = errors.New("timeout")
	errEmptyHeaderSet          = errors.New("empty header set by peer")
	errPeersUnavailable        = errors.New("no peers available or all tried for download")
	errInvalidAncestor         = errors.New("retrieved ancestor is invalid")
	errInvalidChain            = errors.New("retrieved hash chain is invalid")
	errInvalidBlock            = errors.New("retrieved block is invalid")
	errInvalidBody             = errors.New("retrieved block body is invalid")
	errInvalidReceipt          = errors.New("retrieved receipt is invalid")
	errCancelBlockFetch        = errors.New("block download canceled (requested)")
	errCancelHeaderFetch       = errors.New("block header download canceled (requested)")
	errCancelBodyFetch         = errors.New("block body download canceled (requested)")
	errCancelReceiptFetch      = errors.New("receipt download canceled (requested)")
	errCancelStateFetch        = errors.New("state data download canceled (requested)")
	errCancelHeaderProcessing  = errors.New("header processing canceled (requested)")
	errCancelContentProcessing = errors.New("content processing canceled (requested)")
	errNoSyncActive            = errors.New("no sync active")
	errTooOld                  = errors.New("peer doesn't speak recent enough protocol version (need version >= 62)")
)

type Downloader struct {
	mode SyncMode       // Synchronisation mode defining the strategy used (per sync cycle)	//同步模式,定义了同步策略,lightSync, FastSync或者FullSync.
	mux  *event.TypeMux // Event multiplexer to announce sync operation events

	queue   *queue   // Scheduler for selecting the hashes to download			//能够下载的块hash列表.
	peers   *peerSet // Set of active peers from which download can proceed	//peers维护着一个与当前节点连接的所有peer的列表信息.
	stateDB ethdb.Database

	fsPivotLock  *types.Header // Pivot header on critical section entry (cannot change between retries)	//fastSync的中轴,小于中轴的块用fastSync模式,大于中轴的块用fullSync模式.
	fsPivotFails uint32        // Number of subsequent fast sync failures in the critical section	//fastSync失败次数.

	rttEstimate   uint64 // Round trip time to target for download requests	下载请求的rtt估值.
	rttConfidence uint64 // Confidence in the estimated RTT (unit: millionths to allow atomic ops)	//rtt估值的可信度.

	// Statistics
	syncStatsChainOrigin uint64 // Origin block number where syncing started at		同步开始的时候所处的块编号.
	syncStatsChainHeight uint64 // Highest block number known when syncing started		已知的最大块编号,即需要同步到的块编号.
	syncStatsState       stateSyncStats		//state同步状态,只用于在用户日志中展示.
	syncStatsLock        sync.RWMutex // Lock protecting the sync stats fields		同步状态锁.

	lightchain LightChain
	blockchain BlockChain

	// Callbacks
	dropPeer peerDropFn // Drops a peer for misbehaving

	// Status
	synchroniseMock func(id string, hash common.Hash) error // Replacement for synchronise during testing		//用于测试的函数.
	synchronising   int32	//是否正在执行同步的标志,保证同一时间只有一个同步过程在执行.
	notified        int32	//给用户发出通知(即在日志中打印.)

	// Channels	几个channel用来接收外部事件.
	headerCh      chan dataPack        // [eth/62] Channel receiving inbound block headers	从peer收到的header,存储于此.
	bodyCh        chan dataPack        // [eth/62] Channel receiving inbound block bodies		收到body,存储于此.
	receiptCh     chan dataPack        // [eth/63] Channel receiving inbound receipts			收到receipt,存储于此.
	bodyWakeCh    chan bool            // [eth/62] Channel to signal the block body fetcher of new tasks	下载body的任务可以被唤醒了。
	receiptWakeCh chan bool            // [eth/63] Channel to signal the receipt fetcher of new tasks		下载receipt的任务可以被唤醒了。
	headerProcCh  chan []*types.Header // [eth/62] Channel to feed the header processor new tasks		从对端的dataPack中解析出来的headers存储于此。

	// for stateFetcher
	stateSyncStart chan *stateSync		//状态同步开始消息通道.
	trackStateReq  chan *stateReq
	stateCh        chan dataPack // [eth/63] Channel receiving inbound node state data	节点状态数据通道.

	// Cancellation and termination
	cancelPeer string        // Identifier of the peer currently being used as the master (cancel on drop)	//要取消的与哪个peer之间的同步.
	cancelCh   chan struct{} // Channel to cancel mid-flight syncs	//取消消息通道,用于取消正在进行的同步操作.
	cancelLock sync.RWMutex  // Lock to protect the cancel channel and peer in delivers	//取消操作锁.

	quitCh   chan struct{} // Quit channel to signal termination	退出信号通道.
	quitLock sync.RWMutex  // Lock to prevent double closes

	// Testing hooks	以下4个为测试hook点.
	syncInitHook     func(uint64, uint64)  // Method to call upon initiating a new sync run
	bodyFetchHook    func([]*types.Header) // Method to call upon starting a block body fetch
	receiptFetchHook func([]*types.Header) // Method to call upon starting a receipt fetch
	chainInsertHook  func([]*fetchResult)  // Method to call upon inserting a chain of blocks (possibly in multiple invocations)
}

// LightChain encapsulates functions required to synchronise a light chain.
type LightChain interface {
	// HasHeader verifies a header's presence in the local chain.
	HasHeader(h common.Hash, number uint64) bool

	// GetHeaderByHash retrieves a header from the local chain.
	GetHeaderByHash(common.Hash) *types.Header

	// CurrentHeader retrieves the head header from the local chain.
	CurrentHeader() *types.Header

	// GetTdByHash returns the total difficulty of a local block.
	GetTdByHash(common.Hash) *big.Int

	// InsertHeaderChain inserts a batch of headers into the local chain.
	InsertHeaderChain([]*types.Header, int) (int, error)

	// Rollback removes a few recently added elements from the local chain.
	Rollback([]common.Hash)
}

// BlockChain encapsulates functions required to sync a (full or fast) blockchain.
type BlockChain interface {
	LightChain

	// HasBlockAndState verifies block and associated states' presence in the local chain.
	HasBlockAndState(common.Hash) bool

	// GetBlockByHash retrieves a block from the local chain.
	GetBlockByHash(common.Hash) *types.Block

	// CurrentBlock retrieves the head block from the local chain.
	CurrentBlock() *types.Block

	// CurrentFastBlock retrieves the head fast block from the local chain.
	CurrentFastBlock() *types.Block

	// FastSyncCommitHead directly commits the head block to a certain entity.
	FastSyncCommitHead(common.Hash) error

	// InsertChain inserts a batch of blocks into the local chain.
	InsertChain(types.Blocks) (int, error)

	// InsertReceiptChain inserts a batch of receipts into the local chain.
	InsertReceiptChain(types.Blocks, []types.Receipts) (int, error)	//插入receipts.
}

// New creates a new downloader to fetch hashes and blocks from remote peers.
// 创建downloader用来从远端peer获取hash和块.
func New(mode SyncMode, stateDb ethdb.Database, mux *event.TypeMux, chain BlockChain, lightchain LightChain, dropPeer peerDropFn) *Downloader {
	if lightchain == nil {
		lightchain = chain
	}
	//创建downloader对象.
	dl := &Downloader{
		mode:           mode,
		stateDB:        stateDb,
		mux:            mux,
		queue:          newQueue(),
		peers:          newPeerSet(),
		rttEstimate:    uint64(rttMaxEstimate),		//开始创建的时候,rtt估值为最大值,20秒,后期会周期性更新.
		rttConfidence:  uint64(1000000),			//开始的时候估值可信度设置为最大值1000000,后期会周期性更新.
		blockchain:     chain,
		lightchain:     lightchain,
		dropPeer:       dropPeer,
		headerCh:       make(chan dataPack, 1),
		bodyCh:         make(chan dataPack, 1),
		receiptCh:      make(chan dataPack, 1),
		bodyWakeCh:     make(chan bool, 1),
		receiptWakeCh:  make(chan bool, 1),
		headerProcCh:   make(chan []*types.Header, 1),
		quitCh:         make(chan struct{}),
		stateCh:        make(chan dataPack),
		stateSyncStart: make(chan *stateSync),
		trackStateReq:  make(chan *stateReq),
	}
	go dl.qosTuner()	//启动一个协程专门用来根据peer延迟时间周期性更新qos参数,rttEstimate和rttConfidence.
	go dl.stateFetcher()
	return dl
}

// Progress retrieves the synchronisation boundaries, specifically the origin
// block where synchronisation started at (may have failed/suspended); the block
// or header sync is currently at; and the latest known block which the sync targets.
//
// In addition, during the state download phase of fast synchronisation the number
// of processed and the total number of known states are also returned. Otherwise
// these are zero.
func (d *Downloader) Progress() ethereum.SyncProgress {
	// Lock the current stats and return the progress
	d.syncStatsLock.RLock()
	defer d.syncStatsLock.RUnlock()

	current := uint64(0)
	switch d.mode {
	case FullSync:
		current = d.blockchain.CurrentBlock().NumberU64()
	case FastSync:
		current = d.blockchain.CurrentFastBlock().NumberU64()
	case LightSync:
		current = d.lightchain.CurrentHeader().Number.Uint64()
	}
	return ethereum.SyncProgress{
		StartingBlock: d.syncStatsChainOrigin,
		CurrentBlock:  current,
		HighestBlock:  d.syncStatsChainHeight,
		PulledStates:  d.syncStatsState.processed,
		KnownStates:   d.syncStatsState.processed + d.syncStatsState.pending,
	}
}

// Synchronising returns whether the downloader is currently retrieving blocks.
func (d *Downloader) Synchronising() bool {
	return atomic.LoadInt32(&d.synchronising) > 0
}

// RegisterPeer injects a new download peer into the set of block source to be
// used for fetching hashes and blocks from.
func (d *Downloader) RegisterPeer(id string, version int, peer Peer) error {

	logger := log.New("peer", id)
	logger.Trace("Registering sync peer")
	if err := d.peers.Register(newPeerConnection(id, version, peer, logger)); err != nil {
		logger.Error("Failed to register sync peer", "err", err)
		return err
	}
	d.qosReduceConfidence()

	return nil
}

// RegisterLightPeer injects a light client peer, wrapping it so it appears as a regular peer.
func (d *Downloader) RegisterLightPeer(id string, version int, peer LightPeer) error {
	return d.RegisterPeer(id, version, &lightPeerWrapper{peer})
}

// UnregisterPeer remove a peer from the known list, preventing any action from
// the specified peer. An effort is also made to return any pending fetches into
// the queue.
func (d *Downloader) UnregisterPeer(id string) error {
	// Unregister the peer from the active peer set and revoke any fetch tasks
	logger := log.New("peer", id)
	logger.Trace("Unregistering sync peer")
	if err := d.peers.Unregister(id); err != nil {
		logger.Error("Failed to unregister sync peer", "err", err)
		return err
	}
	d.queue.Revoke(id)

	// If this peer was the master peer, abort sync immediately
	d.cancelLock.RLock()
	master := id == d.cancelPeer
	d.cancelLock.RUnlock()

	if master {
		d.Cancel()
	}
	return nil
}

// Synchronise tries to sync up our local block chain with a remote peer, both
// adding various sanity checks as well as wrapping it with various log entries.
//开始同步过程.
//id: peer id.
//head: peer head.
//td: peer total difficulty.
//mode: 同步模式.
//(Synchronize内部调用了synchronize函数,这种方法比较好,可以尝试.)
func (d *Downloader) Synchronise(id string, head common.Hash, td *big.Int, mode SyncMode) error {
	err := d.synchronise(id, head, td, mode)	//传递进来的参数是peer id, peer head, peer total difficulty及同步模式.
	switch err {
	case nil:
	case errBusy:

	case errTimeout, errBadPeer, errStallingPeer,
		errEmptyHeaderSet, errPeersUnavailable, errTooOld,
		errInvalidAncestor, errInvalidChain:
		log.Warn("Synchronisation failed, dropping peer", "peer", id, "err", err)
		d.dropPeer(id)

	default:
		log.Warn("Synchronisation failed, retrying", "err", err)
	}
	return err
}

// synchronise will select the peer and use it for synchronising. If an empty string is given
// it will use the best peer possible and synchronize if it's TD is higher than our own. If any of the
// checks fail an error will be returned. This method is synchronous
//synchronize是一个同步调用的方法.用来进行本地节点与peer之间的同步.
//id: peer id.
//hash: peer head hash.
//td: total difficulty.
//mode: sync mode.
func (d *Downloader) synchronise(id string, hash common.Hash, td *big.Int, mode SyncMode) error {
	// Mock out the synchronisation if testing
	if d.synchroniseMock != nil {	//如果此函数指针不为空(及在测试场景下),那么执行这个mock函数,这个mock只是在测试的时候会被使用,在实际运行中不会被使用到.
		return d.synchroniseMock(id, hash)
	}
	// Make sure only one goroutine is ever allowed past this point at once
	if !atomic.CompareAndSwapInt32(&d.synchronising, 0, 1) {	//保证同一时间只有一个synchronize在执行.此原子操作首先比较synchronising的值,如果为0,则替换成1,返回true; 如果不为0,返回false.
		//有两种情况会进入到这个位置,一个是同步定时器到期会触发,另外一个新节点加入且总节点数不小于5.此处的锁保证了同时只有一个同步过程在执行.
		return errBusy
	}
	defer atomic.StoreInt32(&d.synchronising, 0)	//在函数结束的时候设置synchronising标志为0. (为0表示没有正在进行同步,为1表示正在进行同步.)

	// Post a user notification of the sync (only once per session)
	if atomic.CompareAndSwapInt32(&d.notified, 0, 1) {
		log.Info("Block synchronisation started")
	}
	// Reset the queue, peer set and wake channels to clean any internal leftover state
	d.queue.Reset()	//重置queue队列,queue里存储着能够下载blocks的hash列表.
	d.peers.Reset()	//重置peers队列,peers里存储着能够与之进行同步的peer列表.(把peers中的每个peer进行reset,而不是把peer从peers中删除.)

	//清空队列bodyWakeCh和receiptWakeCh.
	for _, ch := range []chan bool{d.bodyWakeCh, d.receiptWakeCh} {
		select {
		case <-ch:
		default:
		}
	}
	//在执行同步之前,首先查看队列headerCh,bodyCh,receiptCh中是否还有未处理的数据,如果有那么全部清空,清空这三个通道之后,再进行后续的处理.
	for _, ch := range []chan dataPack{d.headerCh, d.bodyCh, d.receiptCh} {
		for empty := false; !empty; {
			select {
			case <-ch:
			default:
				empty = true
			}
		}
	}
	//清空headerProcCh中的现有数据.
	for empty := false; !empty; {
		select {
		case <-d.headerProcCh:
		default:
			empty = true
		}
	}
	// Create cancel channel for aborting mid-flight and mark the master peer
	//创建取消消息通道,并标记此通道属于与哪个peer的连接.
	d.cancelLock.Lock()
	d.cancelCh = make(chan struct{})
	d.cancelPeer = id
	d.cancelLock.Unlock()
	//同步操作结束之后销毁取消消息通道.
	defer d.Cancel() // No matter what, we can't leave the cancel channel open

	// Set the requested sync mode, unless it's forbidden
	//设置同步模式.
	d.mode = mode
	if d.mode == FastSync && atomic.LoadUint32(&d.fsPivotFails) >= fsCriticalTrials {	//當fastsync模式的錯誤計數超過了32之後,強制採用fullSync方式進行同步.
		d.mode = FullSync
	}
	// Retrieve the origin peer and initiate the downloading process
	p := d.peers.Peer(id)	//从peer id获得peer对象.
	if p == nil {
		return errUnknownPeer
	}

	return d.syncWithPeer(p, hash, td)		//与peer进行同步, p:peer对象;hash:需要同步的块hash;td:total difficulty.
}

// syncWithPeer starts a block synchronization based on the hash chain from the
// specified peer and head hash.
//与peer同步,hash:block hash, td:total difficulty.
//p: peer对象指针;
//hash: peer的链头块hash.
//td: total difficulty.
func (d *Downloader) syncWithPeer(p *peerConnection, hash common.Hash, td *big.Int) (err error) {
	d.mux.Post(StartEvent{})	//发出StartEvent事件,此事件一共有两个对象订阅,一个是miner,一个是downloader对象本身, miner在收到这个事件的时候判断其是否正在挖矿,如果正在挖矿则停止挖矿.
	defer func() {	//在函数退出的时候调用此函数.
		// reset on error 同步完成之后判断同步是否成功,如果成功发出DoneEvent事件,如果失败发出FailedEvent事件.
		if err != nil {
			d.mux.Post(FailedEvent{err})
		} else {
			d.mux.Post(DoneEvent{})
		}
	}()
	if p.version < 62 {		//peer版本太低,返回错误.
		return errTooOld
	}

	log.Debug("Synchronising with the network", "peer", p.id, "eth", p.version, "head", hash, "td", td, "mode", d.mode)
	defer func(start time.Time) {	//函数结束的时候执行此函数.
		log.Debug("Synchronisation terminated", "elapsed", time.Since(start))
	}(time.Now())	//time.Now()在此处就已经被计算了,而不是在该函数执行的时候被计算的.

	//查找需要同步的边界,从哪个块开始及到哪个块结束.
	// Look up the sync boundaries: the common ancestor and the target block
	latest, err := d.fetchHeight(p)		//获得peer的最新块的指针.
	if err != nil {
		return err
	}
	height := latest.Number.Uint64()	//获得peer的块高度.

	//找到需要同步的开始块.
	origin, err := d.findAncestor(p, height)	//找到本地链与peer链的公共祖先,即需要从哪个块开始同步.
	if err != nil {
		return err
	}
	//加锁并更新从开始块编号及结束块编号.
	d.syncStatsLock.Lock()	//给同步状态加锁.
	if d.syncStatsChainHeight <= origin || d.syncStatsChainOrigin > origin {
		d.syncStatsChainOrigin = origin
	}
	d.syncStatsChainHeight = height
	d.syncStatsLock.Unlock()	//给同步状态解锁.

	// Initiate the sync using a concurrent header and content retrieval algorithm
	//使用并发的header及content获取算法来初始化同步机制.
	//根据同步模式计算中轴点,小于中轴点的块用fast模式同步,大于中轴点的块用full模式同步.
	pivot := uint64(0)	//pivot:中心点.
	switch d.mode {	//链高度不为0时候触发的同步其模式一定为FullSync模式(即使设置了FastSync模式也会强制转换为FullSync模式.)
	case LightSync:	//轻节点模式.
		pivot = height	//中心点等于peer的最新块的高度.	(lightSync:同步的时候所有块都用fast模式进行同步; fastSync:pivot左侧块用fast方式同步,右侧块用full方式同步.)
	case FastSync:		//fastSync模式.
		// Calculate the new fast/slow sync pivot point	计算快速/慢速同步时候的中心点.小于中心点的块用fastSync,大于中心点的块用fullSync模式进行同步.
		if d.fsPivotLock == nil {	//fs中轴锁非空.
			pivotOffset, err := rand.Int(rand.Reader, big.NewInt(int64(fsPivotInterval)))	//计算fs中轴偏移.(返回1-256之間的隨機數.)
			if err != nil {
				panic(fmt.Sprintf("Failed to access crypto random source: %v", err))
			}
			//计算中轴.
			if height > uint64(fsMinFullBlocks)+pivotOffset.Uint64() {
				pivot = height - uint64(fsMinFullBlocks) - pivotOffset.Uint64()
			}
		} else {	//中轴锁不为空则使用当前的中轴锁中header的值.
			// Pivot point locked in, use this and do not pick a new one!
			pivot = d.fsPivotLock.Number.Uint64()
		}
		// If the point is below the origin, move origin back to ensure state download
		if pivot < origin {
			if pivot > 0 {
				origin = pivot - 1
			} else {
				origin = 0
			}
		}
		log.Debug("Fast syncing until pivot block", "pivot", pivot)	//从此处打印可以看出来,小于pivot的块用fastsync模式,大于pivot的块用full sync模式.
	}

	d.queue.Prepare(origin+1, d.mode, pivot, latest)	//第一个还没有同步的块, 同步模式, 中轴点, 最新块的指针.
	if d.syncInitHook != nil {	//此为测试hook.
		d.syncInitHook(origin, height)
	}

	//同步动作数组.
	fetchers := []func() error{
		func() error { return d.fetchHeaders(p, origin+1) }, // Headers are always retrieved	同步的时候headers总是需要获取的. p: peer指针,origin+1: 从最早的一个尚未同步的块开始同步.
		func() error { return d.fetchBodies(origin + 1) },   // Bodies are retrieved during normal and fast sync	//bodies在normal和fast sync中都会被获取.
		func() error { return d.fetchReceipts(origin + 1) }, // Receipts are retrieved during fast sync	//在fastsync过程中会从多个peers同时获取receipts.
		func() error { return d.processHeaders(origin+1, td) },
	}
	//根据同步模式在动作数组中添加动作.
	if d.mode == FastSync {
		fetchers = append(fetchers, func() error { return d.processFastSyncContent(latest) })
	} else if d.mode == FullSync {
		fetchers = append(fetchers, d.processFullSyncContent)
	}

	//执行同步操作.
	err = d.spawnSync(fetchers)
	if err != nil && d.mode == FastSync && d.fsPivotLock != nil {
		// If sync failed in the critical section, bump the fail counter.
		atomic.AddUint32(&d.fsPivotFails, 1)
	}
	return err
}

// spawnSync runs d.process and all given fetcher functions to completion in
// separate goroutines, returning the first error that appears.
//执行同步动作进行链同步.
func (d *Downloader) spawnSync(fetchers []func() error) error {
	var wg sync.WaitGroup	//定义WaitGroup.
	errc := make(chan error, len(fetchers))	//错误消息通道.
	wg.Add(len(fetchers))	//设置WaitGroup需要等待的协程数量.
	//对每个动作都启动一个单独的协程进行处理.
	for _, fn := range fetchers {
		fn := fn
		go func() { defer wg.Done(); errc <- fn() }()
	}
	// Wait for the first error, then terminate the others.
	var err error
	for i := 0; i < len(fetchers); i++ {
		if i == len(fetchers)-1 {	//当所有fetchers都处理完成之后
			// Close the queue when all fetchers have exited.
			// This will cause the block processor to end when
			// it has processed the queue.
			d.queue.Close()	//关闭queue队列.
		}
		if err = <-errc; err != nil {	//如果有任何一个协程返回错误,那么直接退出此循环.
			break
		}
	}
	d.queue.Close()	//关闭queue队列.
	d.Cancel()	//
	wg.Wait()	//等待wg中的所有协程结束.
	return err
}

// Cancel cancels all of the operations and resets the queue. It returns true
// if the cancel operation was completed.
func (d *Downloader) Cancel() {
	// Close the current cancel channel
	d.cancelLock.Lock()
	if d.cancelCh != nil {
		select {
		case <-d.cancelCh:
			// Channel was already closed
		default:
			close(d.cancelCh)
		}
	}
	d.cancelLock.Unlock()
}

// Terminate interrupts the downloader, canceling all pending operations.
// The downloader cannot be reused after calling Terminate.
func (d *Downloader) Terminate() {
	// Close the termination channel (make sure double close is allowed)
	d.quitLock.Lock()
	select {
	case <-d.quitCh:
	default:
		close(d.quitCh)
	}
	d.quitLock.Unlock()

	// Cancel any pending download requests
	d.Cancel()
}

// fetchHeight retrieves the head header of the remote peer to aid in estimating
// the total time a pending synchronisation would take.
func (d *Downloader) fetchHeight(p *peerConnection) (*types.Header, error) {
	p.log.Debug("Retrieving remote chain height")

	// Request the advertised remote head block and wait for the response
	head, _ := p.peer.Head()
	go p.peer.RequestHeadersByHash(head, 1, 0, false)

	ttl := d.requestTTL()
	timeout := time.After(ttl)
	for {
		select {
		case <-d.cancelCh:
			return nil, errCancelBlockFetch

		case packet := <-d.headerCh:
			// Discard anything not from the origin peer
			if packet.PeerId() != p.id {
				log.Debug("Received headers from incorrect peer", "peer", packet.PeerId())
				break
			}
			// Make sure the peer actually gave something valid
			headers := packet.(*headerPack).headers
			if len(headers) != 1 {
				p.log.Debug("Multiple headers for single request", "headers", len(headers))
				return nil, errBadPeer
			}
			head := headers[0]
			p.log.Debug("Remote head header identified", "number", head.Number, "hash", head.Hash())
			return head, nil

		case <-timeout:
			p.log.Debug("Waiting for head header timed out", "elapsed", ttl)
			return nil, errTimeout

		case <-d.bodyCh:
		case <-d.receiptCh:
			// Out of bounds delivery, ignore
		}
	}
}

// findAncestor tries to locate the common ancestor link of the local chain and
// a remote peers blockchain. In the general case when our node was in sync and
// on the correct chain, checking the top N links should already get us a match.
// In the rare scenario when we ended up on a long reorganisation (i.e. none of
// the head links match), we do a binary search to find the common ancestor.
//找到本地链与Peer链的公共祖先块.(即从这个块之后链还没有同步.)
func (d *Downloader) findAncestor(p *peerConnection, height uint64) (uint64, error) {
	// Figure out the valid ancestor range to prevent rewrite attacks
	//确定有效的初始节点范围以避免rewrite攻击.
	floor, ceil := int64(-1), d.lightchain.CurrentHeader().Number.Uint64()	//ceil为当前链头编号.

	p.log.Debug("Looking for common ancestor", "local", ceil, "remote", height)
	if d.mode == FullSync {
		ceil = d.blockchain.CurrentBlock().NumberU64()
	} else if d.mode == FastSync {
		ceil = d.blockchain.CurrentFastBlock().NumberU64()
	}
	if ceil >= MaxForkAncestry {
		floor = int64(ceil - MaxForkAncestry)
	}
	// Request the topmost blocks to short circuit binary ancestor lookup
	head := ceil
	if head > height {
		head = height
	}
	from := int64(head) - int64(MaxHeaderFetch)
	if from < 0 {
		from = 0
	}
	// Span out with 15 block gaps into the future to catch bad head reports
	limit := 2 * MaxHeaderFetch / 16
	count := 1 + int((int64(ceil)-from)/16)
	if count > limit {
		count = limit
	}
	go p.peer.RequestHeadersByNumber(uint64(from), count, 15, false)

	// Wait for the remote response to the head fetch
	number, hash := uint64(0), common.Hash{}

	ttl := d.requestTTL()
	timeout := time.After(ttl)

	for finished := false; !finished; {
		select {
		case <-d.cancelCh:
			return 0, errCancelHeaderFetch

		case packet := <-d.headerCh:
			// Discard anything not from the origin peer
			if packet.PeerId() != p.id {
				log.Debug("Received headers from incorrect peer", "peer", packet.PeerId())
				break
			}
			// Make sure the peer actually gave something valid
			headers := packet.(*headerPack).headers
			if len(headers) == 0 {
				p.log.Warn("Empty head header set")
				return 0, errEmptyHeaderSet
			}
			// Make sure the peer's reply conforms to the request
			for i := 0; i < len(headers); i++ {
				if number := headers[i].Number.Int64(); number != from+int64(i)*16 {
					p.log.Warn("Head headers broke chain ordering", "index", i, "requested", from+int64(i)*16, "received", number)
					return 0, errInvalidChain
				}
			}
			// Check if a common ancestor was found
			finished = true
			for i := len(headers) - 1; i >= 0; i-- {
				// Skip any headers that underflow/overflow our requested set
				if headers[i].Number.Int64() < from || headers[i].Number.Uint64() > ceil {
					continue
				}
				// Otherwise check if we already know the header or not
				if (d.mode == FullSync && d.blockchain.HasBlockAndState(headers[i].Hash())) || (d.mode != FullSync && d.lightchain.HasHeader(headers[i].Hash(), headers[i].Number.Uint64())) {
					number, hash = headers[i].Number.Uint64(), headers[i].Hash()

					// If every header is known, even future ones, the peer straight out lied about its head
					if number > height && i == limit-1 {
						p.log.Warn("Lied about chain head", "reported", height, "found", number)
						return 0, errStallingPeer
					}
					break
				}
			}

		case <-timeout:
			p.log.Debug("Waiting for head header timed out", "elapsed", ttl)
			return 0, errTimeout

		case <-d.bodyCh:
		case <-d.receiptCh:
			// Out of bounds delivery, ignore
		}
	}
	// If the head fetch already found an ancestor, return
	if !common.EmptyHash(hash) {
		if int64(number) <= floor {
			p.log.Warn("Ancestor below allowance", "number", number, "hash", hash, "allowance", floor)
			return 0, errInvalidAncestor
		}
		p.log.Debug("Found common ancestor", "number", number, "hash", hash)
		return number, nil
	}
	// Ancestor not found, we need to binary search over our chain
	start, end := uint64(0), head
	if floor > 0 {
		start = uint64(floor)
	}
	for start+1 < end {
		// Split our chain interval in two, and request the hash to cross check
		check := (start + end) / 2

		ttl := d.requestTTL()
		timeout := time.After(ttl)

		go p.peer.RequestHeadersByNumber(uint64(check), 1, 0, false)

		// Wait until a reply arrives to this request
		for arrived := false; !arrived; {
			select {
			case <-d.cancelCh:
				return 0, errCancelHeaderFetch

			case packer := <-d.headerCh:
				// Discard anything not from the origin peer
				if packer.PeerId() != p.id {
					log.Debug("Received headers from incorrect peer", "peer", packer.PeerId())
					break
				}
				// Make sure the peer actually gave something valid
				headers := packer.(*headerPack).headers
				if len(headers) != 1 {
					p.log.Debug("Multiple headers for single request", "headers", len(headers))
					return 0, errBadPeer
				}
				arrived = true

				// Modify the search interval based on the response
				if (d.mode == FullSync && !d.blockchain.HasBlockAndState(headers[0].Hash())) || (d.mode != FullSync && !d.lightchain.HasHeader(headers[0].Hash(), headers[0].Number.Uint64())) {
					end = check
					break
				}
				header := d.lightchain.GetHeaderByHash(headers[0].Hash()) // Independent of sync mode, header surely exists
				if header.Number.Uint64() != check {
					p.log.Debug("Received non requested header", "number", header.Number, "hash", header.Hash(), "request", check)
					return 0, errBadPeer
				}
				start = check

			case <-timeout:
				p.log.Debug("Waiting for search header timed out", "elapsed", ttl)
				return 0, errTimeout

			case <-d.bodyCh:
			case <-d.receiptCh:
				// Out of bounds delivery, ignore
			}
		}
	}
	// Ensure valid ancestry and return
	if int64(start) <= floor {
		p.log.Warn("Ancestor below allowance", "number", start, "hash", hash, "allowance", floor)
		return 0, errInvalidAncestor
	}
	p.log.Debug("Found common ancestor", "number", start, "hash", hash)
	return start, nil
}

// fetchHeaders keeps retrieving headers concurrently from the number
// requested, until no more are returned, potentially throttling on the way. To
// facilitate concurrency but still protect against malicious nodes sending bad
// headers, we construct a header chain skeleton using the "origin" peer we are
// syncing with, and fill in the missing headers using anyone else. Headers from
// other peers are only accepted if they map cleanly to the skeleton. If no one
// can fill in the skeleton - not even the origin peer - it's assumed invalid and
// the origin is dropped.
// fetchHeaders用于同步headers.
// p: peer连接.
// from: 从哪一个块开始进行同步.
func (d *Downloader) fetchHeaders(p *peerConnection, from uint64) error {
	p.log.Debug("Directing header downloads", "origin", from)	//最早的没有同步的块的header叫做directing header.
	defer p.log.Debug("Header download terminated")	//程序结束的时候执行此函数.

	// Create a timeout timer, and the associated header fetcher
	skeleton := true            // Skeleton assembly phase or finishing up
	request := time.Now()       // time of the last skeleton fetch request

	//构造一个定时器.
	timeout := time.NewTimer(0) // timer to dump a non-responsive active peer
	<-timeout.C                 // timeout channel should be initially empty
	defer timeout.Stop()

	var ttl time.Duration

	//定义一个匿名函数,用于获取headers.
	getHeaders := func(from uint64) {
		request = time.Now()

		ttl = d.requestTTL()	//获得请求超时时间,ttl是根据以前的经验值计算出来的,会动态更新.
		timeout.Reset(ttl)		//设置超时定时器.

		//获取块头信息.
		if skeleton {	//如果采用框架形式获取块头则每间隔一定数量的块获取下一个块头.
			p.log.Trace("Fetching skeleton headers", "count", MaxHeaderFetch, "from", from)
			go p.peer.RequestHeadersByNumber(from+uint64(MaxHeaderFetch)-1, MaxSkeletonSize, MaxHeaderFetch-1, false)	//reverse为false代表正向查找.
		} else {		//如果不采用框架形式,那么连续获取块头.
			p.log.Trace("Fetching full headers", "count", MaxHeaderFetch, "from", from)
			go p.peer.RequestHeadersByNumber(from, MaxHeaderFetch, 0, false)	//reverse为false代表正向查找.
		}
	}
	// Start pulling the header chain skeleton until all is done
	getHeaders(from)	//获得header框架.

	for {
		select {
		case <-d.cancelCh:
			return errCancelHeaderFetch

		case packet := <-d.headerCh:	//处理peer传递过来的headers。
			// Make sure the active peer is giving us the skeleton headers
			if packet.PeerId() != p.id {
				log.Debug("Received skeleton from incorrect peer", "peer", packet.PeerId())
				break
			}
			headerReqTimer.UpdateSince(request)
			timeout.Stop()	//停止超时定时器。

			// If the skeleton's finished, pull any remaining head headers directly from the origin
			//如果peer没有返回headers,那么就不按照框架模式取header了,而是按照顺序取header.
			//(考虑一种情况,如果经常进行块同步,那么节点之间是不会超过MaxHeaderFetch个间隔的,这样的话在skeleton为true的时候返回的header就为空,因此再次发送取header的请求,此时按照顺序模式取header.)
			if packet.Items() == 0 && skeleton {
				skeleton = false
				getHeaders(from)
				continue
			}
			// If no more headers are inbound, notify the content fetchers and return
			if packet.Items() == 0 {
				p.log.Debug("No more headers available")
				select {
				case d.headerProcCh <- nil:
					return nil
				case <-d.cancelCh:
					return errCancelHeaderFetch
				}
			}
			headers := packet.(*headerPack).headers	//从包中获得传递过来的headers。

			// If we received a skeleton batch, resolve internals concurrently
			//如果按照并发模式取块并且取到了,那么要并发的获取框架块间隔之间的块.
			if skeleton {
				filled, proced, err := d.fillHeaderSkeleton(from, headers)	//from:开始块, headers:框架块列表.（即使使用框架获取，此函数结束之后所有的headers也已经都被补全了。）
				if err != nil {
					p.log.Debug("Skeleton chain invalid", "err", err)
					return errInvalidChain
				}
				headers = filled[proced:]
				from += uint64(proced)
			}
			// Insert all the new headers and fetch the next batch
			// headers都传递过来之后插入headers，然后执行下一次获取。
			if len(headers) > 0 {
				p.log.Trace("Scheduling new headers", "count", len(headers), "from", from)
				select {
				case d.headerProcCh <- headers:	//把从对端获得的headers存储于此，即使是用框架模式获取headers，到此处的时候所有的headers也都已经获得了。
				case <-d.cancelCh:
					return errCancelHeaderFetch
				}
				from += uint64(len(headers))
			}
			getHeaders(from)

		case <-timeout.C:
			// Header retrieval timed out, consider the peer bad and drop
			p.log.Debug("Header request timed out", "elapsed", ttl)
			headerTimeoutMeter.Mark(1)
			d.dropPeer(p.id)

			// Finish the sync gracefully instead of dumping the gathered data though
			for _, ch := range []chan bool{d.bodyWakeCh, d.receiptWakeCh} {
				select {
				case ch <- false:
				case <-d.cancelCh:
				}
			}
			select {
			case d.headerProcCh <- nil:
			case <-d.cancelCh:
			}
			return errBadPeer
		}
	}
}

// fillHeaderSkeleton concurrently retrieves headers from all our available peers
// and maps them to the provided skeleton header chain.
//
// Any partial results from the beginning of the skeleton is (if possible) forwarded
// immediately to the header processor to keep the rest of the pipeline full even
// in the case of header stalls.
//
// The method returs the entire filled skeleton and also the number of headers
// already forwarded for processing.
//从所有的可用节点并发获取header然后把这些header映射到skeleton header chain上.
func (d *Downloader) fillHeaderSkeleton(from uint64, skeleton []*types.Header) ([]*types.Header, int, error) {
	log.Debug("Filling up skeleton", "from", from)
	d.queue.ScheduleSkeleton(from, skeleton)	//把需要获取的headers索引添加到调度队列中。

	var (
		deliver = func(packet dataPack) (int, error) {
			pack := packet.(*headerPack)
			return d.queue.DeliverHeaders(pack.peerId, pack.headers, d.headerProcCh)
		}
		//expire函数，
		expire   = func() map[string]int { return d.queue.ExpireHeaders(d.requestTTL()) }
		throttle = func() bool { return false }

		//reserve函数。
		reserve  = func(p *peerConnection, count int) (*fetchRequest, bool, error) {
			return d.queue.ReserveHeaders(p, count), false, nil	//为制定peer构造fetchRequest，然后把请求添加到headerPendPool中此peer对应的key下。
		}
		//Fetch函数。
		fetch    = func(p *peerConnection, req *fetchRequest) error { return p.FetchHeaders(req.From, MaxHeaderFetch) }
		capacity = func(p *peerConnection) int { return p.HeaderCapacity(d.requestRTT()) }
		setIdle  = func(p *peerConnection, accepted int) { p.SetHeadersIdle(accepted) }
	)
	err := d.fetchParts(errCancelHeaderFetch, d.headerCh, deliver, d.queue.headerContCh, expire,
		d.queue.PendingHeaders, d.queue.InFlightHeaders, throttle, reserve,
		nil, fetch, d.queue.CancelHeaders, capacity, d.peers.HeaderIdlePeers, setIdle, "headers")

	log.Debug("Skeleton fill terminated", "err", err)

	filled, proced := d.queue.RetrieveHeaders()
	return filled, proced, err
}

// fetchBodies iteratively downloads the scheduled block bodies, taking any
// available peers, reserving a chunk of blocks for each, waiting for delivery
// and also periodically checking for timeouts.
//获取块的body.
func (d *Downloader) fetchBodies(from uint64) error {
	log.Debug("Downloading block bodies", "origin", from)

	var (
		deliver = func(packet dataPack) (int, error) {
			pack := packet.(*bodyPack)
			return d.queue.DeliverBodies(pack.peerId, pack.transactions, pack.uncles)
		}
		expire   = func() map[string]int { return d.queue.ExpireBodies(d.requestTTL()) }
		fetch    = func(p *peerConnection, req *fetchRequest) error { return p.FetchBodies(req) }
		capacity = func(p *peerConnection) int { return p.BlockCapacity(d.requestRTT()) }
		setIdle  = func(p *peerConnection, accepted int) { p.SetBodiesIdle(accepted) }
	)
	err := d.fetchParts(errCancelBodyFetch, d.bodyCh, deliver, d.bodyWakeCh, expire,
		d.queue.PendingBlocks, d.queue.InFlightBlocks, d.queue.ShouldThrottleBlocks, d.queue.ReserveBodies,
		d.bodyFetchHook, fetch, d.queue.CancelBodies, capacity, d.peers.BodyIdlePeers, setIdle, "bodies")

	log.Debug("Block body download terminated", "err", err)
	return err
}

// fetchReceipts iteratively downloads the scheduled block receipts, taking any
// available peers, reserving a chunk of receipts for each, waiting for delivery
// and also periodically checking for timeouts.
func (d *Downloader) fetchReceipts(from uint64) error {
	log.Debug("Downloading transaction receipts", "origin", from)

	var (
		deliver = func(packet dataPack) (int, error) {
			pack := packet.(*receiptPack)
			return d.queue.DeliverReceipts(pack.peerId, pack.receipts)
		}
		expire   = func() map[string]int { return d.queue.ExpireReceipts(d.requestTTL()) }
		fetch    = func(p *peerConnection, req *fetchRequest) error { return p.FetchReceipts(req) }
		capacity = func(p *peerConnection) int { return p.ReceiptCapacity(d.requestRTT()) }
		setIdle  = func(p *peerConnection, accepted int) { p.SetReceiptsIdle(accepted) }
	)
	err := d.fetchParts(errCancelReceiptFetch, d.receiptCh, deliver, d.receiptWakeCh, expire,
		d.queue.PendingReceipts, d.queue.InFlightReceipts, d.queue.ShouldThrottleReceipts, d.queue.ReserveReceipts,
		d.receiptFetchHook, fetch, d.queue.CancelReceipts, capacity, d.peers.ReceiptIdlePeers, setIdle, "receipts")

	log.Debug("Transaction receipt download terminated", "err", err)
	return err
}

// fetchParts iteratively downloads scheduled block parts, taking any available
// peers, reserving a chunk of fetch requests for each, waiting for delivery and
// also periodically checking for timeouts.
//
// As the scheduling/timeout logic mostly is the same for all downloaded data
// types, this method is used by each for data gathering and is instrumented with
// various callbacks to handle the slight differences between processing them.
//
// The instrumentation parameters:
//  - errCancel:   error type to return if the fetch operation is cancelled (mostly makes logging nicer)
//  - deliveryCh:  channel from which to retrieve downloaded data packets (merged from all concurrent peers)
//  - deliver:     processing callback to deliver data packets into type specific download queues (usually within `queue`)
//  - wakeCh:      notification channel for waking the fetcher when new tasks are available (or sync completed)
//  - expire:      task callback method to abort requests that took too long and return the faulty peers (traffic shaping)
//  - pending:     task callback for the number of requests still needing download (detect completion/non-completability)
//  - inFlight:    task callback for the number of in-progress requests (wait for all active downloads to finish)
//  - throttle:    task callback to check if the processing queue is full and activate throttling (bound memory use)
//  - reserve:     task callback to reserve new download tasks to a particular peer (also signals partial completions)
//  - fetchHook:   tester callback to notify of new tasks being initiated (allows testing the scheduling logic)
//  - fetch:       network callback to actually send a particular download request to a physical remote peer
//  - cancel:      task callback to abort an in-flight download request and allow rescheduling it (in case of lost peer)
//  - capacity:    network callback to retrieve the estimated type-specific bandwidth capacity of a peer (traffic shaping)
//  - idle:        network callback to retrieve the currently (type specific) idle peers that can be assigned tasks
//  - setIdle:     network callback to set a peer back to idle and update its estimated capacity (traffic shaping)
//  - kind:        textual label of the type being downloaded to display in log mesages
func (d *Downloader) fetchParts(errCancel error, deliveryCh chan dataPack, deliver func(dataPack) (int, error), wakeCh chan bool,
	expire func() map[string]int, pending func() int, inFlight func() bool, throttle func() bool, reserve func(*peerConnection, int) (*fetchRequest, bool, error),
	fetchHook func([]*types.Header), fetch func(*peerConnection, *fetchRequest) error, cancel func(*fetchRequest), capacity func(*peerConnection) int,
	idle func() ([]*peerConnection, int), setIdle func(*peerConnection, int), kind string) error {

	// Create a ticker to detect expired retrieval tasks
	ticker := time.NewTicker(100 * time.Millisecond)	//创建一个100毫秒的定时器。
	defer ticker.Stop()	//在函数退出的时候停止定时器。

	update := make(chan struct{}, 1)

	// Prepare the queue and fetch block parts until the block header fetcher's done
	finished := false
	for {
		select {
		case <-d.cancelCh:
			return errCancel

		case packet := <-deliveryCh:
			// If the peer was previously banned and failed to deliver it's pack
			// in a reasonable time frame, ignore it's message.
			if peer := d.peers.Peer(packet.PeerId()); peer != nil {
				// Deliver the received chunk of data and check chain validity
				accepted, err := deliver(packet)
				if err == errInvalidChain {
					return err
				}
				// Unless a peer delivered something completely else than requested (usually
				// caused by a timed out request which came through in the end), set it to
				// idle. If the delivery's stale, the peer should have already been idled.
				if err != errStaleDelivery {
					setIdle(peer, accepted)
				}
				// Issue a log to the user to see what's going on
				switch {
				case err == nil && packet.Items() == 0:
					peer.log.Trace("Requested data not delivered", "type", kind)
				case err == nil:
					peer.log.Trace("Delivered new batch of data", "type", kind, "count", packet.Stats())
				default:
					peer.log.Trace("Failed to deliver retrieved data", "type", kind, "err", err)
				}
			}
			// Blocks assembled, try to update the progress
			select {
			case update <- struct{}{}:
			default:
			}

		case cont := <-wakeCh:
			// The header fetcher sent a continuation flag, check if it's done
			if !cont {
				finished = true
			}
			// Headers arrive, try to update the progress
			select {
			case update <- struct{}{}:
			default:
			}

		case <-ticker.C:	//1.定时器到期。每间隔100毫秒都向update通道发送消息。
			// Sanity check update the progress
			select {
			case update <- struct{}{}:  //向update通道发送消息。
			default:
			}

		case <-update:	//2. update通道收到了触发消息。
			// Short circuit if we lost all our peers
			if d.peers.Len() == 0 {	//如果peer突然不存在了，那么就返回错误。
				return errNoPeers
			}
			// Check for fetch request timeouts and demote the responsible peers
			for pid, fails := range expire() {
				//
				if peer := d.peers.Peer(pid); peer != nil {
					// If a lot of retrieval elements expired, we might have overestimated the remote peer or perhaps
					// ourselves. Only reset to minimal throughput but don't drop just yet. If even the minimal times
					// out that sync wise we need to get rid of the peer.
					//
					// The reason the minimum threshold is 2 is because the downloader tries to estimate the bandwidth
					// and latency of a peer separately, which requires pushing the measures capacity a bit and seeing
					// how response times reacts, to it always requests one more than the minimum (i.e. min 2).
					if fails > 2 {
						peer.log.Trace("Data delivery timed out", "type", kind)
						setIdle(peer, 0)
					} else {
						peer.log.Debug("Stalling delivery, dropping", "type", kind)
						d.dropPeer(pid)
					}
				}
			}
			// If there's nothing more to fetch, wait or terminate
			if pending() == 0 {
				if !inFlight() && finished {
					log.Debug("Data fetching completed", "type", kind)
					return nil
				}
				break
			}
			// Send a download request to all idle peers, until throttled
			progressed, throttled, running := false, false, inFlight()
			idles, total := idle()

			for _, peer := range idles {
				// Short circuit if throttling activated
				if throttle() {
					throttled = true
					break
				}
				// Short circuit if there is no more available task.
				if pending() == 0 {
					break
				}
				// Reserve a chunk of fetches for a peer. A nil can mean either that
				// no more headers are available, or that the peer is known not to
				// have them.
				//构造fetchRequest.
				request, progress, err := reserve(peer, capacity(peer))	//给peer预留一个header用于获取，并构造fetchRequest请求。
				if err != nil {
					return err
				}
				if progress {
					progressed = true
				}
				if request == nil {
					continue
				}
				if request.From > 0 {
					peer.log.Trace("Requesting new batch of data", "type", kind, "from", request.From)
				} else if len(request.Headers) > 0 {
					peer.log.Trace("Requesting new batch of data", "type", kind, "count", len(request.Headers), "from", request.Headers[0].Number)
				} else {
					peer.log.Trace("Requesting new batch of data", "type", kind, "count", len(request.Hashes))
				}
				// Fetch the chunk and make sure any errors return the hashes to the queue
				if fetchHook != nil {
					fetchHook(request.Headers)
				}
				//从peer获取headers。
				if err := fetch(peer, request); err != nil {
					// Although we could try and make an attempt to fix this, this error really
					// means that we've double allocated a fetch task to a peer. If that is the
					// case, the internal state of the downloader and the queue is very wrong so
					// better hard crash and note the error instead of silently accumulating into
					// a much bigger issue.
					panic(fmt.Sprintf("%v: %s fetch assignment failed", peer, kind))
				}
				running = true
			}
			// Make sure that we have peers available for fetching. If all peers have been tried
			// and all failed throw an error
			if !progressed && !throttled && !running && len(idles) == total && pending() > 0 {
				return errPeersUnavailable
			}
		}
	}
}

// processHeaders takes batches of retrieved headers from an input channel and
// keeps processing and scheduling them into the header chain and downloader's
// queue until the stream ends or a failure occurs.
//processHeaders用于处理从peer收到的批量headers。
func (d *Downloader) processHeaders(origin uint64, td *big.Int) error {
	// Calculate the pivoting point for switching from fast to slow sync
	pivot := d.queue.FastSyncPivot() //计算fastSync中轴点。

	// Keep a count of uncertain headers to roll back
	rollback := []*types.Header{}
	defer func() {
		if len(rollback) > 0 {
			// Flatten the headers and roll them back
			hashes := make([]common.Hash, len(rollback))
			for i, header := range rollback {
				hashes[i] = header.Hash()
			}
			lastHeader, lastFastBlock, lastBlock := d.lightchain.CurrentHeader().Number, common.Big0, common.Big0
			if d.mode != LightSync {
				lastFastBlock = d.blockchain.CurrentFastBlock().Number()
				lastBlock = d.blockchain.CurrentBlock().Number()
			}
			d.lightchain.Rollback(hashes)
			curFastBlock, curBlock := common.Big0, common.Big0
			if d.mode != LightSync {
				curFastBlock = d.blockchain.CurrentFastBlock().Number()
				curBlock = d.blockchain.CurrentBlock().Number()
			}
			log.Warn("Rolled back headers", "count", len(hashes),
				"header", fmt.Sprintf("%d->%d", lastHeader, d.lightchain.CurrentHeader().Number),
				"fast", fmt.Sprintf("%d->%d", lastFastBlock, curFastBlock),
				"block", fmt.Sprintf("%d->%d", lastBlock, curBlock))

			// If we're already past the pivot point, this could be an attack, thread carefully
			if rollback[len(rollback)-1].Number.Uint64() > pivot {
				// If we didn't ever fail, lock in the pivot header (must! not! change!)
				if atomic.LoadUint32(&d.fsPivotFails) == 0 {
					for _, header := range rollback {
						if header.Number.Uint64() == pivot {
							log.Warn("Fast-sync pivot locked in", "number", pivot, "hash", header.Hash())
							d.fsPivotLock = header
						}
					}
				}
			}
		}
	}()

	// Wait for batches of headers to process
	gotHeaders := false

	for {
		select {
		case <-d.cancelCh:
			return errCancelHeaderProcessing

		case headers := <-d.headerProcCh:
			// Terminate header processing if we synced up
			if len(headers) == 0 {	//如果headers数组长度为0，那么说明所有header都处理完，header同步结束了。
				// Notify everyone that headers are fully processed
				for _, ch := range []chan bool{d.bodyWakeCh, d.receiptWakeCh} {
					select {
					case ch <- false:
					case <-d.cancelCh:
					}
				}
				// If no headers were retrieved at all, the peer violated it's TD promise that it had a
				// better chain compared to ours. The only exception is if it's promised blocks were
				// already imported by other means (e.g. fecher):
				//
				// R <remote peer>, L <local node>: Both at block 10
				// R: Mine block 11, and propagate it to L
				// L: Queue block 11 for import
				// L: Notice that R's head and TD increased compared to ours, start sync
				// L: Import of block 11 finishes
				// L: Sync begins, and finds common ancestor at 11
				// L: Request new headers up from 11 (R's TD was higher, it must have something)
				// R: Nothing to give
				if d.mode != LightSync {
					if !gotHeaders && td.Cmp(d.blockchain.GetTdByHash(d.blockchain.CurrentBlock().Hash())) > 0 {
						return errStallingPeer
					}
				}
				// If fast or light syncing, ensure promised headers are indeed delivered. This is
				// needed to detect scenarios where an attacker feeds a bad pivot and then bails out
				// of delivering the post-pivot blocks that would flag the invalid content.
				//
				// This check cannot be executed "as is" for full imports, since blocks may still be
				// queued for processing when the header download completes. However, as long as the
				// peer gave us something useful, we're already happy/progressed (above check).
				if d.mode == FastSync || d.mode == LightSync {
					if td.Cmp(d.lightchain.GetTdByHash(d.lightchain.CurrentHeader().Hash())) > 0 {
						return errStallingPeer
					}
				}
				// Disable any rollback and return
				rollback = nil
				return nil
			}
			// Otherwise split the chunk of headers into batches and process them
			gotHeaders = true

			for len(headers) > 0 {
				// Terminate if something failed in between processing chunks
				select {
				case <-d.cancelCh:
					return errCancelHeaderProcessing
				default:
				}
				// Select the next chunk of headers to import
				limit := maxHeadersProcess
				if limit > len(headers) {
					limit = len(headers)
				}
				chunk := headers[:limit]	//把收到的headers分批次一批一批处理。

				// In case of header only syncing, validate the chunk immediately
				if d.mode == FastSync || d.mode == LightSync {	//fast和light的同步模式，只下载块的header，不下载块的body。
					// Collect the yet unknown headers to mark them as uncertain
					unknown := make([]*types.Header, 0, len(headers))
					for _, header := range chunk {
						if !d.lightchain.HasHeader(header.Hash(), header.Number.Uint64()) {
							unknown = append(unknown, header)
						}
					}
					// If we're importing pure headers, verify based on their recentness
					frequency := fsHeaderCheckFrequency
					if chunk[len(chunk)-1].Number.Uint64()+uint64(fsHeaderForceVerify) > pivot {
						frequency = 1
					}
					if n, err := d.lightchain.InsertHeaderChain(chunk, frequency); err != nil {	//把headers加入到本地链上（块被同步了。）
						// If some headers were inserted, add them too to the rollback list
						if n > 0 {
							rollback = append(rollback, chunk[:n]...)
						}
						log.Debug("Invalid header encountered", "number", chunk[n].Number, "hash", chunk[n].Hash(), "err", err)
						return errInvalidChain
					}
					// All verifications passed, store newly found uncertain headers
					rollback = append(rollback, unknown...)
					if len(rollback) > fsHeaderSafetyNet {
						rollback = append(rollback[:0], rollback[len(rollback)-fsHeaderSafetyNet:]...)
					}
				}
				// If we're fast syncing and just pulled in the pivot, make sure it's the one locked in
				if d.mode == FastSync && d.fsPivotLock != nil && chunk[0].Number.Uint64() <= pivot && chunk[len(chunk)-1].Number.Uint64() >= pivot {
					if pivot := chunk[int(pivot-chunk[0].Number.Uint64())]; pivot.Hash() != d.fsPivotLock.Hash() {
						log.Warn("Pivot doesn't match locked in one", "remoteNumber", pivot.Number, "remoteHash", pivot.Hash(), "localNumber", d.fsPivotLock.Number, "localHash", d.fsPivotLock.Hash())
						return errInvalidChain
					}
				}
				// Unless we're doing light chains, schedule the headers for associated content retrieval
				//fullSync模式和fastSync模式会获取全部或者部分的块body。
				if d.mode == FullSync || d.mode == FastSync {
					// If we've reached the allowed number of pending headers, stall a bit
					//如果当前queue中pending的block或者receipt太多了，则需要等一等。
					for d.queue.PendingBlocks() >= maxQueuedHeaders || d.queue.PendingReceipts() >= maxQueuedHeaders {
						select {
						case <-d.cancelCh:
							return errCancelHeaderProcessing
						case <-time.After(time.Second):
						}
					}
					//header放到schedule中用于body获取。
					// Otherwise insert the headers for content retrieval
					inserts := d.queue.Schedule(chunk, origin)
					if len(inserts) != len(chunk) {
						log.Debug("Stale headers")
						return errBadPeer
					}
				}
				headers = headers[limit:]
				origin += uint64(limit)
			}
			// Signal the content downloaders of the availablility of new tasks
			//给body下载协程发送消息通知新的task可用。
			for _, ch := range []chan bool{d.bodyWakeCh, d.receiptWakeCh} {
				select {
				case ch <- true:
				default:
				}
			}
		}
	}
}

// processFullSyncContent takes fetch results from the queue and imports them into the chain.
func (d *Downloader) processFullSyncContent() error {
	for {
		results := d.queue.WaitResults()
		if len(results) == 0 {
			return nil
		}
		if d.chainInsertHook != nil {
			d.chainInsertHook(results)
		}
		if err := d.importBlockResults(results); err != nil {
			return err
		}
	}
}

//full sync模式下插入塊.
func (d *Downloader) importBlockResults(results []*fetchResult) error {
	for len(results) != 0 {
		// Check for any termination requests. This makes clean shutdown faster.
		select {
		case <-d.quitCh:
			return errCancelContentProcessing
		default:
		}
		// Retrieve the a batch of results to import
		items := int(math.Min(float64(len(results)), float64(maxResultsProcess)))
		first, last := results[0].Header, results[items-1].Header
		log.Debug("Inserting downloaded chain", "items", len(results),
			"firstnum", first.Number, "firsthash", first.Hash(),
			"lastnum", last.Number, "lasthash", last.Hash(),
		)
		blocks := make([]*types.Block, items)
		for i, result := range results[:items] {
			blocks[i] = types.NewBlockWithHeader(result.Header).WithBody(result.Transactions, result.Uncles)
		}
		if index, err := d.blockchain.InsertChain(blocks); err != nil {
			log.Debug("Downloaded item processing failed", "number", results[index].Header.Number, "hash", results[index].Header.Hash(), "err", err)
			return errInvalidChain
		}
		// Shift the results to the next batch
		results = results[items:]
	}
	return nil
}

// processFastSyncContent takes fetch results from the queue and writes them to the
// database. It also controls the synchronisation of state nodes of the pivot block.
func (d *Downloader) processFastSyncContent(latest *types.Header) error {
	// Start syncing state of the reported head block.
	// This should get us most of the state of the pivot block.
	stateSync := d.syncState(latest.Root)	//創建一個新的狀態同步對象.
	defer stateSync.Cancel()
	go func() {
		if err := stateSync.Wait(); err != nil {	//等待狀態同步對象結束后關閉queue.
			d.queue.Close() // wake up WaitResults
		}
	}()

	pivot := d.queue.FastSyncPivot()	//獲得中軸點.
	for {
		results := d.queue.WaitResults()
		if len(results) == 0 {
			return stateSync.Cancel()
		}
		if d.chainInsertHook != nil {
			d.chainInsertHook(results)
		}
		P, beforeP, afterP := splitAroundPivot(pivot, results)	//對從peer獲取到的結果做分離,p:中軸點; beforeP: 採用fastSync方式的節點; afterP: 採用fullSync方式的節點.
		//fast模式處理beforeP.
		if err := d.commitFastSyncData(beforeP, stateSync); err != nil {
			return err
		}
		//處理pivot點.
		if P != nil {
			stateSync.Cancel()
			if err := d.commitPivotBlock(P); err != nil {
				return err
			}
		}
		//fullsync方式處理afterP.
		if err := d.importBlockResults(afterP); err != nil {
			return err
		}
	}
}

//pivot 中軸點. results 從peer獲取的結果.
func splitAroundPivot(pivot uint64, results []*fetchResult) (p *fetchResult, before, after []*fetchResult) {
	for _, result := range results {
		num := result.Header.Number.Uint64()
		switch {
		case num < pivot:
			before = append(before, result)
		case num == pivot:
			p = result
		default:
			after = append(after, result)
		}
	}
	return p, before, after
}

//採用fastSync方式對塊上鏈.
func (d *Downloader) commitFastSyncData(results []*fetchResult, stateSync *stateSync) error {
	for len(results) != 0 {
		// Check for any termination requests.
		select {
		case <-d.quitCh:
			return errCancelContentProcessing
		case <-stateSync.done:
			if err := stateSync.Wait(); err != nil {
				return err
			}
		default:
		}
		// Retrieve the a batch of results to import
		items := int(math.Min(float64(len(results)), float64(maxResultsProcess)))	//一次上鏈的塊數不能夠超過2048
		first, last := results[0].Header, results[items-1].Header
		log.Debug("Inserting fast-sync blocks", "items", len(results),
			"firstnum", first.Number, "firsthash", first.Hash(),
			"lastnumn", last.Number, "lasthash", last.Hash(),
		)
		blocks := make([]*types.Block, items)
		receipts := make([]types.Receipts, items)
		for i, result := range results[:items] {	//對每個result構造針對這個result的新塊.
			blocks[i] = types.NewBlockWithHeader(result.Header).WithBody(result.Transactions, result.Uncles)
			receipts[i] = result.Receipts	//獲得結果中的receipts.
		}
		//塊上鏈.
		if index, err := d.blockchain.InsertReceiptChain(blocks, receipts); err != nil {
			log.Debug("Downloaded item processing failed", "number", results[index].Header.Number, "hash", results[index].Header.Hash(), "err", err)
			return errInvalidChain
		}
		// Shift the results to the next batch
		results = results[items:]
	}
	return nil
}

func (d *Downloader) commitPivotBlock(result *fetchResult) error {
	b := types.NewBlockWithHeader(result.Header).WithBody(result.Transactions, result.Uncles)
	// Sync the pivot block state. This should complete reasonably quickly because
	// we've already synced up to the reported head block state earlier.
	if err := d.syncState(b.Root()).Wait(); err != nil {
		return err
	}
	log.Debug("Committing fast sync pivot as new head", "number", b.Number(), "hash", b.Hash())
	if _, err := d.blockchain.InsertReceiptChain([]*types.Block{b}, []types.Receipts{result.Receipts}); err != nil {
		return err
	}
	return d.blockchain.FastSyncCommitHead(b.Hash())
}

// DeliverHeaders injects a new batch of block headers received from a remote
// node into the download schedule.
//把peer传递过来的一堆headers注入到下载日程表中.(把收到的headers放入downloader的通道headerCh)
func (d *Downloader) DeliverHeaders(id string, headers []*types.Header) (err error) {
	return d.deliver(id, d.headerCh, &headerPack{id, headers}, headerInMeter, headerDropMeter)
}

// DeliverBodies injects a new batch of block bodies received from a remote node.
//把bodyPack放入到d.bodyCh中.
func (d *Downloader) DeliverBodies(id string, transactions [][]*types.Transaction, uncles [][]*types.Header) (err error) {
	return d.deliver(id, d.bodyCh, &bodyPack{id, transactions, uncles}, bodyInMeter, bodyDropMeter)
}

// DeliverReceipts injects a new batch of receipts received from a remote node.
func (d *Downloader) DeliverReceipts(id string, receipts [][]*types.Receipt) (err error) {
	return d.deliver(id, d.receiptCh, &receiptPack{id, receipts}, receiptInMeter, receiptDropMeter)
}

// DeliverNodeData injects a new batch of node state data received from a remote node.
func (d *Downloader) DeliverNodeData(id string, data [][]byte) (err error) {
	return d.deliver(id, d.stateCh, &statePack{id, data}, stateInMeter, stateDropMeter)
}

// deliver injects a new batch of data received from a remote node.
//deliver的作用就是把数据注入到指定的通道中.
//id: peer id.
//destCh: 目的通道,数据就是注入到这个通道.
//packet: 注入到通道中的数据包.
func (d *Downloader) deliver(id string, destCh chan dataPack, packet dataPack, inMeter, dropMeter metrics.Meter) (err error) {
	// Update the delivery metrics for both good and failed deliveries
	inMeter.Mark(int64(packet.Items()))
	defer func() {
		if err != nil {
			dropMeter.Mark(int64(packet.Items()))
		}
	}()
	// Deliver or abort if the sync is canceled while queuing
	d.cancelLock.RLock()
	cancel := d.cancelCh
	d.cancelLock.RUnlock()
	if cancel == nil {
		return errNoSyncActive
	}
	select {
	case destCh <- packet:	//把数据包注入到通道中.
		return nil
	case <-cancel:
		return errNoSyncActive
	}
}

// qosTuner is the quality of service tuning loop that occasionally gathers the
// peer latency statistics and updates the estimated request round trip time.
//qosTurner()函数周期性的统计peer的延迟时间,然后更新qos参数.
//更新的两个参数为: rttEstimate:rtt估计值,rttConfidence:估计值的可信度.
func (d *Downloader) qosTuner() {
	for {
		//设置下载请求的rtt估值字段rttEstimate.
		// Retrieve the current median RTT and integrate into the previoust target RTT
		rtt := time.Duration(float64(1-qosTuningImpact)*float64(atomic.LoadUint64(&d.rttEstimate)) + qosTuningImpact*float64(d.peers.medianRTT()))
		atomic.StoreUint64(&d.rttEstimate, uint64(rtt))

		//设置rtt估值的可信度.
		// A new RTT cycle passed, increase our confidence in the estimated RTT
		conf := atomic.LoadUint64(&d.rttConfidence)
		conf = conf + (1000000-conf)/2
		atomic.StoreUint64(&d.rttConfidence, conf)

		// Log the new QoS values and sleep until the next RTT
		log.Debug("Recalculated downloader QoS values", "rtt", rtt, "confidence", float64(conf)/1000000.0, "ttl", d.requestTTL())
		select {
		case <-d.quitCh:	//接收退出信号,收到退出信号后函数退出.
			return
		case <-time.After(rtt):	//等待rtt的超时时间,然后重新计算rtt,重新开始计算超时.
		}
	}
}

// qosReduceConfidence is meant to be called when a new peer joins the downloader's
// peer set, needing to reduce the confidence we have in out QoS estimates.
func (d *Downloader) qosReduceConfidence() {
	// If we have a single peer, confidence is always 1
	peers := uint64(d.peers.Len())
	if peers == 0 {
		// Ensure peer connectivity races don't catch us off guard
		return
	}
	if peers == 1 {
		atomic.StoreUint64(&d.rttConfidence, 1000000)
		return
	}
	// If we have a ton of peers, don't drop confidence)
	if peers >= uint64(qosConfidenceCap) {
		return
	}
	// Otherwise drop the confidence factor
	conf := atomic.LoadUint64(&d.rttConfidence) * (peers - 1) / peers
	if float64(conf)/1000000 < rttMinConfidence {
		conf = uint64(rttMinConfidence * 1000000)
	}
	atomic.StoreUint64(&d.rttConfidence, conf)

	rtt := time.Duration(atomic.LoadUint64(&d.rttEstimate))
	log.Debug("Relaxed downloader QoS values", "rtt", rtt, "confidence", float64(conf)/1000000.0, "ttl", d.requestTTL())
}

// requestRTT returns the current target round trip time for a download request
// to complete in.
//
// Note, the returned RTT is .9 of the actually estimated RTT. The reason is that
// the downloader tries to adapt queries to the RTT, so multiple RTT values can
// be adapted to, but smaller ones are preffered (stabler download stream).
//返回请求的目标RTT.
func (d *Downloader) requestRTT() time.Duration {
	return time.Duration(atomic.LoadUint64(&d.rttEstimate)) * 9 / 10	//返回目标请求RTT,是RTT估计值的十分之九.
}

// requestTTL returns the current timeout allowance for a single download request
// to finish under.
//获得请求超时时间.
func (d *Downloader) requestTTL() time.Duration {
	var (
		rtt  = time.Duration(atomic.LoadUint64(&d.rttEstimate))	//rtt: round-trip time,往返时间.ttl:time to live,生存时间值.
		conf = float64(atomic.LoadUint64(&d.rttConfidence)) / 1000000.0
	)
	//获得当前rtt和confidence.
	ttl := time.Duration(ttlScaling) * time.Duration(float64(rtt)/conf)	//计算请求超时时间.
	if ttl > ttlLimit {		//如果计算的ttl大于1分钟,那么设置为1分钟.
		ttl = ttlLimit
	}
	return ttl	//返回计算出来的ttl.
}
