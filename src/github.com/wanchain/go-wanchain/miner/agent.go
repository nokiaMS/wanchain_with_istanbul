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
	"sync"

	"sync/atomic"

	"github.com/wanchain/go-wanchain/consensus"
	"github.com/wanchain/go-wanchain/log"
)

//cpu代理。
type CpuAgent struct {
	mu sync.Mutex	//互斥锁.

	workCh        chan *Work
	stop          chan struct{}	//channel,接收stop消息.
	quitCurrentOp chan struct{}	//通道:退出当前操作.
	returnCh      chan<- *Result

	chain  consensus.ChainReader	//访问本地区块链的不完备方法集合,用于header验证及uncle验证.
	engine consensus.Engine		//共识算法引擎。

	isMining int32 // isMining indicates whether the agent is currently mining	//当前agent是否正在挖矿。
}

//构建cpu挖矿代理.
func NewCpuAgent(chain consensus.ChainReader, engine consensus.Engine) *CpuAgent {
	miner := &CpuAgent{
		chain:  chain,						//区块链。
		engine: engine,						//共识算法engine。
		stop:   make(chan struct{}, 1),	//异步通道，接收stop消息。
		workCh: make(chan *Work, 1),		//异步通道，接收work类型消息。
	}
	return miner
}

//返回Work指针类型的通道。
func (self *CpuAgent) Work() chan<- *Work            { return self.workCh }
func (self *CpuAgent) SetReturnCh(ch chan<- *Result) { self.returnCh = ch }

//停止agent。
func (self *CpuAgent) Stop() {
	if !atomic.CompareAndSwapInt32(&self.isMining, 1, 0) {
		return // agent already stopped
	}
	self.stop <- struct{}{}		//像stop channel发送消息。
done:
	// Empty work channel	//清空work channel.
	for {
		select {
		case <-self.workCh:
		default:
			break done	//break <label>的意思是跳出for循环到<label处，但是跳出后就不再再次执行for循环了。>
		}
	}
}

//启动挖矿.
func (self *CpuAgent) Start() {
	//如果没有启动挖矿，那么启动；如果已经启动了，那么不再再次启动。
	if !atomic.CompareAndSwapInt32(&self.isMining, 0, 1) {
		return // agent already started
	}
	//启动事件监控。
	go self.update()
}

//事件监控.
func (self *CpuAgent) update() {
out:
	for {
		select {
		case work := <-self.workCh:
			self.mu.Lock()
			if self.quitCurrentOp != nil {	//需要退出当前通道,则退出.
				close(self.quitCurrentOp)	//关闭quitCurrentOp通道.
			}
			self.quitCurrentOp = make(chan struct{})
			go self.mine(work, self.quitCurrentOp)
			self.mu.Unlock()
		case <-self.stop:	//收到停止消息.
			self.mu.Lock()
			if self.quitCurrentOp != nil {	//需要退出当前操作,则退出.
				close(self.quitCurrentOp)	//关闭quitCurrentOp通道.
				self.quitCurrentOp = nil
			}
			self.mu.Unlock()
			break out	//跳出for循环,协程退出.
		}
	}
}

func (self *CpuAgent) mine(work *Work, stop <-chan struct{}) {
	if result, err := self.engine.Seal(self.chain, work.Block, stop); result != nil {
		log.Info("Successfully sealed new block", "number", result.Number(), "hash", result.Hash())
		self.returnCh <- &Result{work, result}
	} else {
		if err != nil {
			log.Warn("Block sealing failed", "err", err)
		}
		self.returnCh <- nil
	}
}

func (self *CpuAgent) GetHashRate() int64 {
	if pow, ok := self.engine.(consensus.PoW); ok {
		return int64(pow.Hashrate())
	}
	return 0
}
