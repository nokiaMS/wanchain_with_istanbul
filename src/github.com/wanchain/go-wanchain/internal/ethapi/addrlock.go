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

package ethapi

import (
	"sync"

	"github.com/wanchain/go-wanchain/common"
)

//AddrLocker类型.
//AddrLocker中每个账户拥有一个互斥锁,账户之间的互斥锁互不影响.
type AddrLocker struct {
	mu    sync.Mutex	//互斥锁.
	locks map[common.Address]*sync.Mutex	//映射 Address -> *sync.Mutex
}

// lock returns the lock of the given address.
//返回给定地址对应的锁的指针
func (l *AddrLocker) lock(address common.Address) *sync.Mutex {
	l.mu.Lock() 	//本对象加锁.
	defer l.mu.Unlock()	//使用结束后解锁.
	//map为空则创建新map.
	if l.locks == nil {
		l.locks = make(map[common.Address]*sync.Mutex)
	}
	//地址对应的锁为空则创建新锁.
	if _, ok := l.locks[address]; !ok {
		l.locks[address] = new(sync.Mutex)
	}
	//返回地址对应的锁.
	return l.locks[address]
}

// LockAddr locks an account's mutex. This is used to prevent another tx getting the
// same nonce until the lock is released. The mutex prevents the (an identical nonce) from
// being read again during the time that the first transaction is being signed.
//对给定账户加锁.
func (l *AddrLocker) LockAddr(address common.Address) {
	l.lock(address).Lock()
}

// UnlockAddr unlocks the mutex of the given account.
//对给定账户解锁.
func (l *AddrLocker) UnlockAddr(address common.Address) {
	l.lock(address).Unlock()
}
