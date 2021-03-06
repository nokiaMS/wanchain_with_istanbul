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

package downloader

//空结构体通常作为消息在通道中传递.
type DoneEvent struct{}	//downloader DoneEvent消息, downloader与peer同步结束.
type StartEvent struct{}	//downloader StartEvent消息	,downloader开始与peer同步的时候会发送这个消息.
type FailedEvent struct{ Err error }	//downloader FailedEvent消息, downloader与peer同步失败.
