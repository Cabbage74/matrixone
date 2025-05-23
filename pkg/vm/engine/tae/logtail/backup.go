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

package logtail

import (
	"context"
	"fmt"
	"math"

	"github.com/matrixorigin/matrixone/pkg/common/mpool"
	"github.com/matrixorigin/matrixone/pkg/container/vector"
	"github.com/matrixorigin/matrixone/pkg/objectio/ioutil"
	"github.com/matrixorigin/matrixone/pkg/pb/plan"
	"github.com/matrixorigin/matrixone/pkg/util/fault"
	"github.com/matrixorigin/matrixone/pkg/vm/engine"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/ckputil"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/index"
	"go.uber.org/zap"

	"github.com/matrixorigin/matrixone/pkg/container/batch"
	"github.com/matrixorigin/matrixone/pkg/container/types"
	"github.com/matrixorigin/matrixone/pkg/fileservice"
	"github.com/matrixorigin/matrixone/pkg/logutil"
	"github.com/matrixorigin/matrixone/pkg/objectio"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/blockio"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/common"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/containers"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/db/dbutils"
	"github.com/matrixorigin/matrixone/pkg/vm/engine/tae/mergesort"
)

type objData struct {
	stats      *objectio.ObjectStats
	data       []*batch.Batch
	sortKey    uint16
	ckpRow     int
	tid        uint64
	appendable bool
	dataType   objectio.DataMetaType
}

type BackupDeltaLocDataSource struct {
	ctx        context.Context
	fs         fileservice.FileService
	ts         types.TS
	ds         map[string]*objData
	tombstones []objectio.ObjectStats
	needShrink bool
}

func NewBackupDeltaLocDataSource(
	ctx context.Context,
	fs fileservice.FileService,
	ts types.TS,
	ds map[string]*objData,
) *BackupDeltaLocDataSource {
	return &BackupDeltaLocDataSource{
		ctx:        ctx,
		fs:         fs,
		ts:         ts,
		ds:         ds,
		needShrink: true,
	}
}

func (d *BackupDeltaLocDataSource) String() string {
	return "BackupDeltaLocDataSource"
}

func (d *BackupDeltaLocDataSource) SetTS(
	ts types.TS,
) {
	d.ts = ts
}

func (d *BackupDeltaLocDataSource) Next(
	_ context.Context,
	_ []string,
	_ []types.Type,
	_ []uint16,
	_ int32,
	_ any,
	_ *mpool.MPool,
	_ *batch.Batch,
) (*objectio.BlockInfo, engine.DataState, error) {
	return nil, engine.Persisted, nil
}

func (d *BackupDeltaLocDataSource) Close() {

}

func (d *BackupDeltaLocDataSource) ApplyTombstones(
	_ context.Context,
	_ *objectio.Blockid,
	_ []int64,
	_ engine.TombstoneApplyPolicy,
) ([]int64, error) {
	panic("Not Support ApplyTombstones")
}
func (d *BackupDeltaLocDataSource) SetOrderBy(orderby []*plan.OrderBySpec) {
	panic("Not Support order by")
}

func (d *BackupDeltaLocDataSource) GetOrderBy() []*plan.OrderBySpec {
	panic("Not Support order by")
}

func (d *BackupDeltaLocDataSource) SetFilterZM(zm objectio.ZoneMap) {
	panic("Not Support order by")
}

func ForeachTombstoneObject(
	onTombstone func(tombstone *objData) (next bool, err error),
	ds map[string]*objData,
) error {
	for _, d := range ds {
		if d.appendable && d.data[0].Vecs[0].Length() > 0 {
			if next, err := onTombstone(d); !next || err != nil {
				return err
			}
		}
	}
	return nil
}

func buildDS(
	onTombstone func(tombstone objectio.ObjectStats) (next bool, err error),
	ds []objectio.ObjectStats,
) error {
	for _, d := range ds {
		if next, err := onTombstone(d); !next || err != nil {
			return err
		}
	}
	return nil
}

