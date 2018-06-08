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

package gasprice

import (
	"context"
	"math/big"
	"sort"
	"sync"

	"github.com/wanchain/go-wanchain/common"
	"github.com/wanchain/go-wanchain/internal/ethapi"
	"github.com/wanchain/go-wanchain/params"
	"github.com/wanchain/go-wanchain/rpc"
)

//最大的gas price值. 500 * Shannon.
var maxPrice = big.NewInt(0).Mul(big.NewInt(500 * params.Shannon),params.WanGasTimesFactor)

type Config struct {
	Blocks     int
	Percentile int
	Default    *big.Int `toml:",omitempty"`
}

// Oracle recommends gas prices based on the content of recent
// blocks. Suitable for both light and full clients.
//oracle翻译成权威,智囊.
//Oracle对象会基于最近的几个块给出建议的gasprice. 对轻量节点和全节点都适用.
type Oracle struct {
	backend   ethapi.Backend	//后端api backend.
	lastHead  common.Hash
	lastPrice *big.Int
	cacheLock sync.RWMutex
	fetchLock sync.Mutex

	checkBlocks, maxEmpty, maxBlocks int
	percentile int
}

// NewOracle returns a new oracle.
//创建一个新的gasprice oracle.
func NewOracle(backend ethapi.Backend, params Config) *Oracle {
	blocks := params.Blocks
	if blocks < 1 {
		blocks = 1
	}
	percent := params.Percentile
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	return &Oracle{
		backend:     backend,
		lastPrice:   params.Default,	//如果gwan命令没有配置这个值,那么使用default config的值,如果命令行参数中已经配置了这个值,那么使用命令行中的这个值.
		checkBlocks: blocks,
		maxEmpty:    blocks / 2,
		maxBlocks:   blocks * 5,
		percentile:  percent,
	}
}

// SuggestPrice returns the recommended gas price.
//计算并返回gas price.
//SuggestPrice计算price的方法如下:
//    1. 如果区块链没有更新,那么使用cache中的price.
//    2. 如果检测到区块链中有了更新(添加了新的块)那么则重新计算price,计算的方法如下:
//            选取从链头开始最多10个(可配置)块,然后获得块中所有交易的price,排序后取中间值作为suggest gas price,并同时更新cache.
func (gpo *Oracle) SuggestPrice(ctx context.Context) (*big.Int, error) {
	gpo.cacheLock.RLock()	//cacheLock及lastHead和lastPrice能够缓存gpo的最后的状态,这样避免了重复的计算suggest gas price的过程,提高了效率.
	lastHead := gpo.lastHead
	lastPrice := gpo.lastPrice
	gpo.cacheLock.RUnlock()

	head, _ := gpo.backend.HeaderByNumber(ctx, rpc.LatestBlockNumber)	//返回链头块的header.(如果链有更新,那么自然返回了新的链头.)
	headHash := head.Hash()
	if headHash == lastHead {	//如果缓存的header就是链头块,那么直接返回缓存的lastPrice.
		return lastPrice, nil
	}

	//能够走到这里,说明链头有了更新,cache失效了,所以需要重新计算.
	gpo.fetchLock.Lock()
	defer gpo.fetchLock.Unlock()

	//再次读一次lasthead,因为这个域可能在别的函数中被更新了.
	// try checking the cache again, maybe the last fetch fetched what we need
	gpo.cacheLock.RLock()
	lastHead = gpo.lastHead
	lastPrice = gpo.lastPrice
	gpo.cacheLock.RUnlock()
	if headHash == lastHead {
		return lastPrice, nil
	}

	blockNum := head.Number.Uint64()
	ch := make(chan getBlockPricesResult, gpo.checkBlocks)
	sent := 0
	exp := 0
	var txPrices []*big.Int		//用来顺序存储遍历的所有块的所有的交易的gas price.
	//遍历从链头开始的最多10个块(如果当前链不够10个块,那么有几个遍历几个.)
	for sent < gpo.checkBlocks && blockNum > 0 {	//blockNum >0表示非创世块,即在只有创世块的时候不会走到这里.
		go gpo.getBlockPrices(ctx, blockNum, ch)	//启动一个单独的协程来获得每个块中交易的gas price列表,并把结果缓存在ch通道中.
		sent++
		exp++
		blockNum--
	}
	maxEmpty := gpo.maxEmpty	//默认值得话maxEmpty应该为10/2 = 5

	//exp为实际遍历了的块的个数.
	for exp > 0 {	//只有创世块的时候exp为0,因此也不会走到这里.
		res := <-ch
		if res.err != nil {
			return lastPrice, res.err
		}
		exp--	//exp为0的时候退出循环.
		if len(res.prices) > 0 {
			txPrices = append(txPrices, res.prices...)
			continue
		}
		if maxEmpty > 0 {
			maxEmpty--
			continue
		}
		if blockNum > 0 && sent < gpo.maxBlocks {
			go gpo.getBlockPrices(ctx, blockNum, ch)
			sent++
			exp++
			blockNum--
		}
	}
	price := lastPrice //如果gwan命令行参数设置了这个值,那么最早的时候的gas price就是gwan参数中的gas price.
	//计算建议的gas price价格: 计算方法是从排好序的价格中选取中间的一个价格.
	if len(txPrices) > 0 {
		sort.Sort(bigIntArray(txPrices))
		price = txPrices[(len(txPrices)-1)*gpo.percentile/100] //默认的percentile为50%.
	}
	if price.Cmp(maxPrice) > 0 {	//如果价格比maxprice还要大那么就返回max price.
		price = new(big.Int).Set(maxPrice)
	}

	//更新cache.
	gpo.cacheLock.Lock()
	gpo.lastHead = headHash
	gpo.lastPrice = price
	gpo.cacheLock.Unlock()
	return price, nil
}

//存储每个块中的所有交易对应的price.
type getBlockPricesResult struct {
	prices []*big.Int	//块中所有交易的prices.
	err    error	//在遍历块中出现的错误.
}

// getLowestPrice calculates the lowest transaction gas price in a given block
// and sends it to the result channel. If the block is empty, price is nil.
//获得blockNum对应的块中的所有交易的price,以getBlockPricesResult对象的形式返回结果.到通道ch中.
func (gpo *Oracle) getBlockPrices(ctx context.Context, blockNum uint64, ch chan getBlockPricesResult) {
	block, err := gpo.backend.BlockByNumber(ctx, rpc.BlockNumber(blockNum))	//获得块的信息.
	if block == nil {	//如果没有number对应的块,那么直接返回一个空的结果.
		ch <- getBlockPricesResult{nil, err}
		return
	}
	txs := block.Transactions()		//获得块中的交易.
	prices := make([]*big.Int, len(txs))	//构造交易个数大小的prices数组来存储每个交易的gas price.
	for i, tx := range txs {	//一次存储每个交易的price.
		prices[i] = tx.GasPrice()
	}
	ch <- getBlockPricesResult{prices, nil}	//返回结果.
}

type bigIntArray []*big.Int

func (s bigIntArray) Len() int           { return len(s) }
func (s bigIntArray) Less(i, j int) bool { return s[i].Cmp(s[j]) < 0 }
func (s bigIntArray) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
