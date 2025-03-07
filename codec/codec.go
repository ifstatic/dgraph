/*
 * Copyright 2018 Dgraph Labs, Inc. and Contributors
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

package codec

import (
	"encoding/binary"

	"github.com/dgraph-io/dgraph/protos/pb"
	"github.com/dgraph-io/dgraph/x"
	"github.com/dgraph-io/ristretto/z"
	"github.com/dgraph-io/sroar"
)

type seekPos int

const (
	// SeekStart is used with Seek() to search relative to the Uid, returning it in the results.
	SeekStart seekPos = iota
	// SeekCurrent to Seek() a Uid using it as offset, not as part of the results.
	SeekCurrent
)

var (
	bitMask uint64 = 0xffffffff00000000
)

func ToList(rm *sroar.Bitmap) *pb.List {
	return &pb.List{
		Bitmap: rm.ToBufferWithCopy(),
	}
}

func ToSortedList(rm *sroar.Bitmap) *pb.List {
	return &pb.List{
		SortedUids: rm.ToArray(),
	}
}

func ListCardinality(l *pb.List) uint64 {
	if l == nil {
		return 0
	}
	if len(l.SortedUids) > 0 {
		return uint64(len(l.SortedUids))
	}
	b := FromListNoCopy(l)
	return uint64(b.GetCardinality())
}

func OneUid(uid uint64) *pb.List {
	bm := sroar.NewBitmap()
	bm.Set(uid)
	return ToList(bm)
}

func GetUids(l *pb.List) []uint64 {
	if l == nil {
		return []uint64{}
	}
	if len(l.SortedUids) > 0 {
		return l.SortedUids
	}
	return FromListNoCopy(l).ToArray()
}

func SetUids(l *pb.List, uids []uint64) {
	if len(l.SortedUids) > 0 {
		l.SortedUids = uids
	} else {
		r := sroar.NewBitmap()
		r.SetMany(uids)
		l.Bitmap = r.ToBuffer()
	}
}

func BitmapToSorted(l *pb.List) {
	if l == nil {
		return
	}
	l.SortedUids = FromList(l).ToArray()
	l.Bitmap = nil
}

func MatrixToBitmap(matrix []*pb.List) *sroar.Bitmap {
	res := sroar.NewBitmap()
	for _, l := range matrix {
		r := FromList(l)
		res.Or(r)
	}
	return res
}

func Intersect(matrix []*pb.List) *sroar.Bitmap {
	out := sroar.NewBitmap()
	if len(matrix) == 0 {
		return out
	}
	out.Or(FromList(matrix[0]))
	for _, l := range matrix[1:] {
		r := FromList(l)
		out.And(r)
	}
	return out
}

func Merge(matrix []*pb.List) *sroar.Bitmap {
	out := sroar.NewBitmap()
	if len(matrix) == 0 {
		return out
	}

	var bms []*sroar.Bitmap
	for _, m := range matrix {
		if bmc := FromListNoCopy(m); bmc != nil {
			bms = append(bms, bmc)
		}
	}
	return sroar.FastOr(bms...)
}

func FromList(l *pb.List) *sroar.Bitmap {
	if l == nil {
		return sroar.NewBitmap()
	}
	// Keep the check for bitmap before sortedUids because we expect to have bitmap very often
	if len(l.Bitmap) > 0 {
		return sroar.FromBufferWithCopy(l.Bitmap)
	}
	if len(l.SortedUids) > 0 {
		bm := sroar.NewBitmap()
		bm.SetMany(l.SortedUids)
		return bm
	}
	return sroar.NewBitmap()
}

func FromListNoCopy(l *pb.List) *sroar.Bitmap {
	if l == nil {
		return sroar.NewBitmap()
	}
	// Keep the check for bitmap before sortedUids because we expect to have bitmap very often
	if len(l.Bitmap) > 0 {
		return sroar.FromBuffer(l.Bitmap)
	}
	if len(l.SortedUids) > 0 {
		bm := sroar.NewBitmap()
		bm.SetMany(l.SortedUids)
		return bm
	}
	return sroar.NewBitmap()
}

func FromBytes(buf []byte) *sroar.Bitmap {
	r := sroar.NewBitmap()
	if buf == nil || len(buf) == 0 {
		return r
	}
	return sroar.FromBuffer(buf)
}

func FromBackup(buf []byte) *sroar.Bitmap {
	var prev uint64
	var uids []uint64
	for len(buf) > 0 {
		uid, n := binary.Uvarint(buf)
		if uid == 0 {
			break
		}
		buf = buf[n:]

		next := prev + uid
		uids = append(uids, next)
		prev = next
	}
	return sroar.FromSortedList(uids)
}

func ToUids(plist *pb.PostingList, start uint64) []uint64 {
	r := sroar.FromBufferWithCopy(plist.Bitmap)
	r.RemoveRange(0, start)
	return r.ToArray()
}

// RemoveRange would remove [from, to] from bm.
func RemoveRange(bm *sroar.Bitmap, from, to uint64) {
	bm.RemoveRange(from, to)
	bm.Remove(to)
}

// DecodeToBuffer is the same as Decode but it returns a z.Buffer which is
// calloc'ed and can be SHOULD be freed up by calling buffer.Release().
func DecodeToBuffer(buf *z.Buffer, bm *sroar.Bitmap) {
	var last uint64
	tmp := make([]byte, 16)
	itr := bm.ManyIterator()
	uids := make([]uint64, 64)
	for {
		got := itr.NextMany(uids)
		if got == 0 {
			break
		}
		for _, u := range uids[:got] {
			n := binary.PutUvarint(tmp, u-last)
			x.Check2(buf.Write(tmp[:n]))
			last = u
		}
	}
}
