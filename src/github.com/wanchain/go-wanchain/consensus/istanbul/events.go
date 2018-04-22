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

package istanbul

//提出一个proposal的时候产生此消息.
// RequestEvent is posted to propose a proposal
type RequestEvent struct {
	Proposal Proposal
}

//MessageEvent用于Istanbul engine的通信.
// MessageEvent is posted for Istanbul engine communication
type MessageEvent struct {
	Payload []byte
}

//当proposal被提交的时候发出此事件.
// FinalCommittedEvent is posted when a proposal is committed
type FinalCommittedEvent struct {
}