func GetTombstonesByBlockId(
	bid *objectio.Blockid,
	deleteMask *objectio.Bitmap,
	scanOp func(func(tombstone *objData) (bool, error)) error,
	needShrink bool,
) (err error) {

	onTombstone := func(oData *objData) (bool, error) {
		obj := oData.stats
		if !oData.appendable {
			return true, nil
		}
		if !obj.ZMIsEmpty() {
			objZM := obj.SortKeyZoneMap()
			if skip := !objZM.RowidPrefixEq(bid[:]); skip {
				return true, nil
			}
		}

		for idx := 0; idx < int(obj.BlkCnt()); idx++ {
			if idx >= len(oData.data) {
				logutil.Warn("GetTombstonesByBlockId skip tombstone",
					zap.Int("idx", idx),
					zap.Int("len", len(oData.data)),
					zap.Int("blkcnt ", int(obj.BlkCnt())),
					zap.Uint64("tid", oData.tid),
					zap.String("name", obj.ObjectName().String()),
					zap.String("stats", obj.String()))
				return true, nil
			}
			rowids := vector.MustFixedColWithTypeCheck[types.Rowid](oData.data[idx].Vecs[0])
			start, end := ioutil.FindStartEndOfBlockFromSortedRowids(rowids, bid)
			if start == end {
				continue
			}
			deleteRows := make([]int64, 0, end-start)
			for i := start; i < end; i++ {
				row := rowids[i].GetRowOffset()
				deleteMask.Add(uint64(row))
				deleteRows = append(deleteRows, int64(i))
			}

			// Shrink the tombstone batch, Because the rowid in the tombstone is no longer needed after apply,
			// it cannot be written to disk
			if needShrink {
				oData.data[idx].Shrink(deleteRows, true)
			}
		}
		return true, nil
	}

	err = scanOp(onTombstone)
	return err
}

func (d *BackupDeltaLocDataSource) GetTombstones(
	ctx context.Context, bid *objectio.Blockid,
) (deletedRows objectio.Bitmap, err error) {
	// PXU TODO: temp use GetNoReuseBitmap here
	deletedRows = objectio.GetNoReuseBitmap()
	if len(d.tombstones) > 0 {
		if err = buildDS(
			func(tombstone objectio.ObjectStats) (bool, error) {
				if !tombstone.ZMIsEmpty() {
					objZM := tombstone.SortKeyZoneMap()
					if skip := !objZM.PrefixEq(bid[:]); skip {
						return true, nil
					}
				}
				name := tombstone.ObjectName()
				logutil.Infof("[GetSnapshot] tombstone object: %v, block count: %d", name.String(), tombstone.BlkCnt())
				for id := uint32(0); id < tombstone.BlkCnt(); id++ {
					location := tombstone.ObjectLocation()
					location.SetID(uint16(id))
					bat, _, err := ioutil.LoadOneBlock(ctx, d.fs, location, objectio.SchemaData)
					if err != nil {
						return false, err
					}
					if !tombstone.GetCNCreated() {
						deleteRow := make([]int64, 0)
						for v := 0; v < bat.Vecs[0].Length(); v++ {
							var commitTs types.TS
							err = commitTs.Unmarshal(bat.Vecs[len(bat.Vecs)-1].GetRawBytesAt(v))
							if err != nil {
								return false, err
							}
							if commitTs.GT(&d.ts) {

								logutil.Debug("[GetSnapshot]",
									zap.Int("row", v),
									zap.String("commitTs", commitTs.ToString()),
									zap.String("location", location.String()))
							} else {
								deleteRow = append(deleteRow, int64(v))
							}
						}
						if len(deleteRow) != bat.Vecs[0].Length() {
							bat.Shrink(deleteRow, false)
						}
					}
					if id == 0 {
						d.ds[name.String()] = &objData{
							stats:      &tombstone,
							dataType:   objectio.SchemaData,
							sortKey:    uint16(math.MaxUint16),
							data:       make([]*batch.Batch, 0),
							appendable: true,
						}
					}
					d.ds[name.String()].data = append(d.ds[name.String()].data, bat)
				}
				return true, nil
			},
			d.tombstones,
		); err != nil {
			deletedRows.Release()
			return
		}
	}
	scanOp := func(onTombstone func(tombstone *objData) (bool, error)) (err error) {
		return ForeachTombstoneObject(onTombstone, d.ds)
	}

	if err = GetTombstonesByBlockId(
		bid,
		&deletedRows,
		scanOp,
		d.needShrink,
	); err != nil {
		deletedRows.Release()
		return
	}
	return
}

