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

package miner

import (
	//"bytes"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wanchain/go-wanchain/common"
	"github.com/wanchain/go-wanchain/consensus"
	//"github.com/wanchain/go-wanchain/consensus/misc"
	"github.com/wanchain/go-wanchain/core"
	"github.com/wanchain/go-wanchain/core/state"
	"github.com/wanchain/go-wanchain/core/types"
	"github.com/wanchain/go-wanchain/core/vm"
	"github.com/wanchain/go-wanchain/ethdb"
	"github.com/wanchain/go-wanchain/event"
	"github.com/wanchain/go-wanchain/log"
	"github.com/wanchain/go-wanchain/params"
	set "gopkg.in/fatih/set.v0"
)

const (
	resultQueueSize  = 10		//cpu agent处理的结果会返回到结果通道,此为结果通道的buffer大小.
	miningLogAtDepth = 5		//挖矿日志深度,用于存储unconfirmed块集合。

	// txChanSize is the size of channel listening to TxPreEvent.
	// The number is referenced from the size of tx pool.
	txChanSize = 4096		//txChan通道buffer大小.
	// chainHeadChanSize is the size of channel listening to ChainHeadEvent.
	chainHeadChanSize = 10	//chainHead事件通道的buffer大小.
	// chainSideChanSize is the size of channel listening to ChainSideEvent.
	chainSideChanSize = 10	//chainSide事件通道的buffer大小.
)

// Agent can register themself with the worker
//一个worker中可以注册多个agent.
type Agent interface {
	Work() chan<- *Work		//返回一个只写通道,用于worker把组装好的Work对象传递给agent.
	SetReturnCh(chan<- *Result)	//设置结果返回通道.
	Stop()		//停止agent.
	Start()		//启动agent.
	GetHashRate() int64
}

// Work is the workers current environment and holds
// all of the current state information
type Work struct {
	config *params.ChainConfig	//配置参数.
	signer types.Signer

	state     *state.StateDB // apply state changes here	//存储区块链的状态变化。
	ancestors *set.Set       // ancestor set (used for checking uncle parent validity)
	family    *set.Set       // family set (used for checking uncle invalidity)
	uncles    *set.Set       // uncle set
	tcount    int            // tx count in cycle

	Block *types.Block // the new block

	header   *types.Header			//区块头指针.
	txs      []*types.Transaction	//交易列表.
	receipts []*types.Receipt

	createdAt time.Time		//Work对象的时间戳.
}

//agent处理的返回结果.
type Result struct {
	Work  *Work		//worker对象传递过来的Work对象,此结构中的Block就是基于这个Work对象挖掘出来的.
	Block *types.Block	//返回的区块.
}

// worker is the main object which takes care of applying messages to the new state
type worker struct {
	config *params.ChainConfig	//配置参数.
	engine consensus.Engine		//共识算法引擎.

	mu sync.Mutex	//worker对象操作的互斥锁.

	// update loop
	mux          *event.TypeMux
	txCh         chan core.TxPreEvent			//TxPreEvent事件
	txSub        event.Subscription				//TxPreEvent订阅
	chainHeadCh  chan core.ChainHeadEvent		//ChainHeadEvent事件
	chainHeadSub event.Subscription				//ChainHeadEvent订阅
	chainSideCh  chan core.ChainSideEvent		//ChainSideEvent事件
	chainSideSub event.Subscription				//ChainSideEvent订阅
	wg           sync.WaitGroup

	agents map[Agent]struct{}	//此语法实际上就是实现了一个Agent的set.
	recv   chan *Result		//worker中的agent挖出一个块之后,结果会返回到recv通道,这个recv通道就是用来接收从agent返回的挖矿结果.

	eth     Backend
	chain   *core.BlockChain
	proc    core.Validator
	chainDb ethdb.Database

	coinbase common.Address		//挖矿的账号.
	extra    []byte			//worker对象的extra数据.

	currentMu sync.Mutex	//当前work对象锁.
	current   *Work			//当前的work对象.

	uncleMu        sync.Mutex	//uncle块的锁,操作possibleUncles时会对其加锁.
	possibleUncles map[common.Hash]*types.Block

	unconfirmed *unconfirmedBlocks // set of locally mined blocks pending canonicalness confirmations

	// atomic status counters
	mining int32	//是否正在挖矿标记.
	atWork int32	//正在工作的agent计数.

	// Seal work used minimum time(second).
	// If used less time actual, will wait time gap before deal with new mined block.
	miniSealTime int64
}

