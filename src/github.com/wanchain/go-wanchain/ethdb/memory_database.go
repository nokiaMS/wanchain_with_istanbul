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

import (
	"errors"
	"sync"

	"github.com/wanchain/go-wanchain/common"
)

/*
 * This is a test memory database. Do not use for any production it does not get persisted
 */
 //内存数据库.
type MemDatabase struct {
	db   map[string][]byte		//数据库,其实就是 string -> []byte 的map.
	lock sync.RWMutex			//内存数据库操作的读写锁.
}

//生成一个新的内存数据库,其实一个内存数据库就是一个string->[]byte的map.
func NewMemDatabase() (*MemDatabase, error) {	//返回一个MemDatabase的指针及一个error.
	return &MemDatabase{	//创建一个MemDatabase.
		db: make(map[string][]byte),
	}, nil
}
//写记录到数据库.
func (db *MemDatabase) Put(key []byte, value []byte) error {
	db.lock.Lock()
	defer db.lock.Unlock()

	db.db[string(key)] = common.CopyBytes(value)
	return nil
}

//判断数据库中是否有key项.
func (db *MemDatabase) Has(key []byte) (bool, error) {
	db.lock.RLock()
	defer db.lock.RUnlock()

	_, ok := db.db[string(key)]
	return ok, nil
}

//获得key索引的数据项.
func (db *MemDatabase) Get(key []byte) ([]byte, error) {
	db.lock.RLock()
	defer db.lock.RUnlock()

	if entry, ok := db.db[string(key)]; ok {
		return common.CopyBytes(entry), nil
	}
	return nil, errors.New("not found")
}

//获得数据库中的所有key.
func (db *MemDatabase) Keys() [][]byte {
	db.lock.RLock()
	defer db.lock.RUnlock()

	keys := [][]byte{}
	for key := range db.db {
		keys = append(keys, []byte(key))
	}
	return keys
}

/*
func (db *MemDatabase) GetKeys() []*common.Key {
	data, _ := db.Get([]byte("KeyRing"))

	return []*common.Key{common.NewKeyFromBytes(data)}
}
*/

//删除以key为索引的项.
func (db *MemDatabase) Delete(key []byte) error {
	db.lock.Lock()
	defer db.lock.Unlock()

	delete(db.db, string(key))
	return nil
}

//关闭数据库.
func (db *MemDatabase) Close() {}

//创建一个新的batch对象,用于批处理.
func (db *MemDatabase) NewBatch() Batch {
	return &memBatch{db: db}
}

type kv struct{ k, v []byte }

//memBatch对象,用于批量操作.
type memBatch struct {
	db     *MemDatabase
	writes []kv
	size   int
}

//向batch中写入数据.
func (b *memBatch) Put(key, value []byte) error {
	b.writes = append(b.writes, kv{common.CopyBytes(key), common.CopyBytes(value)})
	b.size += len(value)
	return nil
}

//批量写入内存数据库.
func (b *memBatch) Write() error {
	b.db.lock.Lock()
	defer b.db.lock.Unlock()

	for _, kv := range b.writes {
		b.db.db[string(kv.k)] = kv.v
	}
	return nil
}

//返回memBatch的大小.
func (b *memBatch) ValueSize() int {
	return b.size
}
