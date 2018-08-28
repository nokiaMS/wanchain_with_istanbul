// Copyright 2014 The go-ethereum Authors
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

// Package miner implements Ethereum block creation and mining.
package miner	//miner包负责向外提供一个挖矿得到的新区块.

import (
	"fmt"
	"sync/atomic"

	"github.com/wanchain/go-wanchain/accounts"
	"github.com/wanchain/go-wanchain/common"
	"github.com/wanchain/go-wanchain/consensus"
	"github.com/wanchain/go-wanchain/core"
	"github.com/wanchain/go-wanchain/core/state"
	"github.com/wanchain/go-wanchain/core/types"
	"github.com/wanchain/go-wanchain/eth/downloader"
	"github.com/wanchain/go-wanchain/ethdb"
	"github.com/wanchain/go-wanchain/event"
	"github.com/wanchain/go-wanchain/log"
	"github.com/wanchain/go-wanchain/params"
)

// Backend wraps all methods required for mining.
type Backend interface {
	AccountManager() *accounts.Manager
	BlockChain() *core.BlockChain
	TxPool() *core.TxPool
	ChainDb() ethdb.Database
}

// Miner creates blocks and searches for proof-of-work values.
//Miner类型,作为一个公共对象(首字母大写),向包外提供挖矿相关功能.
type Miner struct {
	mux *event.TypeMux	//用于订阅并接收downloader的事件.

	worker *worker	//一个Miner对象包含一个worker对象.

	coinbase common.Address		//一个Miner对象包含一个coinbase,coinbase表示挖矿得的钱需要放入的账号.
	mining   int32	//表示是否正在挖矿.
	eth      Backend
	engine   consensus.Engine	//共识算法engine.

	canStart    int32 // can start indicates whether we can start the mining operation		//是否允许挖矿的标志(此标志只在块同步开始的时候被设置为0,块同步结束重新设置为1,其余情况均为1.因此从此标志可以看出是否正在进行块同步.)
	shouldStart int32 // should start indicates whether we should start after sync		//块同步完成之后是否开始挖矿.
}

//构造一个新的Miner对象.
func New(eth Backend, config *params.ChainConfig, mux *event.TypeMux, engine consensus.Engine) *Miner {
	miner := &Miner{	//构造一个Miner对象.
		eth:      eth,
		mux:      mux,	//订阅并接收downloader事件.
		engine:   engine,	//共识算法engine.
		worker:   newWorker(config, engine, common.Address{}, eth, mux),	//构造worker对象
		canStart: 1,	//允许挖矿.
	}
	miner.Register(NewCpuAgent(eth.BlockChain(), engine))	//创建一个cpu agent并注册到miner对象中.
	go miner.update()	//启动单独协程注册并处理downloader事件.

	return miner
}

// update keeps track of the downloader events. Please be aware that this is a one shot type of update loop.
// It's entered once and as soon as `Done` or `Failed` has been broadcasted the events are unregistered and
// the loop is exited. This to prevent a major security vuln where external parties can DOS you with blocks
// and halt your mining operation for as long as the DOS continues.

//在Miner对象创建的时候会监听downloader的消息,从而块同步会导致挖矿停止,在块同步结束之后继续挖矿.这次同步完成之后Miner对象就不再监听downloader的事件了.
//因此块同步阻塞挖矿这个过程只在geth启动的时候会出现(因为geth启动会创建Miner对象,Miner对象会创建go update()协程来监控downloader事件),
//在geth启动并第一次块同步完成(不管是正常结束还是失败)之后,后续就不会再出现块同步阻断挖矿过程的现象了.
//这样做的目的主要是为了防止黑客节点持续广播块进行洪泛攻击从而导致节点不能挖矿的攻击行为的发生.
func (self *Miner) update() {
	events := self.mux.Subscribe(downloader.StartEvent{}, downloader.DoneEvent{}, downloader.FailedEvent{})	//订阅downloader的三个事件.
out:
	for ev := range events.Chan() {
		switch ev.Data.(type) {	//根据事件的类型分别处理.
		case downloader.StartEvent:	//如果开始块同步了那么就停止挖矿.
			atomic.StoreInt32(&self.canStart, 0)	//设置不能挖矿标志.
			if self.Mining() {	//如果正在挖矿,那么则停止.
				self.Stop()	//停止挖矿.
				atomic.StoreInt32(&self.shouldStart, 1)	//同步完成之后应该继续挖矿.(正在挖矿,所以块同步结束之后要继续挖矿.)
				log.Info("Mining aborted due to sync")
			}
		case downloader.DoneEvent, downloader.FailedEvent:		//块同步完成或者块同步失败.
			shouldStart := atomic.LoadInt32(&self.shouldStart) == 1	//获得是否应该继续挖矿标记.

			atomic.StoreInt32(&self.canStart, 1)	//修改标记,使得Miner允许挖矿.
			atomic.StoreInt32(&self.shouldStart, 0)	//继续挖矿标识设置为0(块同步完成之后,继续挖矿,恢复shouldStart标记). (如果块同步开始的时候正在挖矿,那么先停止挖矿进行块同步;等到块同步结束(完成或者失败)再继续开始挖矿.)
			if shouldStart {
				self.Start(self.coinbase)	//开始挖矿.
			}
			// unsubscribe. we're only interested in this event once
			events.Unsubscribe()	//不再订阅downloader的消息.
			// stop immediately and ignore all further pending events
			break out	//跳出循环, update()协程结束.
		}
	}
}