func GetCheckpointReader(
	ctx context.Context,
	sid string,
	fs fileservice.FileService,
	location objectio.Location,
	version uint32,
) (*CKPReader, error) {
	select {
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	default:
	}
	reader := NewCKPReader(version, location, common.CheckpointAllocator, fs)
	if err := reader.ReadMeta(ctx); err != nil {
		return nil, err
	}
	return reader, nil
}

func addObjectToObjectData(
	stats *objectio.ObjectStats,
	isABlk bool,
	row int, tid uint64,
	blockType objectio.DataMetaType,
	objectsData *map[string]*objData,
) {
	name := stats.ObjectName().String()
	if (*objectsData)[name] != nil {
		panic("object already exists")
	}
	object := &objData{
		stats:      stats,
		appendable: isABlk,
		tid:        tid,
		dataType:   blockType,
		sortKey:    uint16(math.MaxUint16),
	}
	(*objectsData)[name] = object
	(*objectsData)[name].ckpRow = row
}

func trimTombstoneData(
	ctx context.Context,
	fs fileservice.FileService,
	ts types.TS,
	objectsData *map[string]*objData,
) error {
	for name := range *objectsData {
		if !(*objectsData)[name].appendable {
			continue
		}
		if (*objectsData)[name].dataType != objectio.SchemaTombstone {
			panic("Invalid data type")
		}
		location := (*objectsData)[name].stats.ObjectLocation()
		var bat *batch.Batch
		var err error
		var sortKey uint16
		// As long as there is an aBlk to be deleted, isCkpChange must be set to true.
		commitTs := types.TS{}
		location.SetID(uint16(0))
		bat, sortKey, err = ioutil.LoadOneBlock(ctx, fs, location, objectio.SchemaData)
		if err != nil {
			return err
		}
		deleteRow := make([]int64, 0)
		for v := 0; v < bat.Vecs[0].Length(); v++ {
			err = commitTs.Unmarshal(bat.Vecs[len(bat.Vecs)-1].GetRawBytesAt(v))
			if err != nil {
				return err
			}
			if commitTs.GT(&ts) {
				logutil.Debugf("delete row %v, commitTs %v, location %v",
					v, commitTs.ToString(), (*objectsData)[name].stats.ObjectLocation().String())
			} else {
				deleteRow = append(deleteRow, int64(v))
			}
		}
		if len(deleteRow) != bat.Vecs[0].Length() {
			bat.Shrink(deleteRow, false)
		}
		bat = formatData(bat)
		(*objectsData)[name].sortKey = sortKey
		(*objectsData)[name].data = make([]*batch.Batch, 0)
		(*objectsData)[name].data = append((*objectsData)[name].data, bat)
	}
	return nil
}

func appendValToBatch(
	account uint32,
	db, tbl uint64,
	objType int8,
	id objectio.ObjectStats,
	create, delete types.TS,
	encoder *types.Packer,
	dst *batch.Batch,
	mp *mpool.MPool,
) (err error) {
	if err = vector.AppendFixed(
		dst.Vecs[ckputil.TableObjectsAttr_Accout_Idx], account, false, mp,
	); err != nil {
		return
	}
	if err = vector.AppendFixed(
		dst.Vecs[ckputil.TableObjectsAttr_DB_Idx], db, false, mp,
	); err != nil {
		return
	}
	if err = vector.AppendFixed(
		dst.Vecs[ckputil.TableObjectsAttr_Table_Idx], tbl, false, mp,
	); err != nil {
		return
	}
	if err = vector.AppendBytes(
		dst.Vecs[ckputil.TableObjectsAttr_ID_Idx], id[:], false, mp,
	); err != nil {
		return
	}
	if err = vector.AppendFixed(
		dst.Vecs[ckputil.TableObjectsAttr_ObjectType_Idx], objType, false, mp,
	); err != nil {
		return
	}
	encoder.Reset()
	ckputil.EncodeCluser(encoder, tbl, objType, id.ObjectName().ObjectId(), delete.IsEmpty())
	if err = vector.AppendBytes(
		dst.Vecs[ckputil.TableObjectsAttr_Cluster_Idx], encoder.Bytes(), false, mp,
	); err != nil {
		return
	}
	if err = vector.AppendFixed(
		dst.Vecs[ckputil.TableObjectsAttr_CreateTS_Idx], create, false, mp,
	); err != nil {
		return
	}
	if err = vector.AppendFixed(
		dst.Vecs[ckputil.TableObjectsAttr_DeleteTS_Idx], delete, false, mp,
	); err != nil {
		return
	}
	dst.SetRowCount(dst.Vecs[0].Length())
	return
}

