// Copyright 2021 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package model

import (
	"bytes"
	"fmt"
	"io"

	"github.com/matrixorigin/matrixone/pkg/common/moerr"
	"github.com/matrixorigin/matrixone/pkg/container/types"
	"github.com/matrixorigin/matrixone/pkg/objectio"
)

const (
	MemoTreeVersion1 uint16 = iota
	MemoTreeVersion2
	MemoTreeVersion3
)

type TreeVisitor interface {
	VisitTable(dbID, id uint64, dCnt, tCnt int) error
	String() string
}

type BaseTreeVisitor struct {
	TableFn func(uint64, uint64, int, int) error
}

func (visitor *BaseTreeVisitor) String() string { return "" }

func (visitor *BaseTreeVisitor) VisitTable(dbID, tableID uint64, dCnt, tCnt int) (err error) {
	if visitor.TableFn != nil {
		return visitor.TableFn(dbID, tableID, dCnt, tCnt)
	}
	return
}

type stringVisitor struct {
	buf bytes.Buffer
}

func (visitor *stringVisitor) VisitTable(dbID, id uint64, dcnt, tcnt int) (err error) {
	if visitor.buf.Len() != 0 {
		_ = visitor.buf.WriteByte('\n')
	}
	_, _ = visitor.buf.WriteString(fmt.Sprintf("Tree-TBL(%d,%d)(%d,%d)", dbID, id, dcnt, tcnt))
	return
}

func (visitor *stringVisitor) VisitObject(dbID, tableID uint64, id *objectio.ObjectId) (err error) {
	_, _ = visitor.buf.WriteString(fmt.Sprintf("\nTree-OBJ[%s]", id.String()))
	return
}

func (visitor *stringVisitor) VisitTombstone(dbID, tableID uint64, id *objectio.ObjectId) (err error) {
	_, _ = visitor.buf.WriteString(fmt.Sprintf("\nTree-Tombstone[%s]", id.String()))
	return
}

func (visitor *stringVisitor) String() string {
	if visitor.buf.Len() == 0 {
		return "<Empty Tree>"
	}
	return visitor.buf.String()
}

type Tree struct {
	Tables map[uint64]*TableTree
}

type TableTree struct {
	DbID    uint64
	ID      uint64
	DataCnt int
	TombCnt int
}

func NewTree() *Tree {
	return &Tree{
		Tables: make(map[uint64]*TableTree),
	}
}

func NewTableTree(dbID, id uint64) *TableTree {
	return &TableTree{
		DbID: dbID,
		ID:   id,
	}
}

func (tree *Tree) Reset() {
	tree.Tables = make(map[uint64]*TableTree)
}

func (tree *Tree) String() string {
	visitor := new(stringVisitor)
	_ = tree.Visit(visitor)
	return visitor.String()
}

func (tree *Tree) Visit(visitor TreeVisitor) (err error) {
	for _, table := range tree.Tables {
		if err = visitor.VisitTable(table.DbID, table.ID, table.DataCnt, table.TombCnt); err != nil {
			if moerr.IsMoErrCode(err, moerr.OkStopCurrRecur) {
				err = nil
				continue
			}
			return
		}
	}
	return
}
func (tree *Tree) IsEmpty() bool                 { return tree.TableCount() == 0 }
func (tree *Tree) TableCount() int               { return len(tree.Tables) }
func (tree *Tree) GetTable(id uint64) *TableTree { return tree.Tables[id] }
func (tree *Tree) HasTable(id uint64) bool {
	_, found := tree.Tables[id]
	return found
}

func (tree *Tree) Equal(o *Tree) bool {
	if tree == nil && o == nil {
		return true
	} else if tree == nil || o == nil {
		return false
	}
	if len(tree.Tables) != len(o.Tables) {
		return false
	}
	for id, table := range tree.Tables {
		if otable, found := o.Tables[id]; !found {
			return false
		} else {
			if !table.Equal(otable) {
				return false
			}
		}
	}
	return true
}
func (tree *Tree) AddTable(dbID, id uint64) {
	if _, exist := tree.Tables[id]; !exist {
		table := NewTableTree(dbID, id)
		tree.Tables[id] = table
	}
}

func (tree *Tree) AddObject(dbID, tableID uint64, id *objectio.ObjectId, isTombstone bool) {
	var table *TableTree
	var exist bool
	if table, exist = tree.Tables[tableID]; !exist {
		table = NewTableTree(dbID, tableID)
		tree.Tables[tableID] = table
	}
	table.AddObject(id, isTombstone)
}

