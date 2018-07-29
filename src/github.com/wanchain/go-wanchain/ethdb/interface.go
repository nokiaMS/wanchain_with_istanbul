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

package ethdb

// Code using batches should try to add this much data to the batch.
// The value was determined empirically.
const IdealBatchSize = 100 * 1024	//已下载但是尚未提交到状态数据库中的交易.

// Putter wraps the database write operation supported by both batches and regular databases.
// 数据库写操作.
type Putter interface {
	Put(key []byte, value []byte) error
}

// Database wraps all database operations. All methods are safe for concurrent use.
//Database数据库接口定义了所有的数据库操作,所有方法都需要能够安全的提供并发操作.
type Database interface {
	Putter								//提供数据库写入接口.
	Get(key []byte) ([]byte, error)		//提供key获得对应的value.
	Has(key []byte) (bool, error)		//判断数据库中key是否存在.
	Delete(key []byte) error			//从数据库中删除key对应的项.
	Close()								//关闭数据库.
	NewBatch() Batch					//创建一个批量操作的batch对象.
}

// Batch is a write-only database that commits changes to its host database
// when Write is called. Batch cannot be used concurrently.
type Batch interface {	//批量操作接口.
	Putter	//写入操作.
	ValueSize() int // amount of data in the batch	batch中的数据个数.
	Write() error	// batch中的Write()函数不支持并发.
}
