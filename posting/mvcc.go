/*
 * Copyright 2017-2018 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package posting

import (
	"bytes"
	"encoding/hex"
	"math"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dgraph-io/badger/v3"
	"github.com/dgraph-io/badger/v3/skl"
	"github.com/dgraph-io/badger/v3/y"
	"github.com/dgraph-io/dgo/v210/protos/api"
	"github.com/dgraph-io/dgraph/protos/pb"
	"github.com/dgraph-io/dgraph/x"
	"github.com/dgraph-io/ristretto/z"
	"github.com/golang/glog"
	"github.com/pkg/errors"
)

type pooledKeys struct {
	// keysCh is populated with batch of 64 keys that needs to be rolled up during reads
	keysCh chan *[][]byte
	// keysPool is sync.Pool to share the batched keys to rollup.
	keysPool *sync.Pool
}

// incrRollupi is used to batch keys for rollup incrementally.
type incrRollupi struct {
	// We are using 2 priorities with now, idx 0 represents the high priority keys to be rolled up
	// while idx 1 represents low priority keys to be rolled up.
	priorityKeys []*pooledKeys
	count        uint64
}

var (
	// ErrTsTooOld is returned when a transaction is too old to be applied.
	ErrTsTooOld = errors.Errorf("Transaction is too old")
	// ErrInvalidKey is returned when trying to read a posting list using
	// an invalid key (e.g the key to a single part of a larger multi-part list).
	ErrInvalidKey = errors.Errorf("cannot read posting list using multi-part list key")

	// IncrRollup is used to batch keys for rollup incrementally.
	IncrRollup = &incrRollupi{
		priorityKeys: make([]*pooledKeys, 2),
	}
)

func init() {
	x.AssertTrue(len(IncrRollup.priorityKeys) == 2)
	for i := range IncrRollup.priorityKeys {
		IncrRollup.priorityKeys[i] = &pooledKeys{
			keysCh: make(chan *[][]byte, 16),
			keysPool: &sync.Pool{
				New: func() interface{} {
					return new([][]byte)
				},
			},
		}
	}
}

// rollupKey takes the given key's posting lists, rolls it up and writes back to badger
func (ir *incrRollupi) rollupKey(sl *skl.Skiplist, key []byte) error {
	l, err := GetNoStore(key, math.MaxUint64)
	if err != nil {
		return err
	}

	kvs, err := l.Rollup(nil)
	if err != nil {
		return err
	}

	// If we do a rollup, we typically won't need to update the key in cache.
	// The only caveat is that the key written by rollup would be written at +1
	// timestamp, hence bumping the latest TS for the key by 1. The cache should
	// understand that.
	const N = uint64(1000)
	if glog.V(2) {
		if count := atomic.AddUint64(&ir.count, 1); count%N == 0 {
			glog.V(2).Infof("Rolled up %d keys", count)
		}
	}

	for _, kv := range kvs {
		vs := y.ValueStruct{
			Value: kv.Value,
		}
		if len(kv.UserMeta) > 0 {
			vs.UserMeta = kv.UserMeta[0]
		}
		switch vs.UserMeta {
		case BitCompletePosting, BitEmptyPosting:
			vs.Meta = badger.BitDiscardEarlierVersions
		default:
		}
		sl.Put(y.KeyWithTs(kv.Key, kv.Version), vs)
	}

	return nil
}

// TODO: When the opRollup is not running the keys from keysPool of ir are dropped. Figure out some
// way to handle that.
func (ir *incrRollupi) addKeyToBatch(key []byte, priority int) {
	rki := ir.priorityKeys[priority]
	batch := rki.keysPool.Get().(*[][]byte)
	*batch = append(*batch, key)
	if len(*batch) < 16 {
		rki.keysPool.Put(batch)
		return
	}

	select {
	case rki.keysCh <- batch:
	default:
		// Drop keys and build the batch again. Lossy behavior.
		*batch = (*batch)[:0]
		rki.keysPool.Put(batch)
	}
}

// Process will rollup batches of 64 keys in a go routine.
func (ir *incrRollupi) Process(closer *z.Closer) {
	defer closer.Done()

	m := make(map[uint64]int64) // map hash(key) to ts. hash(key) to limit the size of the map.

	limiter := time.NewTicker(time.Millisecond)
	defer limiter.Stop()

	cleanupTick := time.NewTicker(5 * time.Minute)
	defer cleanupTick.Stop()

	baseTick := time.NewTicker(500 * time.Millisecond)
	defer baseTick.Stop()

	const initSize = 1 << 20
	sl := skl.NewGrowingSkiplist(initSize)

	handover := func() {
		if sl.Empty() {
			return
		}
		if err := x.RetryUntilSuccess(3600, time.Second, func() error {
			return pstore.HandoverSkiplist(sl, nil)
		}); err != nil {
			glog.Errorf("Rollup handover skiplist returned error: %v\n", err)
		}
		// If we have an error, the skiplist might not be safe to use still. So,
		// just create a new one always.
		sl = skl.NewGrowingSkiplist(initSize)
	}
	doRollup := func(batch *[][]byte, priority int) {
		currTs := time.Now().Unix()
		for _, key := range *batch {
			hash := z.MemHash(key)
			if elem := m[hash]; currTs-elem < 10 {
				continue
			}
			// Key not present or Key present but last roll up was more than 10 sec ago.
			// Add/Update map and rollup.
			m[hash] = currTs
			if err := ir.rollupKey(sl, key); err != nil {
				glog.Warningf("Error %v rolling up key %v\n", err, key)
			}
		}
		*batch = (*batch)[:0]
		ir.priorityKeys[priority].keysPool.Put(batch)
	}

	var ticks int
	for {
		select {
		case <-closer.HasBeenClosed():
			return
		case <-cleanupTick.C:
			currTs := time.Now().UnixNano()
			for hash, ts := range m {
				// Remove entries from map which have been there for there more than 10 seconds.
				if currTs-ts >= int64(10*time.Second) {
					delete(m, hash)
				}
			}
		case <-baseTick.C:
			// Pick up incomplete batches from the keysPool, and process them.
			// This handles infrequent writes case, where a batch might take a
			// long time to fill up.
			batch := ir.priorityKeys[0].keysPool.Get().(*[][]byte)
			if len(*batch) > 0 {
				doRollup(batch, 0)
			} else {
				ir.priorityKeys[0].keysPool.Put(batch)
			}
			ticks++
			if ticks%4 == 0 { // base tick is every 500ms. This is 2s.
				handover()
			}
		case batch := <-ir.priorityKeys[0].keysCh:
			// P0 keys are high priority keys. They have more than a threshold number of deltas.
			doRollup(batch, 0)
			// We don't need a limiter here as we don't expect to call this function frequently.
		case batch := <-ir.priorityKeys[1].keysCh:
			doRollup(batch, 1)
			// throttle to 1 batch = 16 rollups per 1 ms.
			<-limiter.C
		}
	}
}

// ShouldAbort returns whether the transaction should be aborted.
func (txn *Txn) ShouldAbort() bool {
	if txn == nil {
		return false
	}
	return atomic.LoadUint32(&txn.shouldAbort) > 0
}

func (txn *Txn) addConflictKey(conflictKey uint64) {
	txn.Lock()
	defer txn.Unlock()
	if txn.conflicts == nil {
		txn.conflicts = make(map[uint64]struct{})
	}
	if conflictKey > 0 {
		txn.conflicts[conflictKey] = struct{}{}
	}
}

func (txn *Txn) ReadKeys() map[uint64]struct{} {
	txn.Lock()
	defer txn.Unlock()
	return txn.cache.readKeys
}

func (txn *Txn) Deltas() map[string][]byte {
	txn.Lock()
	defer txn.Unlock()
	return txn.cache.deltas
}

// FillContext updates the given transaction context with data from this transaction.
func (txn *Txn) FillContext(ctx *api.TxnContext, gid uint32) {
	txn.Lock()
	ctx.StartTs = txn.StartTs

	for key := range txn.conflicts {
		// We don'txn need to send the whole conflict key to Zero. Solving #2338
		// should be done by sending a list of mutating predicates to Zero,
		// along with the keys to be used for conflict detection.
		fps := strconv.FormatUint(key, 36)
		ctx.Keys = append(ctx.Keys, fps)
	}
	ctx.Keys = x.Unique(ctx.Keys)

	txn.Unlock()
	txn.cache.fillPreds(ctx, gid)
}

// ToSkiplist replaces CommitToDisk. ToSkiplist creates a Badger usable Skiplist from the Txn, so
// it can be passed over to Badger after commit. This only stores deltas to the commit timestamps.
// It does not try to generate a state. State generation is done via rollups, which happen when a
// snapshot is created.  Don't call this for schema mutations. Directly commit them.
func (txn *Txn) ToSkiplist() error {
	cache := txn.cache
	cache.Lock()
	defer cache.Unlock()

	var keys []string
	for key := range cache.deltas {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	// Add these keys to be rolled up after we're done writing them to Badger.
	// Some full text indices could easily gain hundreds of thousands of
	// mutations, while never being read. We do want to capture those cases.
	// Update: We roll up the keys in oracle.DeleteTxnsAndRollupKeys, which is a
	// callback that happens after skip list gets handed over to Badger.

	b := skl.NewBuilder(1 << 10)
	for _, key := range keys {
		k := []byte(key)
		data := cache.deltas[key]
		if len(data) == 0 {
			continue
		}

		if err := badger.ValidEntry(pstore, k, data); err != nil {
			glog.Errorf("Invalid Entry. len(key): %d len(val): %d\n", len(k), len(data))
			continue
		}
		b.Add(y.KeyWithTs(k, math.MaxUint64),
			y.ValueStruct{
				Value:    data,
				UserMeta: BitDeltaPosting,
			})
	}
	txn.sl = b.Skiplist()
	return nil
}

func ResetCache() {
	lCache.Clear()
}

// RemoveCachedKeys will delete the cached list by this txn.
func (txn *Txn) UpdateCachedKeys(commitTs uint64) {
	if txn == nil || txn.cache == nil {
		return
	}
	x.AssertTrue(commitTs > 0)
	for key := range txn.cache.deltas {
		lCache.SetIfPresent([]byte(key), commitTs, 0)
	}
}

func unmarshalOrCopy(plist *pb.PostingList, item *badger.Item) error {
	if plist == nil {
		return errors.Errorf("cannot unmarshal value to a nil posting list of key %s",
			hex.Dump(item.Key()))
	}

	return item.Value(func(val []byte) error {
		if len(val) == 0 {
			// empty pl
			return nil
		}
		return plist.Unmarshal(val)
	})
}

// ReadPostingList constructs the posting list from the disk using the passed iterator.
// Use forward iterator with allversions enabled in iter options.
// key would now be owned by the posting list. So, ensure that it isn't reused elsewhere.
func ReadPostingList(key []byte, it *badger.Iterator) (*List, error) {
	// Previously, ReadPostingList was not checking that a multi-part list could only
	// be read via the main key. This lead to issues during rollup because multi-part
	// lists ended up being rolled-up multiple times. This issue was caught by the
	// uid-set Jepsen test.
	pk, err := x.Parse(key)
	if err != nil {
		return nil, errors.Wrapf(err, "while reading posting list with key [%v]", key)
	}
	if pk.HasStartUid {
		// Trying to read a single part of a multi part list. This type of list
		// should be read using using the main key because the information needed
		// to access the whole list is stored there.
		// The function returns a nil list instead. This is safe to do because all
		// public methods of the List object are no-ops and the list is being already
		// accessed via the main key in the places where this code is reached (e.g rollups).
		return nil, ErrInvalidKey
	}

	l := new(List)
	l.key = key
	l.plist = new(pb.PostingList)

	// We use the following block of code to trigger incremental rollup on this key.
	deltaCount := 0
	defer func() {
		if deltaCount > 0 {
			// If deltaCount is high, send it to high priority channel instead.
			if deltaCount > 500 {
				IncrRollup.addKeyToBatch(key, 0)
			} else {
				IncrRollup.addKeyToBatch(key, 1)
			}
		}
	}()

	// Iterates from highest Ts to lowest Ts
	for it.Valid() {
		item := it.Item()
		if !bytes.Equal(item.Key(), l.key) {
			break
		}
		l.maxTs = x.Max(l.maxTs, item.Version())
		if item.IsDeletedOrExpired() {
			// Don't consider any more versions.
			break
		}

		switch item.UserMeta() {
		case BitEmptyPosting:
			l.minTs = item.Version()
			return l, nil
		case BitCompletePosting:
			if err := unmarshalOrCopy(l.plist, item); err != nil {
				return nil, err
			}
			l.minTs = item.Version()

			// No need to do Next here. The outer loop can take care of skipping
			// more versions of the same key.
			return l, nil
		case BitDeltaPosting:
			err := item.Value(func(val []byte) error {
				pl := &pb.PostingList{}
				if err := pl.Unmarshal(val); err != nil {
					return err
				}
				pl.CommitTs = item.Version()
				for _, mpost := range pl.Postings {
					// commitTs, startTs are meant to be only in memory, not
					// stored on disk.
					mpost.CommitTs = item.Version()
				}
				if l.mutationMap == nil {
					l.mutationMap = make(map[uint64]*pb.PostingList)
				}
				l.mutationMap[pl.CommitTs] = pl
				return nil
			})
			if err != nil {
				return nil, err
			}
			deltaCount++
		case BitSchemaPosting:
			return nil, errors.Errorf(
				"Trying to read schema in ReadPostingList for key: %s", hex.Dump(key))
		default:
			return nil, errors.Errorf(
				"Unexpected meta: %d for key: %s", item.UserMeta(), hex.Dump(key))
		}
		if item.DiscardEarlierVersions() {
			break
		}
		it.Next()
	}
	return l, nil
}

func getNew(key []byte, pstore *badger.DB, readTs uint64) (*List, error) {
	if pstore.IsClosed() {
		return nil, badger.ErrDBClosed
	}

	var seenTs uint64
	// We use badger subscription to invalidate the cache. For every write we make the value
	// corresponding to the key in the cache to nil. So, if we get some non-nil value from the cache
	// then it means that no  writes have happened after the last set of this key in the cache.
	if val, ok := lCache.Get(key); ok {
		switch val := val.(type) {
		case *List:
			l := val
			// l.maxTs can be greater than readTs. We might have the latest
			// version cached, while readTs is looking for an older version.
			if l != nil && l.maxTs <= readTs {
				l.RLock()
				lCopy := copyList(l)
				l.RUnlock()
				return lCopy, nil
			}

		case uint64:
			seenTs = val
		}
	} else {
		// The key wasn't found in cache. So, we set the key upfront.  This
		// gives it a chance to register in the cache, so it can capture any new
		// writes comming from commits. Once we
		// retrieve the value from Badger, we do an update if the key is already
		// present in the cache.
		// We must guarantee that the cache contains the latest version of the
		// key. This mechanism avoids the following race condition:
		// 1. We read from Badger at Ts 10.
		// 2. New write comes in for the key at Ts 12. The key isn't in cache,
		// so this write doesn't get registered with the cache.
		// 3. Cache set the value read from Badger at Ts10.
		//
		// With this Set then Update mechanism, before we read from Badger, we
		// already set the key in cache. So, any new writes coming in would get
		// registered with cache correctly, before we update the value.
		lCache.Set(key, uint64(1), 0)
	}

	txn := pstore.NewTransactionAt(readTs, false)
	defer txn.Discard()

	// When we do rollups, an older version would go to the top of the LSM tree, which can cause
	// issues during txn.Get. Therefore, always iterate.
	iterOpts := badger.DefaultIteratorOptions
	iterOpts.AllVersions = true
	iterOpts.PrefetchValues = false
	itr := txn.NewKeyIterator(key, iterOpts)
	defer itr.Close()
	latestTs := itr.Seek(key)
	l, err := ReadPostingList(key, itr)
	if err != nil {
		return l, err
	}
	l.RLock()
	// Rollup is useful to improve memory utilization in the cache and also for
	// reads.  However, in case the posting list is split, this would read all
	// the parts and create a full PL. Not sure how much of an issue that is.
	out, err := l.rollup(math.MaxUint64, false)
	l.RUnlock()
	if err != nil {
		return nil, err
	}

	// We could consider writing this to Badger here, as we already have a
	// rolled up version. But, doing the write here to Badger wouldn't be ideal.
	// We write to Badger using Skiplists, instead of writing one entry at a
	// time. In fact, rollups use getNew. So our cache here would get used by
	// the roll up, hence achieving this optimization.

	newList := func() *List {
		return &List{
			minTs: out.newMinTs,
			maxTs: l.maxTs,
			key:   l.key,
			plist: out.plist,
		}
	}

	// Only set l to the cache if readTs >= latestTs, which implies that l is
	// the latest version of the PL. We also check that we're reading a version
	// from Badger, which is higher than the write registered by the cache.
	if readTs >= latestTs && latestTs >= seenTs {
		lCache.SetIfPresent(key, newList(), 0)
	}
	return newList(), nil
}

func copyList(l *List) *List {
	l.AssertRLock()
	// No need to clone the immutable layer or the key since mutations will not modify it.
	lCopy := &List{
		minTs: l.minTs,
		maxTs: l.maxTs,
		key:   l.key,
		plist: l.plist,
	}
	// We do a rollup before storing PL in cache.
	x.AssertTrue(len(l.mutationMap) == 0)
	return lCopy
}