//创建一个新的worker对象. 不对miner包外公开,在Miner对象创建的时候被调用.
func newWorker(config *params.ChainConfig, engine consensus.Engine, coinbase common.Address, eth Backend, mux *event.TypeMux) *worker {
	worker := &worker{  //创建worker对象.
		config:         config,
		engine:         engine,		//共识算法引擎.
		eth:            eth,
		mux:            mux,
		txCh:           make(chan core.TxPreEvent, txChanSize),				//txCh是一个接受TxPreEvent类型的异步通道.
		chainHeadCh:    make(chan core.ChainHeadEvent, chainHeadChanSize),	//ChainHeadEvent事件通道.
		chainSideCh:    make(chan core.ChainSideEvent, chainSideChanSize), 	//ChainSideEvent事件通道.
		chainDb:        eth.ChainDb(),
		recv:           make(chan *Result, resultQueueSize),		//接收agent的返回结果.
		chain:          eth.BlockChain(),
		proc:           eth.BlockChain().Validator(),
		possibleUncles: make(map[common.Hash]*types.Block),
		coinbase:       coinbase,	//挖矿用的账号.
		agents:         make(map[Agent]struct{}),		//worker关联的agent列表.
		unconfirmed:    newUnconfirmedBlocks(eth.BlockChain(), miningLogAtDepth),	//创建一个unconfirmed块集合。
		miniSealTime:   12,
	}

	// Subscribe TxPreEvent for tx pool  //订阅txPool的TxPreEvent事件.
	worker.txSub = eth.TxPool().SubscribeTxPreEvent(worker.txCh)  //订阅了txpool的TxPreEvent.

	// Subscribe events for blockchain  //订阅blockchain的chainHeadCh和chainSideCh事件.
	worker.chainHeadSub = eth.BlockChain().SubscribeChainHeadEvent(worker.chainHeadCh)		//订阅chainHeadCh事件.
	worker.chainSideSub = eth.BlockChain().SubscribeChainSideEvent(worker.chainSideCh)		//订阅chainSideCh事件.
	go worker.update()	//创建协程.等待并处理以上三个事件,

	go worker.wait()    //创建协程,等待并处理cpuAgent挖掘出来并返回的区块.
	worker.commitNewWork()  //组装区块提交给agent处理.

	return worker
}

//设置worker对象的coinbase.
func (self *worker) setEtherbase(addr common.Address) {
	self.mu.Lock()	//给worker对象加锁.
	defer self.mu.Unlock()	//函数退出之后自动解锁.
	self.coinbase = addr	//设置worker的coinbase.
}

//设置extraData.
func (self *worker) setExtra(extra []byte) {
	self.mu.Lock()	//worker对象加锁.
	defer self.mu.Unlock()		//函数退出的时候worker对象解锁.
	self.extra = extra		//设置extra data.
}

//获得当前pending块及相关的状态.
func (self *worker) pending() (*types.Block, *state.StateDB) {
	self.currentMu.Lock()	//加当前work对象锁.
	defer self.currentMu.Unlock()	//函数退出时解除当前work对象锁.

	if atomic.LoadInt32(&self.mining) == 0 {	//如果没有挖矿,那么临时构造一个对象并返回.
		return types.NewBlock(
			self.current.header,
			self.current.txs,
			nil,
			self.current.receipts,
		), self.current.state.Copy()
	}
	return self.current.Block /*当前pending的块*/, self.current.state.Copy() /*返回一个当前区块链状态的独立拷贝*/
}