//开始挖矿.
//coinbase: 使用此账号挖矿.
func (self *Miner) Start(coinbase common.Address) {
	atomic.StoreInt32(&self.shouldStart, 1)
	self.worker.setEtherbase(coinbase)	//设置worker对象的coinbase.
	self.coinbase = coinbase			//设置miner对象的coinbase.

	//正在进行块同步,不能开始挖矿.
	if atomic.LoadInt32(&self.canStart) == 0 {
		log.Info("Network syncing, will start miner afterwards")
		return
	}
	atomic.StoreInt32(&self.mining, 1)		//设置正在挖矿标记.

	log.Info("Starting mining operation")
	self.worker.start()		//开始挖矿.
	self.worker.commitNewWork()		//组装新的work给agent,agent处理完成之后会把被共识算法确认好的block返回给worker.
}

//停止挖矿.
func (self *Miner) Stop() {
	self.worker.stop()	//停止挖矿.
	atomic.StoreInt32(&self.mining, 0)		//更新正在挖矿标志为否.
	atomic.StoreInt32(&self.shouldStart, 0)	//设置shouldStart标志位0.
}

//注册挖矿代理, agent:要注册的挖矿代理对象.
func (self *Miner) Register(agent Agent) {
	if self.Mining() {	//没有挖矿的时候不会执行agent start.挖矿后如果走到此处才会执行agent start.
		agent.Start()	//开始挖矿.
	}
	self.worker.register(agent)		//注册agent到worker中.
}

//解除注册agent.
func (self *Miner) Unregister(agent Agent) {
	self.worker.unregister(agent)	//把agent从worker的agent列表中删除.
}

//是否正在挖矿.返回true表示正在挖矿,false表示没有正在挖矿.
func (self *Miner) Mining() bool {
	return atomic.LoadInt32(&self.mining) > 0	//mining大于0表示正在挖矿,0表示没有正在挖矿.	LoadInt32表示原子读取int32的操作.
}

func (self *Miner) HashRate() (tot int64) {
	if pow, ok := self.engine.(consensus.PoW); ok {
		tot += int64(pow.Hashrate())
	}
	// do we care this might race? is it worth we're rewriting some
	// aspects of the worker/locking up agents so we can get an accurate
	// hashrate?
	for agent := range self.worker.agents {
		if _, ok := agent.(*CpuAgent); !ok {
			tot += agent.GetHashRate()
		}
	}
	return
}

//设置extraData.
func (self *Miner) SetExtra(extra []byte) error {
	if uint64(len(extra)) > params.MaximumExtraDataSize {
		return fmt.Errorf("Extra exceeds max length. %d > %v", len(extra), params.MaximumExtraDataSize)
	}
	self.worker.setExtra(extra)		//设置extraData
	return nil
}

//获得当前pending块及相关的状态.
// Pending returns the currently pending block and associated state.
func (self *Miner) Pending() (*types.Block, *state.StateDB) {
	return self.worker.pending()  //获得当前pending块及相关的状态.
}

// PendingBlock returns the currently pending block.
//
// Note, to access both the pending block and the pending state
// simultaneously, please use Pending(), as the pending state can
// change between multiple method calls
//获得当前pending的块(所谓的pending block就是已经组装好了但是还在确认中,还没有上链的块.)
func (self *Miner) PendingBlock() *types.Block {
	return self.worker.pendingBlock()	//获得pending block.
}

//设置挖矿使用的coinbase.
func (self *Miner) SetEtherbase(addr common.Address) {
	self.coinbase = addr	//设置Miner的coinbase.
	self.worker.setEtherbase(addr)		//设置worker的coinbase.
}