// Need to format the loaded batch, otherwise panic may occur when WriteBatch.
func formatData(data *batch.Batch) *batch.Batch {
	data.Attrs = make([]string, 0)
	for i := range data.Vecs {
		att := fmt.Sprintf("col_%d", i)
		data.Attrs = append(data.Attrs, att)
	}
	if data.Vecs[0].Length() > 0 {
		tmp := containers.ToTNBatch(data, common.CheckpointAllocator)
		data = containers.ToCNBatch(tmp)
	}
	return data
}

func LoadCheckpointEntriesFromKey(
	ctx context.Context,
	sid string,
	fs fileservice.FileService,
	location objectio.Location,
	version uint32,
	softDeletes *map[string]bool,
	baseTS *types.TS,
) ([]*objectio.BackupObject, *CKPReader, error) {
	locations := make([]*objectio.BackupObject, 0)
	ckpReader, err := GetCheckpointReader(ctx, sid, fs, location, version)
	if err != nil {
		return nil, nil, err
	}

	locations = append(locations, &objectio.BackupObject{
		Location: location,
		NeedCopy: true,
	})

	for _, location = range ckpReader.GetLocations() {
		locations = append(locations, &objectio.BackupObject{
			Location: location,
			NeedCopy: true,
		})
	}

	ckpReader.ForEachRow(
		ctx,
		func(
			account uint32,
			dbid, tid uint64,
			objectType int8,
			objectStats objectio.ObjectStats,
			createAt, deletedAt types.TS,
			rowID types.Rowid,
		) error {
			commitAt := createAt
			if !deletedAt.IsEmpty() {
				commitAt = deletedAt
			}
			isAblk := objectStats.GetAppendable()
			if objectStats.Extent().End() == 0 {
				// tn obj is in the batch too
				return nil
			}

			if deletedAt.IsEmpty() && isAblk {
				// no flush, no need to copy
				return nil
			}

			bo := &objectio.BackupObject{
				Location: objectStats.ObjectLocation(),
				CrateTS:  createAt,
				DropTS:   deletedAt,
			}
			if baseTS.IsEmpty() || (!baseTS.IsEmpty() &&
				(createAt.GE(baseTS) || commitAt.GE(baseTS))) {
				bo.NeedCopy = true
			}
			locations = append(locations, bo)
			if !deletedAt.IsEmpty() {
				if softDeletes != nil {
					if !(*softDeletes)[objectStats.ObjectName().String()] {
						(*softDeletes)[objectStats.ObjectName().String()] = true
					}
				}
			}
			return nil
		},
	)
	return locations, ckpReader, nil
}