func (tree *Tree) Shrink(tableID uint64) (empty bool) {
	delete(tree.Tables, tableID)
	empty = tree.IsEmpty()
	return
}

func (tree *Tree) Compact() (empty bool) {
	toDelete := make([]uint64, 0)
	for id, table := range tree.Tables {
		if table.Compact() {
			toDelete = append(toDelete, id)
		}
	}
	for _, id := range toDelete {
		delete(tree.Tables, id)
	}
	empty = tree.IsEmpty()
	return
}

func (tree *Tree) Merge(ot *Tree) {
	if ot == nil {
		return
	}
	for _, ott := range ot.Tables {
		t, found := tree.Tables[ott.ID]
		if !found {
			t = NewTableTree(ott.DbID, ott.ID)
			tree.Tables[ott.ID] = t
		}
		t.Merge(ott)
	}
}

func (tree *Tree) WriteTo(w io.Writer) (n int64, err error) {
	cnt := uint32(len(tree.Tables))
	if _, err = w.Write(types.EncodeUint32(&cnt)); err != nil {
		return
	}
	n += 4
	var tmpn int64
	for _, table := range tree.Tables {
		if tmpn, err = table.WriteTo(w); err != nil {
			return
		}
		n += tmpn
	}
	return
}

func (tree *Tree) ReadFromWithVersion(r io.Reader, ver uint16) (n int64, err error) {
	var cnt uint32
	if _, err = r.Read(types.EncodeUint32(&cnt)); err != nil {
		return
	}
	n += 4
	if cnt == 0 {
		return
	}
	var tmpn int64
	for i := 0; i < int(cnt); i++ {
		table := NewTableTree(0, 0)
		if tmpn, err = table.ReadFromWithVersion(r, ver); err != nil {
			return
		}
		tree.Tables[table.ID] = table
		n += tmpn
	}
	return
}

func (ttree *TableTree) AddObject(_ *objectio.ObjectId, isTombstone bool) {
	if isTombstone {
		ttree.TombCnt++
	} else {
		ttree.DataCnt++
	}
}

func (ttree *TableTree) IsEmpty() bool {
	return ttree.DataCnt == 0 && ttree.TombCnt == 0
}

func (ttree *TableTree) Compact() (empty bool) {
	empty = ttree.IsEmpty()
	return
}

func (ttree *TableTree) Merge(ot *TableTree) {
	if ot == nil {
		return
	}
	if ot.ID != ttree.ID {
		panic(fmt.Sprintf("Cannot merge 2 different table tree: %d, %d", ttree.ID, ot.ID))
	}
	ttree.DataCnt += ot.DataCnt
	ttree.TombCnt += ot.TombCnt
}

func (ttree *TableTree) WriteTo(w io.Writer) (n int64, err error) {
	if _, err = w.Write(types.EncodeUint64(&ttree.DbID)); err != nil {
		return
	}
	if _, err = w.Write(types.EncodeUint64(&ttree.ID)); err != nil {
		return
	}
	cnt := uint32(ttree.DataCnt)
	if _, err = w.Write(types.EncodeUint32(&cnt)); err != nil {
		return
	}
	cnt = uint32(ttree.TombCnt)
	if _, err = w.Write(types.EncodeUint32(&cnt)); err != nil {
		return
	}
	return
}

func (ttree *TableTree) ReadFromWithVersion(r io.Reader, ver uint16) (n int64, err error) {
	if _, err = r.Read(types.EncodeUint64(&ttree.DbID)); err != nil {
		return
	}
	if _, err = r.Read(types.EncodeUint64(&ttree.ID)); err != nil {
		return
	}
	var cnt uint32
	if _, err = r.Read(types.EncodeUint32(&cnt)); err != nil {
		return
	}
	ttree.DataCnt = int(cnt)
	if _, err = r.Read(types.EncodeUint32(&cnt)); err != nil {
		return
	}
	ttree.TombCnt = int(cnt)

	return
}

func (ttree *TableTree) Equal(o *TableTree) bool {
	if ttree == nil && o == nil {
		return true
	} else if ttree == nil || o == nil {
		return false
	}
	if ttree.ID != o.ID || ttree.DbID != o.DbID {
		return false
	}
	return ttree.DataCnt == o.DataCnt && ttree.TombCnt == o.TombCnt
}
