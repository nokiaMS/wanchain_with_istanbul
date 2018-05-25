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

package core

import (
	"github.com/wanchain/go-wanchain/common"
	"github.com/wanchain/go-wanchain/consensus/istanbul"
	"fmt"
)

//开始共识算法流程.
//启动istanbul consensus状态机。  
// Start implements core.Engine.Start
func (c *core) Start() error {
	//开始一个新的consensus round.
	// Start a new round from last sequence + 1
	c.startNewRound(common.Big0)	//初始round为0.

	// Tests will handle events itself, so we have to make subscribeEvents()
	// be able to call in test.
	c.subscribeEvents()	//订阅istanbul consensus相关事件.
	go c.handleEvents()		//启动一个单独的线程进行事件处理.

	return nil
}

// Stop implements core.Engine.Stop
func (c *core) Stop() error {
	c.stopTimer()
	c.unsubscribeEvents()

	// Make sure the handler goroutine exits
	c.handlerWg.Wait()
	return nil
}

// ----------------------------------------------------------------------------

// Subscribe both internal and external events
//ibft订阅外部事件和内部事件.
func (c *core) subscribeEvents() {
	c.events = c.backend.EventMux().Subscribe(
		// external events
		istanbul.RequestEvent{},	//proposal相关事件.
		istanbul.MessageEvent{},	//istanbul engine相关事件.
		// internal events
		backlogEvent{},				//订阅日志事件.
	)
	c.timeoutSub = c.backend.EventMux().Subscribe(	//订阅超时事件.
		timeoutEvent{},
	)
	c.finalCommittedSub = c.backend.EventMux().Subscribe(	//订阅proposal committed事件.
		istanbul.FinalCommittedEvent{},
	)
}

//解除订阅.
// Unsubscribe all events
func (c *core) unsubscribeEvents() {
	c.events.Unsubscribe()
	c.timeoutSub.Unsubscribe()
	c.finalCommittedSub.Unsubscribe()
}

//处理事件.
func (c *core) handleEvents() {
	// Clear state
	defer func() {
		c.current = nil
		c.handlerWg.Done()
	}()

	c.handlerWg.Add(1)

	for {
		select {	//让线程在多个channel操作上等待.
		case event, ok := <-c.events.Chan():	//engine事件.
			if !ok {
				return
			}
			// A real event arrived, process interesting content
			//engine事件分为三类.
			switch ev := event.Data.(type) {
			case istanbul.RequestEvent:	//proposal相关事件.
				fmt.Println("gx1: events RequestEvent")
				r := &istanbul.Request{
					Proposal: ev.Proposal,
				}
				err := c.handleRequest(r)
				if err == errFutureMessage {
					c.storeRequestMsg(r)
				}
			case istanbul.MessageEvent:	//engine通信相关事件.
				fmt.Println("gx1: events MessageEvent")
				if err := c.handleMsg(ev.Payload); err == nil {
					c.backend.Gossip(c.valSet, ev.Payload)
				}
			case backlogEvent:		//backlog相关事件.
				fmt.Println("gx1: events backlogEvent")
				// No need to check signature for internal messages
				if err := c.handleCheckedMsg(ev.msg, ev.src); err == nil {
					p, err := ev.msg.Payload()
					if err != nil {
						c.logger.Warn("Get message payload failed", "err", err)
						continue
					}
					c.backend.Gossip(c.valSet, p)
				}
			}
		case _, ok := <-c.timeoutSub.Chan():	//超时事件.
			fmt.Println("gx1: timeoutSub")
			if !ok {
				return
			}
			c.handleTimeoutMsg()
		case event, ok := <-c.finalCommittedSub.Chan():	//proposal最终提交事件.
			fmt.Println("gx1: finalCommittedSub")
			if !ok {
				return
			}
			switch event.Data.(type) {
			case istanbul.FinalCommittedEvent:
				c.handleFinalCommitted()
			}
		}
	}
}

// sendEvent sends events to mux
func (c *core) sendEvent(ev interface{}) {
	c.backend.EventMux().Post(ev)
}

func (c *core) handleMsg(payload []byte) error {
	fmt.Println("gx1: handleMsg 1")
	logger := c.logger.New()

	// Decode message and check its signature
	msg := new(message)
	if err := msg.FromPayload(payload, c.validateFn); err != nil {
		logger.Error("Failed to decode message from payload", "err", err)
		return err
	}

	// Only accept message if the address is valid
	_, src := c.valSet.GetByAddress(msg.Address)
	if src == nil {
		logger.Error("Invalid address in message", "msg", msg)
		return istanbul.ErrUnauthorizedAddress
	}

	return c.handleCheckedMsg(msg, src)
}

func (c *core) handleCheckedMsg(msg *message, src istanbul.Validator) error {
	logger := c.logger.New("address", c.address, "from", src)

	// Store the message if it's a future message
	testBacklog := func(err error) error {
		if err == errFutureMessage {
			c.storeBacklog(msg, src)
		}

		return err
	}

	switch msg.Code {
	case msgPreprepare:
		fmt.Println("gx1: msgPreprepare step.")
		return testBacklog(c.handlePreprepare(msg, src))
	case msgPrepare:
		fmt.Println("gx1: msgPrepare step.")
		return testBacklog(c.handlePrepare(msg, src))
	case msgCommit:
		fmt.Println("gx1: msgCommit step.")
		return testBacklog(c.handleCommit(msg, src))
	case msgRoundChange:
		fmt.Println("gx1: msgRoundChange step.")
		return testBacklog(c.handleRoundChange(msg, src))
	default:
		fmt.Println("gx1: default step.")
		logger.Error("Invalid message", "msg", msg)
	}

	return errInvalidMessage
}

func (c *core) handleTimeoutMsg() {
	// If we're not waiting for round change yet, we can try to catch up
	// the max round with F+1 round change message. We only need to catch up
	// if the max round is larger than current round.
	if !c.waitingForRoundChange {
		maxRound := c.roundChangeSet.MaxRound(c.valSet.F() + 1)
		if maxRound != nil && maxRound.Cmp(c.current.Round()) > 0 {
			c.sendRoundChange(maxRound)
			return
		}
	}

	lastProposal, _ := c.backend.LastProposal()
	if lastProposal != nil && lastProposal.Number().Cmp(c.current.Sequence()) >= 0 {
		c.logger.Trace("round change timeout, catch up latest sequence", "number", lastProposal.Number().Uint64())
		c.startNewRound(common.Big0)
	} else {
		c.sendNextRoundChange()
	}
}