func ReWriteCheckpointAndBlockFromKey(
	ctx context.Context,
	sid string,
	fs, dstFs fileservice.FileService,
	loc objectio.Location,
	lastCkpData *CKPReader,
	version uint32, ts types.TS,
) (objectio.Location, objectio.Location, []string, error) {
	logutil.Info("[Start]", common.OperationField("ReWrite Checkpoint"),
		common.OperandField(loc.String()),
		common.OperandField(ts.ToString()))
	phaseNumber := 0
	var err error
	defer func() {
		if err != nil {
			logutil.Error("[DoneWithErr]", common.OperationField("ReWrite Checkpoint"),
				common.AnyField("error", err),
				common.AnyField("phase", phaseNumber),
			)
		}
	}()
	objectsData := make(map[string]*objData, 0)
	tombstonesData := make(map[string]*objData, 0)
	// tombstonesData2 is the tombstone recorded in the last checkpoint,
	// only used when cutting aobject, and does not need to modify itself
	tombstonesData2 := make(map[string]*objData, 0)

	defer func() {
		for i := range objectsData {
			if objectsData[i] != nil && objectsData[i].data != nil {
				for z := range objectsData[i].data {
					for y := range objectsData[i].data[z].Vecs {
						objectsData[i].data[z].Vecs[y].Free(common.DebugAllocator)
					}
				}
			}
		}
	}()
	phaseNumber = 1
	// Load checkpoint
	ckpReader, err := GetCheckpointReader(ctx, sid, fs, loc, version)
	if err != nil {
		return nil, nil, nil, err
	}

	phaseNumber = 2
	// Analyze checkpoint to get the object file
	var files []string

	initData := func(
		od *map[string]*objData,
		objectType int8,
		dataType objectio.DataMetaType,
	) {
		i := 0
		ckpReader.ForEachRow(
			ctx,
			func(
				account uint32,
				dbid, tid uint64,
				objectType2 int8,
				stats objectio.ObjectStats,
				createAt, deleteAt types.TS,
				rowID types.Rowid,
			) error {
				if objectType == objectType2 {
					appendable := stats.GetAppendable()
					commitTS := createAt
					if !deleteAt.IsEmpty() {
						commitTS = deleteAt
					}
					if commitTS.LT(&ts) {
						panic(any(fmt.Sprintf("commitTs less than ts: %v-%v", commitTS.ToString(), ts.ToString())))
					}
					if deleteAt.IsEmpty() {
						i++
						return nil
					}
					if createAt.GE(&ts) {
						panic(any(fmt.Sprintf("createAt equal to ts: %v-%v", createAt.ToString(), ts.ToString())))
					}
					addObjectToObjectData(&stats, appendable, i, tid, dataType, od)
					i++
				}
				return nil
			},
		)
	}

	initData2 := func(
		od *map[string]*objData,
		objectType int8,
		dataType objectio.DataMetaType,
	) {
		i := 0
		lastCkpData.ForEachRow(
			ctx,
			func(
				account uint32,
				dbid, tid uint64,
				objectType2 int8,
				stats objectio.ObjectStats,
				create, deleteAt types.TS,
				rowID types.Rowid,
			) error {
				if objectType2 == objectType {
					appendable := stats.GetAppendable()
					if deleteAt.IsEmpty() {
						i++
						return nil
					}
					if !appendable {
						i++
						return nil
					}
					addObjectToObjectData(&stats, appendable, i, tid, dataType, od)
					i++
				}
				return nil
			},
		)
	}

	initData(&objectsData, ckputil.ObjectType_Data, objectio.SchemaData)
	initData(&tombstonesData, ckputil.ObjectType_Tombstone, objectio.SchemaTombstone)
	initData2(&tombstonesData2, ckputil.ObjectType_Tombstone, objectio.SchemaTombstone)

	phaseNumber = 3

	// Trim tombstone files based on timestamp
	err = trimTombstoneData(ctx, fs, ts, &tombstonesData)
	if err != nil {
		return nil, nil, nil, err
	}
	// Trim tombstone files based on timestamp
	err = trimTombstoneData(ctx, fs, ts, &tombstonesData2)
	if err != nil {
		return nil, nil, nil, err
	}

	backupPool := dbutils.MakeDefaultSmallPool("backup-vector-pool")
	defer backupPool.Destory()
	insertObjBatch := make(map[uint64][]*objData)

	phaseNumber = 4

	insertBatchFun := func(
		objsData map[string]*objData,
		initData func(*objData, *ioutil.BlockWriter) (bool, error),
	) error {
		for _, objectData := range objsData {
			if insertObjBatch[objectData.tid] == nil {
				insertObjBatch[objectData.tid] = make([]*objData, 0)
			}
			if !objectData.appendable {
				insertObjBatch[objectData.tid] = append(insertObjBatch[objectData.tid], objectData)
				continue
			}
			objectName := objectData.stats.ObjectName()
			fileNum := uint16(1000) + objectName.Num()
			segment := objectName.SegmentId()
			name := objectio.BuildObjectName(&segment, fileNum)
			var writer *ioutil.BlockWriter
			writer, err = ioutil.NewBlockWriter(dstFs, name.String())
			if err != nil {
				return err
			}
			var isEmpty bool
			if isEmpty, err = initData(objectData, writer); err != nil {
				return err
			}
			if isEmpty {
				continue
			}

			// For the aBlock that needs to be retained,
			// the corresponding NBlock is generated and inserted into the corresponding batch.
			if len(objectData.data) > 2 {
				panic(any(fmt.Sprintf("objectData.data length > 2: %v - %d",
					objectData.stats.ObjectLocation().String(), len(objectData.data))))
			}

			if objectData.data[0].Vecs[0].Length() == 0 {
				panic(any(fmt.Sprintf("data rows is 0: %v", objectData.stats.ObjectLocation().String())))
			}

			sortData := containers.ToTNBatch(objectData.data[0], common.DebugAllocator)

			if objectData.sortKey != math.MaxUint16 {
				_, err = mergesort.SortBlockColumns(sortData.Vecs, int(objectData.sortKey), backupPool)
				if err != nil {
					return err
				}
			}

			objectData.data[0] = containers.ToCNBatch(sortData)
			_, err = writer.WriteBatch(objectData.data[0])
			if err != nil {
				return err
			}
			blocks, extent, err := writer.Sync(ctx)
			if err != nil {
				return err
			}
			files = append(files, name.String())
			blockLocation := objectio.BuildLocation(name, extent, blocks[0].GetRows(), blocks[0].GetID())
			ss := writer.GetObjectStats()
			objectData.stats = &ss
			objectio.SetObjectStatsLocation(objectData.stats, blockLocation)
			insertObjBatch[objectData.tid] = append(insertObjBatch[objectData.tid], objectData)
		}
		return nil
	}

	// tombstonesData2 is used to merge the source of ds
	dsTombstone := tombstonesData2
	for key, objectData := range tombstonesData {
		if dsTombstone[key] == nil {
			dsTombstone[key] = objectData
		}
	}
	err = insertBatchFun(
		objectsData,
		func(oData *objData, writer *ioutil.BlockWriter) (bool, error) {
			ds := NewBackupDeltaLocDataSource(ctx, fs, ts, dsTombstone)
			blk := oData.stats.ConstructBlockInfo(uint16(0))
			bat, sortKey, err := blockio.BlockDataReadBackup(ctx, &blk, ds, nil, ts, fs)
			if err != nil {
				return true, err
			}
			if bat.Vecs[0].Length() == 0 {
				logutil.Info("[Data Empty] ReWrite Checkpoint",
					zap.String("object", oData.stats.ObjectName().String()),
					zap.Uint64("tid", oData.tid))
				return true, nil
			}
			oData.sortKey = sortKey
			oData.data = make([]*batch.Batch, 0, 1)
			oData.data = append(oData.data, bat)
			if oData.sortKey != math.MaxUint16 {
				writer.SetPrimaryKey(oData.sortKey)
			}
			result := batch.NewWithSize(len(oData.data[0].Vecs) - 2)
			for i := range result.Vecs {
				result.Vecs[i] = oData.data[0].Vecs[i]
			}
			result = formatData(result)
			oData.data[0] = result
			return false, nil
		})

	if err != nil {
		return nil, nil, nil, err
	}

	err = insertBatchFun(
		tombstonesData,
		func(oData *objData, writer *ioutil.BlockWriter) (bool, error) {
			if oData.data[0].Vecs[0].Length() == 0 {
				logutil.Info("[Data Empty] ReWrite Checkpoint",
					zap.String("tombstone", oData.stats.ObjectName().String()),
					zap.Uint64("tid", oData.tid))
				return true, nil
			}
			writer.SetTombstone()
			writer.SetPrimaryKeyWithType(
				uint16(objectio.TombstonePrimaryKeyIdx),
				index.HBF,
				index.ObjectPrefixFn,
				index.BlockPrefixFn,
			)
			return false, nil
		})

	if err != nil {
		return nil, nil, nil, err
	}

	phaseNumber = 5

	dataSinker := ckputil.NewDataSinker(
		common.CheckpointAllocator, dstFs, ioutil.WithMemorySizeThreshold(DefaultCheckpointSize))
	encoder := types.NewPacker()
	defer encoder.Close()
	if len(insertObjBatch) > 0 {
		objectInfoMeta := ckputil.NewObjectListBatch()
		tombstoneInfoMeta := ckputil.NewObjectListBatch()
		infoInsert := make(map[int]*objData, 0)
		infoInsertTombstone := make(map[int]*objData, 0)
		for tid := range insertObjBatch {
			for i := range insertObjBatch[tid] {
				obj := insertObjBatch[tid][i]
				if obj.dataType == objectio.SchemaData {
					if infoInsert[obj.ckpRow] != nil {
						panic("should not have info insert")
					}
					infoInsert[obj.ckpRow] = obj
				} else {
					if infoInsertTombstone[obj.ckpRow] != nil {
						panic("should not have info insert")
					}
					infoInsertTombstone[obj.ckpRow] = obj
				}
			}

		}

		initCkpBatch := func(objectType int8, newMeta *batch.Batch, insertObjData map[int]*objData) {
			i := 0
			ckpReader.ForEachRow(
				ctx,
				func(
					account uint32,
					dbid, tid uint64,
					objectType2 int8,
					objectStats objectio.ObjectStats,
					create, delete types.TS,
					rowID types.Rowid,
				) error {
					if objectType2 == objectType {
						appendValToBatch(account, dbid, tid, objectType2, objectStats, create, delete, encoder, newMeta, common.CheckpointAllocator)
						if insertObjData[i] != nil {
							if !insertObjData[i].appendable {
								row := newMeta.RowCount() - 1
								containers.UpdateValue(
									newMeta.Vecs[ckputil.TableObjectsAttr_DeleteTS_Idx], uint32(row), types.TS{}, false, common.CheckpointAllocator,
								)
							} else {
								appendValToBatch(account, dbid, tid, objectType2, objectStats, create, delete, encoder, newMeta, common.CheckpointAllocator)
								row := newMeta.RowCount() - 1
								objectio.WithSorted()(insertObjData[i].stats)
								containers.UpdateValue(
									newMeta.Vecs[ckputil.TableObjectsAttr_ID_Idx], uint32(row), insertObjData[i].stats[:], false, common.CheckpointAllocator,
								)
								containers.UpdateValue(
									newMeta.Vecs[ckputil.TableObjectsAttr_DeleteTS_Idx], uint32(row), types.TS{}, false, common.CheckpointAllocator,
								)
								_, sarg, _ := fault.TriggerFault("back up UT")
								if sarg == "" {
									containers.UpdateValue(
										newMeta.Vecs[ckputil.TableObjectsAttr_CreateTS_Idx], uint32(row), create, false, common.CheckpointAllocator,
									)
								}
							}
						}
						i++
					}
					return nil
				},
			)
		}

		initCkpBatch(ckputil.ObjectType_Data, objectInfoMeta, infoInsert)
		initCkpBatch(ckputil.ObjectType_Tombstone, tombstoneInfoMeta, infoInsertTombstone)
		dataSinker.Write(ctx, objectInfoMeta)
		dataSinker.Write(ctx, tombstoneInfoMeta)

	} else {
		dest := ckputil.NewObjectListBatch()
		ckpReader.ForEachRow(
			ctx,
			func(
				account uint32,
				dbid, tid uint64,
				objectType int8,
				objectStats objectio.ObjectStats,
				create, delete types.TS,
				rowID types.Rowid,
			) error {
				appendValToBatch(
					account, dbid, tid, objectType, objectStats, create, delete, encoder, dest, common.CheckpointAllocator,
				)
				return nil
			},
		)
		dataSinker.Write(ctx, dest)
	}
	newData := NewCheckpointDataWithSinker(dataSinker, common.CheckpointAllocator)
	location, checkpointFiles, err := newData.Sync(
		ctx, dstFs,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	logutil.Info("[Done]",
		common.AnyField("checkpoint", location.String()),
		common.OperationField("ReWrite Checkpoint"),
		common.AnyField("new object", checkpointFiles))
	files = append(files, checkpointFiles...)
	files = append(files, location.Name().String())
	return location, location, files, nil
}