//获得pending block.
func (self *worker) pendingBlock() *types.Block {
	self.currentMu.Lock()	//当前Work对象加锁.
	defer self.currentMu.Unlock()	//函数退出时自动给当前Work对象解锁.

	if atomic.LoadInt32(&self.mining) == 0 {	//如果没有挖矿,那么组装一个新区块返回.
		return types.NewBlock(
			self.current.header,
			self.current.txs,
			nil,
			self.current.receipts,
		)
	}
	return self.current.Block	//如果正在挖矿,那么返回当前已经生成但是还没有被确认上链的块.
}

//开始挖矿.
func (self *worker) start() {
	self.mu.Lock()	//对worker对象加锁.
	defer self.mu.Unlock()		//函数退出解锁.
	atomic.StoreInt32(&self.mining, 1)		//设置正在挖矿的标志位.
	if istanbul, ok := self.engine.(consensus.Istanbul); ok {	//如果使用ibft,那么启动ibft.(把engine类型强制转换为Istanbul类型.t.(Type))
		//如果是istanbul engine,那么进到此处.
		istanbul.Start(self.chain, self.chain.CurrentBlock, self.chain.HasBadBlock)	//ibft启动.
	}

	// spin up agents	//启动worker中的所有agents.
	for agent := range self.agents {
		agent.Start()	//agent启动.
	}
}

//停止挖矿.
func (self *worker) stop() {
	self.wg.Wait()	//阻塞执行,知道waitgroup中的计数变为0.

	self.mu.Lock()	//worker对象互斥锁加锁.
	defer self.mu.Unlock()	//在函数退出的时候解锁.
	if atomic.LoadInt32(&self.mining) == 1 {	//如果正在挖矿那么停止所有agent.
		for agent := range self.agents {	//停止所有agent.
			agent.Stop()	//停止agent.
		}
	}
	atomic.StoreInt32(&self.mining, 0)		//更改正在挖矿额标志位位0.
	atomic.StoreInt32(&self.atWork, 0)		//更改正在工作的agent数量为0.
}

//注册agent到worker对象中.
func (self *worker) register(agent Agent) {
	self.mu.Lock()	//worker对象加锁.
	defer self.mu.Unlock()		//函数退出解锁.
	self.agents[agent] = struct{}{}	//加入到worker的agent列表.
	agent.SetReturnCh(self.recv)	//设置agent的结果返回通道, worker在wait()函数中会读取这个通道的消息并处理agent返回的结果.
}

//从worker中把agent解除注册.
func (self *worker) unregister(agent Agent) {
	self.mu.Lock()	//worker对象加锁.
	defer self.mu.Unlock()	//函数退出解锁.
	delete(self.agents, agent)	//从worker的agent列表中把指定agent删除.
	agent.Stop()	//停止agent.
}

//响应并处理worker订阅的事件.
func (self *worker) update() {
	defer self.txSub.Unsubscribe()			//update()退出时解除注册TxPreEvent事件.
	defer self.chainHeadSub.Unsubscribe()	//update()退出时解除注册ChainHeadEvent事件.
	defer self.chainSideSub.Unsubscribe()	//update()退出时解除注册ChainSideEvent事件.

	for {	//无限循环,处理并响应事件.
		// A real event arrived, process interesting content
		select {
		// Handle ChainHeadEvent
		case <-self.chainHeadCh:	//处理ChainHeadEvent.
			if h, ok := self.engine.(consensus.Handler); ok {
				h.NewChainHead()
			}
			self.commitNewWork()

		// Handle ChainSideEvent
		case ev := <-self.chainSideCh:		//处理ChainSideEvent事件.
			self.uncleMu.Lock()
			self.possibleUncles[ev.Block.Hash()] = ev.Block
			self.uncleMu.Unlock()

		// Handle TxPreEvent
		case ev := <-self.txCh:	//处理TxPreEvent事件.
			// Apply transaction to the pending state if we're not mining
			if atomic.LoadInt32(&self.mining) == 0 {  //判断是否正在挖矿,如果没有正在挖矿,那么执行交易.
				self.currentMu.Lock()
				acc, _ := types.Sender(self.current.signer, ev.Tx)
				txs := map[common.Address]types.Transactions{acc: {ev.Tx}}
				txset := types.NewTransactionsByPriceAndNonce(self.current.signer, txs)

				self.current.commitTransactions(self.mux, txset, self.chain, self.coinbase)  //此处会执行交易.
				self.currentMu.Unlock()
			}

		// System stopped	系统停止则udpate()函数返回.
		case <-self.txSub.Err():
			return
		case <-self.chainHeadSub.Err():
			return
		case <-self.chainSideSub.Err():
			return
		}
	}
}

//worker对象等待并处理从agent返回的区块.
func (self *worker) wait() {
	for {
		mustCommitNewWork := true
		for result := range self.recv {	//挖掘出了新的区块之后,agent会把已经经过共识算法确认的区块放入到recv通道,然后worker对象从recv通道中读取已经确认的区块进行处理.
			atomic.AddInt32(&self.atWork, -1)	//从recv中读出一个块,说明一个agent实例已经完成了任务,因此atWork减1.

			if result == nil {	//读出结果为空,则继续等待.
				continue
			}
			block := result.Block	//获得已经开始共识的块。
			work := result.Work		//获得此块对应的Work对象.

			// waiting minimum sealing time
			//beginTime := block.Header().Time.Int64()
			//for time.Now().Unix()-beginTime < self.miniSealTime {
			//	log.Trace("need wait minimum sealing time", "now", time.Now().Unix(), "block begin", beginTime)
			//	time.Sleep(time.Millisecond * 100)
			//
			//	// if have synchronized new block from remote, stop waiting at once.
			//	if self.chain.CurrentHeader().Number.Cmp(block.Header().Number) >= 0 {
			//		log.Info("have synchronized new block from remote, should stop wait sealing minimum time at once",
			//			"chain last block", self.chain.CurrentHeader().Number.Int64(),
			//			"the waiting block", block.Header().Number.Int64())
			//		break
			//	}
			//}

			// Update the block hash in all logs since it is now available and not when the
			// receipt/log of individual transactions were created.
			for _, r := range work.receipts {
				for _, l := range r.Logs {
					l.BlockHash = block.Hash()
				}
			}
			for _, log := range work.state.Logs() {
				log.BlockHash = block.Hash()
			}

			//块上链.
			stat, err := self.chain.WriteBlockAndState(block, work.receipts, work.state)
			if err != nil {
				log.Error("Failed writing block to chain", "err", err)
				continue
			}
			// check if canon block and write transactions
			if stat == core.CanonStatTy {
				// implicit by posting ChainHeadEvent
				mustCommitNewWork = false
			}
			// Broadcast the block and announce chain insertion event
			self.mux.Post(core.NewMinedBlockEvent{Block: block})
			var (
				events []interface{}
				logs   = work.state.Logs()
			)
			events = append(events, core.ChainEvent{Block: block, Hash: block.Hash(), Logs: logs})
			if stat == core.CanonStatTy {
				events = append(events, core.ChainHeadEvent{Block: block})
			}
			self.chain.PostChainEvents(events, logs)

			// Insert the block into the set of pending ones to wait for confirmations
			self.unconfirmed.Insert(block.NumberU64(), block.Hash())	//把agent返回来已经经过共识算法确认的，并且已经写到链上的块放到unconfirmed块集合中等待确认。

			if mustCommitNewWork {
				self.commitNewWork()
			}
		}
	}
}

// push sends a new work task to currently live miner agents.
//把Work对象交给agent处理.
func (self *worker) push(work *Work) {
	//没有挖矿则不处理.
	if atomic.LoadInt32(&self.mining) != 1 {
		return
	}

	//(在创建miner对象及worker对象的代码流程中会调用commitNewWork()函数,然而在这个时候cpu agent还没有创建,所以对象创建时执行commitNewWork()并不会走进下面这个循环.)
	for agent := range self.agents {
		atomic.AddInt32(&self.atWork, 1) //正在工作的agent计数加1,当前只有一个cpu agent,没有其他agent.
		if ch := agent.Work(); ch != nil {		//获得agent的Work对象写入通道,然后把组装好的Work对象传递给agent.
			ch <- work	//把组装好的Work对象写入agent通道中.
		}
	}
}

// makeCurrent creates a new environment for the current cycle.
// 生成一个新的Work对象.
func (self *worker) makeCurrent(parent *types.Block, header *types.Header) error {
	state, err := self.chain.StateAt(parent.Root())	//从parent的root创建一个新的状态。
	if err != nil {
		return err
	}
	//创建一个新的Work对象.
	work := &Work{
		config:    self.config,
		signer:    types.NewEIP155Signer(self.config.ChainId),
		state:     state,
		ancestors: set.New(),
		family:    set.New(),
		uncles:    set.New(),
		header:    header,
		createdAt: time.Now(),
	}

	// when 08 is processed ancestors contain 07 (quick block)
	for _, ancestor := range self.chain.GetBlocksFromHash(parent.Hash(), 7) {
		for _, uncle := range ancestor.Uncles() {
			work.family.Add(uncle.Hash())
		}
		work.family.Add(ancestor.Hash())
		work.ancestors.Add(ancestor.Hash())
	}

	// Keep track of transactions which return errors so they can be removed
	work.tcount = 0		//Work对象构造出来之后里面没有交易.
	self.current = work	//设置worker对象的当前Work对象为新生成的Work对象.
	return nil
}

//组装新的Work给agent.
func (self *worker) commitNewWork() {
	self.mu.Lock()	//worker对象锁.
	defer self.mu.Unlock()	//函数退出的时候解锁.
	self.uncleMu.Lock()		//uncle块锁加锁
	defer self.uncleMu.Unlock()	//函数退出时解锁.
	self.currentMu.Lock()	//当前work加锁
	defer self.currentMu.Unlock()	//函数退出后解锁.

	tstart := time.Now()	//获得当前系统时间.
	parent := self.chain.CurrentBlock()		//获得当前的链头区块.

	tstamp := tstart.Unix()		//转换成时间戳格式,单位为秒.
	//如果当前链头块的时间比当前时间还新,说明对当前node时间比较滞后了, 那么把时间戳设置为parent的时间戳加1,供后续比较使用.
	if parent.Time().Cmp(new(big.Int).SetInt64(tstamp)) >= 0 {
		tstamp = parent.Time().Int64() + 1
	}

	//链头比当前时间太超前的话,则此节点需要sleep一段时间之后再出块.
	// this will ensure we're not going off too far in the future
	if now := time.Now().Unix(); tstamp > now+1 {	//当前时间落后于区块链头的时间戳了,则休眠等待时间到达区块链头的时间.
		wait := time.Duration(tstamp-now) * time.Second
		log.Info("Mining too far in the future", "wait", common.PrettyDuration(wait))
		time.Sleep(wait)
	}

	num := parent.Number()	//获得parent Block编号.
	header := &types.Header{	//生成一个新的区块头.此时header中的Root字段还为空。
		ParentHash: parent.Hash(),	//parent区块的hash.
		Number:     num.Add(num, common.Big1),	// 返回parent的Number +1.
		GasLimit:   core.CalcGasLimit(parent),	// 计算区块的gasLimit.
		GasUsed:    new(big.Int),
		Extra:      self.extra,		//把worker的extra数据拷贝到区块头中.
		Time:       big.NewInt(tstamp),		//出块时间.
	}

	//设置区块头的coinbase为当前挖矿的账号.
	// Only set the coinbase if we are mining (avoid spurious block rewards)
	if atomic.LoadInt32(&self.mining) == 1 {
		header.Coinbase = self.coinbase
	}

	//调用了istanbul的Prepare()函数完成header对象的准备。.
	if err := self.engine.Prepare(self.chain, header, atomic.LoadInt32(&self.mining) == 1); err != nil {	//istanbul中也没有对header中的root赋值。
		log.Error("Failed to prepare header for mining", "err", err)
		return
	}
	// If we are care about TheDAO hard-fork check whether to override the extra-data or not
	//if daoBlock := self.config.DAOForkBlock; daoBlock != nil {
	//	// Check whether the block is among the fork extra-override range
	//	limit := new(big.Int).Add(daoBlock, params.DAOForkExtraRange)
	//	if header.Number.Cmp(daoBlock) >= 0 && header.Number.Cmp(limit) < 0 {
	//		// Depending whether we support or oppose the fork, override differently
	//		if self.config.DAOForkSupport {
	//			header.Extra = common.CopyBytes(params.DAOForkBlockExtra)
	//		} else if bytes.Equal(header.Extra, params.DAOForkBlockExtra) {
	//			header.Extra = []byte{} // If miner opposes, don't let it use the reserved extra-data
	//		}
	//	}
	//}

	//生成当前Work对象.
	// Could potentially happen if starting to mine in an odd state.
	err := self.makeCurrent(parent, header)
	if err != nil {
		log.Error("Failed to create mining context", "err", err)
		return
	}
	// Create the current work task and check any fork transitions needed
	work := self.current  //此时work.tcount还是0,也就是work中的交易数为0.
	//if self.config.DAOForkSupport && self.config.DAOForkBlock != nil && self.config.DAOForkBlock.Cmp(header.Number) == 0 {
	//	misc.ApplyDAOHardFork(work.state)
	//}

	//获得可以打包的交易列表.
	pending, err := self.eth.TxPool().Pending()	//获得txPool中所有pending的交易.
	if err != nil {
		log.Error("Failed to fetch pending transactions", "err", err)
		return
	}
	txs := types.NewTransactionsByPriceAndNonce(self.current.signer, pending)	//返回排序后的交易列表.
	work.commitTransactions(self.mux, txs, self.chain, self.coinbase)	//执行这些交易,并把处理好的交易信息写入到Work对象中.

	// compute uncles for the new block.
	//var (
	//	uncles    []*types.Header
	//	badUncles []common.Hash
	//)
	//for hash, uncle := range self.possibleUncles {
	//	if len(uncles) == 2 {
	//		break
	//	}
	//	if err := self.commitUncle(work, uncle.Header()); err != nil {
	//		log.Trace("Bad uncle found and will be removed", "hash", hash)
	//		log.Trace(fmt.Sprint(uncle))
	//
	//		badUncles = append(badUncles, hash)
	//	} else {
	//		log.Debug("Committing new uncle to block", "hash", hash)
	//		uncles = append(uncles, uncle.Header())
	//	}
	//}
	//for _, hash := range badUncles {
	//	delete(self.possibleUncles, hash)
	//}
	uncles := []*types.Header{}
	// Create the new block to seal with the consensus engine
	//返回一个最终组装好了的块交给共识算法来确认.（给区块“定型”）
	if work.Block, err = self.engine.Finalize(self.chain, header, work.state, work.txs, uncles, work.receipts); err != nil {
		log.Error("Failed to finalize block for sealing", "err", err)
		return
	}
	// We only care about logging if we're actually mining.
	if atomic.LoadInt32(&self.mining) == 1 {
		log.Info("Commit new mining work", "number", work.Block.Number(), "txs", work.tcount, "uncles", len(uncles), "elapsed", common.PrettyDuration(time.Since(tstart)))
		self.unconfirmed.Shift(work.Block.NumberU64() - 1)	//当前区块的Number减去1就是其parent区块的Number号。
	}
	self.push(work)	//把组装好的Work对象交给agent来处理.
	//在执行self.engine.Finalize()之前,所有交易都已经被执行完毕,所有receipts都已经收到了,在Finalize()函数中已经更新了header.root,也就是说块的状态已经定下来了.
	//self.push(work)只是填充一下不需要修改header的内容,然后让各个节点去共识.
	//在self.push(work)之前Finalize()的时候block header中的txHash, receiptHash和root就已经确定了.
}

func (self *worker) commitUncle(work *Work, uncle *types.Header) error {
	hash := uncle.Hash()
	if work.uncles.Has(hash) {
		return fmt.Errorf("uncle not unique")
	}
	if !work.ancestors.Has(uncle.ParentHash) {
		return fmt.Errorf("uncle's parent unknown (%x)", uncle.ParentHash[0:4])
	}
	if work.family.Has(hash) {
		return fmt.Errorf("uncle already in family (%x)", hash)
	}
	work.uncles.Add(uncle.Hash())
	return nil
}

func (env *Work) commitTransactions(mux *event.TypeMux, txs *types.TransactionsByPriceAndNonce, bc *core.BlockChain, coinbase common.Address) {
	gp := new(core.GasPool).AddGas(env.header.GasLimit)	//获得当前区块的gasLimit.

	var coalescedLogs []*types.Log

	for {
		// Retrieve the next transaction and abort if all done
		tx := txs.Peek()	//返回按照交易价格排序后最头的交易。
		if tx == nil {
			break
		}
		// Error may be ignored here. The error has already been checked
		// during transaction acceptance is the transaction pool.
		//
		// We use the eip155 signer regardless of the current hf.
		from, _ := types.Sender(env.signer, tx)
		// Check whether the tx is replay protected. If we're not in the EIP155 hf
		// phase, start ignoring the sender until we do.
		//if tx.Protected() && !env.config.IsEIP155(env.header.Number) {
		//	log.Trace("Ignoring reply protected transaction", "hash", tx.Hash(), "eip155", env.config.EIP155Block)
		//
		//	txs.Pop()
		//	continue
		//}
		// Start executing the transaction
		env.state.Prepare(tx.Hash(), common.Hash{}, env.tcount)

		err, logs := env.commitTransaction(tx, bc, coinbase, gp) //执行交易.

		switch err {
		case core.ErrGasLimitReached:
			//gasLimit影响一个块中能够容纳的交易数量,因此也就能够影响交易完成速率.
			// Pop the current out-of-gas transaction without shifting in the next from the account
			log.Trace("Gas limit exceeded for current block", "sender", from)
			txs.Pop()

		case core.ErrNonceTooLow:
			// New head notification data race between the transaction pool and miner, shift
			log.Trace("Skipping transaction with low nonce", "sender", from, "nonce", tx.Nonce())
			txs.Shift()

		case core.ErrNonceTooHigh:
			// Reorg notification data race between the transaction pool and miner, skip account =
			log.Trace("Skipping account with hight nonce", "sender", from, "nonce", tx.Nonce())
			txs.Pop()

		case nil:
			// Everything ok, collect the logs and shift in the next transaction from the same account
			coalescedLogs = append(coalescedLogs, logs...)
			env.tcount++
			txs.Shift()

		default:
			// Strange error, discard the transaction and get the next in line (note, the
			// nonce-too-high clause will prevent us from executing in vain).
			log.Debug("Transaction failed, account skipped", "hash", tx.Hash(), "err", err)
			txs.Shift()
		}
	}

	if len(coalescedLogs) > 0 || env.tcount > 0 {
		// make a copy, the state caches the logs and these logs get "upgraded" from pending to mined
		// logs by filling in the block hash when the block was mined by the local miner. This can
		// cause a race condition if a log was "upgraded" before the PendingLogsEvent is processed.
		cpy := make([]*types.Log, len(coalescedLogs))
		for i, l := range coalescedLogs {
			cpy[i] = new(types.Log)
			*cpy[i] = *l
		}
		go func(logs []*types.Log, tcount int) {
			if len(logs) > 0 {
				mux.Post(core.PendingLogsEvent{Logs: logs})
			}
			if tcount > 0 {
				mux.Post(core.PendingStateEvent{})
			}
		}(cpy, env.tcount)
	}
}

//执行交易。
func (env *Work) commitTransaction(tx *types.Transaction, bc *core.BlockChain, coinbase common.Address, gp *core.GasPool) (error, []*types.Log) {
	snap := env.state.Snapshot()

	//真正执行交易的函数.
	receipt, _, err := core.ApplyTransaction(env.config, bc, &coinbase, gp, env.state, env.header, tx, env.header.GasUsed, vm.Config{})
	if err != nil {
		env.state.RevertToSnapshot(snap)	//交易如果执行失败，那么回退到执行之间的状态。
		return err, nil
	}
	env.txs = append(env.txs, tx)
	env.receipts = append(env.receipts, receipt)

	return nil, receipt.Logs
}
