// Copyright 2023 Matrix Origin
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

package lockservice

import (
	"bytes"
	"context"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/matrixorigin/matrixone/pkg/common/moerr"
	"github.com/matrixorigin/matrixone/pkg/common/morpc"
	"github.com/matrixorigin/matrixone/pkg/defines"
	pb "github.com/matrixorigin/matrixone/pkg/pb/lock"
	"github.com/matrixorigin/matrixone/pkg/pb/timestamp"
)

var methodVersions = map[pb.Method]int64{
	pb.Method_Lock:                   defines.MORPCVersion1,
	pb.Method_ForwardLock:            defines.MORPCVersion1,
	pb.Method_Unlock:                 defines.MORPCVersion1,
	pb.Method_GetTxnLock:             defines.MORPCVersion1,
	pb.Method_GetWaitingList:         defines.MORPCVersion1,
	pb.Method_KeepRemoteLock:         defines.MORPCVersion1,
	pb.Method_GetBind:                defines.MORPCVersion1,
	pb.Method_KeepLockTableBind:      defines.MORPCVersion1,
	pb.Method_ForwardUnlock:          defines.MORPCVersion1,
	pb.Method_SetRestartService:      defines.MORPCVersion2,
	pb.Method_CanRestartService:      defines.MORPCVersion2,
	pb.Method_RemainTxnInService:     defines.MORPCVersion2,
	pb.Method_ValidateService:        defines.MORPCVersion2,
	pb.Method_CannotCommit:           defines.MORPCVersion2,
	pb.Method_GetActiveTxn:           defines.MORPCVersion2,
	pb.Method_CheckOrphan:            defines.MORPCVersion2,
	pb.Method_ResumeInvalidCN:        defines.MORPCVersion2,
	pb.Method_AbortRemoteDeadlockTxn: defines.MORPCVersion2,
}

func (s *service) initRemote() {
	if s.cfg.disconnectAfterRead > 0 {
		s.cfg.RPC.BackendOptions = append(s.cfg.RPC.BackendOptions,
			morpc.WithDisconnectAfterRead(s.cfg.disconnectAfterRead))
	}

	rpcClient, err := NewClient(s.cfg.ServiceID, s.cfg.RPC)
	if err != nil {
		panic(err)
	}

	s.activeTxnHolder = newMapBasedTxnHandler(
		s.serviceID,
		s.logger,
		s.fsp,
		func(sid string) (bool, error) {
			ok, err := validateService(s.cfg.RemoteLockTimeout.Duration, sid, s.remote.client, s.logger)
			if err == nil {
				return ok, nil
			}

			// can retry error means we cannot determine whether the service
			// is valid or not.
			if isRetryError(err) {
				return true, nil
			}

			// we determine that the service is invalid, and all associated
			// transaction is marked as cannot commit on tn.
			return false, err
		},
		func(txn []pb.OrphanTxn) ([][]byte, error) {
			req := acquireRequest()
			defer releaseRequest(req)

			req.Method = pb.Method_CannotCommit
			req.CannotCommit.OrphanTxnList = txn

			ctx, cancel := context.WithTimeoutCause(context.Background(), defaultRPCTimeout, moerr.CauseInitRemote1)
			defer cancel()

			resp, err := s.remote.client.Send(ctx, req)
			if err != nil {
				return nil, moerr.AttachCause(ctx, err)
			}
			defer releaseResponse(resp)
			return resp.CannotCommit.CommittingTxn, nil
		},
		func(txn pb.WaitTxn) (bool, error) {
			req := acquireRequest()
			defer releaseRequest(req)

			req.Method = pb.Method_GetActiveTxn
			req.GetActiveTxn.ServiceID = txn.CreatedOn

			ctx, cancel := context.WithTimeoutCause(context.Background(), defaultRPCTimeout, moerr.CauseInitRemote2)
			defer cancel()

			resp, err := s.remote.client.Send(ctx, req)
			if err != nil {
				return false, moerr.AttachCause(ctx, err)
			}
			defer releaseResponse(resp)

			// cn restarted
			if !resp.GetActiveTxn.Valid {
				return false, nil
			}

			for _, v := range resp.GetActiveTxn.Txn {
				if bytes.Equal(v, txn.TxnID) {
					return true, nil
				}
			}
			return false, nil
		},
	)

	rpcServer, err := NewServer(
		s.cfg.ServiceID,
		s.cfg.ListenAddress,
		s.cfg.RPC,
		s.option.serverOpts...,
	)
	if err != nil {
		panic(err)
	}

	s.remote.client = rpcClient
	s.remote.server = rpcServer
	s.remote.keeper = NewLockTableKeeper(
		s.serviceID,
		rpcClient,
		s.cfg.KeepBindDuration.Duration,
		s.cfg.KeepRemoteLockDuration.Duration,
		s.tableGroups,
		s)
	s.initRemoteHandler()
	if err := s.remote.server.Start(); err != nil {
		panic(err)
	}
	if err := s.stopper.RunTask(s.unlockTimeoutRemoteTxn); err != nil {
		panic(err)
	}
}

func (s *service) initRemoteHandler() {
	s.remote.server.RegisterMethodHandler(pb.Method_Lock,
		s.handleRemoteLock)
	s.remote.server.RegisterMethodHandler(pb.Method_ForwardLock,
		s.handleForwardLock)
	s.remote.server.RegisterMethodHandler(pb.Method_Unlock,
		s.handleRemoteUnlock)
	s.remote.server.RegisterMethodHandler(pb.Method_GetTxnLock,
		s.handleRemoteGetLock)
	s.remote.server.RegisterMethodHandler(pb.Method_GetWaitingList,
		s.handleRemoteGetWaitingList)
	s.remote.server.RegisterMethodHandler(pb.Method_KeepRemoteLock,
		s.handleKeepRemoteLock)
	s.remote.server.RegisterMethodHandler(pb.Method_ValidateService,
		s.handleValidateService)
	s.remote.server.RegisterMethodHandler(pb.Method_GetActiveTxn,
		s.handleGetActiveTxn)
	s.remote.server.RegisterMethodHandler(pb.Method_AbortRemoteDeadlockTxn,
		s.handleAbortRemoteDeadlockTxn)
}

func (s *service) handleRemoteLock(
	ctx context.Context,
	cancel context.CancelFunc,
	req *pb.Request,
	resp *pb.Response,
	cs morpc.ClientSession) {
	if !s.canLockOnServiceStatus(req.Lock.TxnID, req.Lock.Options, req.LockTable.Table, req.Lock.Rows) {
		writeResponse(s.logger, cancel, resp, moerr.NewRetryForCNRollingRestart(), cs)
		return
	}

	l, err := s.getLocalLockTable(req, resp)
	if err != nil ||
		l == nil {
		// means that the lockservice sending the lock request holds a stale
		// lock table binding.
		writeResponse(s.logger, cancel, resp, err, cs)
		return
	}

	txn := s.activeTxnHolder.getActiveTxn(req.Lock.TxnID, true, req.Lock.ServiceID)
	txn.Lock()
	defer txn.Unlock()
	if !bytes.Equal(txn.txnID, req.Lock.TxnID) {
		writeResponse(s.logger, cancel, resp, ErrTxnNotFound, cs)
		return
	}
	if txn.deadlockFound {
		writeResponse(s.logger, cancel, resp, ErrDeadLockDetected, cs)
		return
	}

	var e error
	// it needs to inc table bind ref when set restart cn
	h := txn.getHoldLocksLocked(l.getBind().Group)
	_, hasBind := h.tableBinds[l.getBind().Table]
	defer func() {
		if s.isStatus(pb.Status_ServiceLockEnable) ||
			e != nil ||
			hasBind {
			return
		}
		s.incRef(l.getBind().Group, l.getBind().Table)
	}()

	l.lock(
		ctx,
		txn,
		req.Lock.Rows,
		LockOptions{LockOptions: req.Lock.Options, async: true},
		func(result pb.Result, err error) {
			e = err
			resp.Lock.Result = result
			writeResponse(s.logger, cancel, resp, err, cs)
		})
}

func (s *service) handleForwardLock(
	ctx context.Context,
	cancel context.CancelFunc,
	req *pb.Request,
	resp *pb.Response,
	cs morpc.ClientSession) {
	if !s.canLockOnServiceStatus(req.Lock.TxnID, req.Lock.Options, req.LockTable.Table, req.Lock.Rows) {
		writeResponse(s.logger, cancel, resp, moerr.NewRetryForCNRollingRestart(), cs)
		return
	}

	l, err := s.getLockTable(
		req.LockTable.Group,
		req.LockTable.Table)
	if err != nil ||
		l == nil {
		// means that the lockservice sending the lock request holds a stale
		// lock table binding.
		writeResponse(s.logger, cancel, resp, err, cs)
		return
	}

	txn := s.activeTxnHolder.getActiveTxn(req.Lock.TxnID, true, "")
	txn.Lock()
	if !bytes.Equal(txn.txnID, req.Lock.TxnID) {
		txn.Unlock()
		writeResponse(s.logger, cancel, resp, ErrTxnNotFound, cs)
		return
	}
	if txn.deadlockFound {
		txn.Unlock()
		writeResponse(s.logger, cancel, resp, ErrDeadLockDetected, cs)
		return
	}

	var e error
	// it needs to inc table bind ref when set restart cn
	h := txn.getHoldLocksLocked(l.getBind().Group)
	_, hasBind := h.tableBinds[l.getBind().Table]
	defer func() {
		if s.isStatus(pb.Status_ServiceLockEnable) ||
			e != nil ||
			hasBind {
			return
		}
		s.incRef(l.getBind().Group, l.getBind().Table)
	}()

	l.lock(
		ctx,
		txn,
		req.Lock.Rows,
		LockOptions{LockOptions: req.Lock.Options, async: true},
		func(result pb.Result, err error) {
			txn.Unlock()
			e = err
			resp.Lock.Result = result
			writeResponse(s.logger, cancel, resp, err, cs)
		})
}

func (s *service) handleRemoteUnlock(
	ctx context.Context,
	cancel context.CancelFunc,
	req *pb.Request,
	resp *pb.Response,
	cs morpc.ClientSession) {
	err := s.Unlock(ctx, req.Unlock.TxnID, req.Unlock.CommitTS, req.Unlock.Mutations...)
	writeResponse(s.logger, cancel, resp, err, cs)
}

func (s *service) handleValidateService(
	ctx context.Context,
	cancel context.CancelFunc,
	req *pb.Request,
	resp *pb.Response,
	cs morpc.ClientSession) {
	resp.ValidateService = pb.ValidateServiceResponse{
		OK: s.serviceID == req.ValidateService.ServiceID,
	}
	writeResponse(s.logger, cancel, resp, nil, cs)
}

func (s *service) handleGetActiveTxn(
	ctx context.Context,
	cancel context.CancelFunc,
	req *pb.Request,
	resp *pb.Response,
	cs morpc.ClientSession) {
	resp.GetActiveTxn.Valid = s.serviceID == req.GetActiveTxn.ServiceID
	if resp.GetActiveTxn.Valid && s.cfg.TxnIterFunc != nil {
		s.cfg.TxnIterFunc(func(txnID []byte) bool {
			resp.GetActiveTxn.Txn = append(resp.GetActiveTxn.Txn, txnID)
			return true
		})
	}
	writeResponse(s.logger, cancel, resp, nil, cs)
}

func (s *service) handleRemoteGetLock(
	ctx context.Context,
	cancel context.CancelFunc,
	req *pb.Request,
	resp *pb.Response,
	cs morpc.ClientSession) {
	l, err := s.getLocalLockTable(req, resp)
	if err != nil ||
		l == nil {
		// means that the lockservice sending the lock request holds a stale lock
		// table binding.
		writeResponse(s.logger, cancel, resp, err, cs)
		return
	}

	l.getLock(
		req.GetTxnLock.Row,
		pb.WaitTxn{TxnID: req.GetTxnLock.TxnID},
		func(lock Lock) {
			resp.GetTxnLock.Value = int32(lock.value)
			values := make([]pb.WaitTxn, 0)
			lock.waiters.iter(func(w *waiter) bool {
				values = append(values, w.txn)
				return true
			})
			resp.GetTxnLock.WaitingList = values
		})
	writeResponse(s.logger, cancel, resp, err, cs)
}

func (s *service) handleRemoteGetWaitingList(
	ctx context.Context,
	cancel context.CancelFunc,
	req *pb.Request,
	resp *pb.Response,
	cs morpc.ClientSession) {
	select {
	case s.fetchWhoWaitingListC <- who{ctx: ctx, cancel: cancel, cs: cs, resp: resp, txnID: req.GetWaitingList.Txn.TxnID}:
		return
	default:
		writeResponse(s.logger, cancel, resp, ErrDeadLockDetected, cs)
	}
}

func (s *service) handleAbortRemoteDeadlockTxn(
	ctx context.Context,
	cancel context.CancelFunc,
	req *pb.Request,
	resp *pb.Response,
	cs morpc.ClientSession) {
	s.abortDeadlockTxn(req.GetAbortRemoteDeadlockTxn().Txn, ErrDeadLockDetected)
	resp.AbortRemoteDeadlockTxn.OK = true
	writeResponse(s.logger, cancel, resp, nil, cs)
}

func (s *service) handleKeepRemoteLock(
	ctx context.Context,
	cancel context.CancelFunc,
	req *pb.Request,
	resp *pb.Response,
	cs morpc.ClientSession) {
	l, err := s.getLocalLockTable(req, resp)
	if err != nil ||
		l == nil {
		writeResponse(s.logger, cancel, resp, err, cs)
		return
	}

	s.activeTxnHolder.keepRemoteActiveTxn(req.KeepRemoteLock.ServiceID)
	writeResponse(s.logger, cancel, resp, nil, cs)
}

func (s *service) getLocalLockTable(
	req *pb.Request,
	resp *pb.Response) (lockTable, error) {
	l, err := s.getLockTable(
		req.LockTable.Group,
		req.LockTable.Table)
	if err != nil {
		return nil, err
	}
	if l == nil {
		l, err = s.getLockTableWithCreate(
			req.LockTable.Group,
			req.LockTable.Table,
			req.Lock.Rows,
			req.Lock.Options.Sharding)
		if err != nil || l.getBind().Changed(req.LockTable) {
			return nil, ErrLockTableNotFound
		}
	}
	bind := l.getBind()
	if bind.Changed(req.LockTable) {
		resp.NewBind = &bind
		return nil, nil
	}

	if _, ok := l.(*remoteLockTable); ok {
		// Assuming that we have cn0, cn1, and table1, we consider the following timing:
		// 1. at time t0, cn0 obtains the t1 lock table, and the lock-table bind is t1-cn0-table1-version1.
		// 2. at time t1, cn0 down.
		// 3. at time t2, cn0 restarted, and (t2-t1) < cfg.KeepBindTimeout，so lock-table allocator will keep
		//    the bind t1-cn0-table1-version1 valid
		// 4. cn1 try to lock table1 and gets the binding t1-cn0-table1-version1 from allocator or local cache, then
		//    sends a lock request to cn0.
		// 5. cn0 receive the lock request, but the lock-table bind is t1-cn0-table1-version2, and cn0 cn0 will consider
		//    this lock-table bind to be a remote lock table, because the serviceID(t1-cn0) != serviceID(t2-cn0). This
		//    will make rpc handle blocked.
		uuid := getUUIDFromServiceIdentifier(s.serviceID)
		uuidRequest := getUUIDFromServiceIdentifier(bind.ServiceID)
		if strings.EqualFold(uuid, uuidRequest) {
			s.logger.Warn("stale bind found, handle remote lock on remote lock table instance",
				zap.String("bind", bind.DebugString()))
			// only remove old bind lock table
			s.tableGroups.removeWithFilter(
				func(table uint64, lt lockTable) bool {
					return lt.getBind().Equal(bind)
				})
			return nil, ErrLockTableBindChanged
		}

		s.logger.Fatal("get local lock table, but found remote lock table, ip reused between two cns.",
			zap.String("request", req.DebugString()),
			zap.String("serviceID", s.serviceID),
			zap.String("request-lock-table", req.LockTable.DebugString()),
			zap.String("current-bind", bind.DebugString()))
	}

	return l, nil
}

func (s *service) getTxnWaitingListOnRemote(
	txnID []byte,
	createdOn string) ([]pb.WaitTxn, error) {
	ctx, cancel := context.WithTimeoutCause(context.Background(), defaultRPCTimeout, moerr.CauseGetTxnWaitingListOnRemote)
	defer cancel()

	req := acquireRequest()
	defer releaseRequest(req)

	req.Method = pb.Method_GetWaitingList
	req.GetWaitingList.Txn.TxnID = txnID
	req.GetWaitingList.Txn.CreatedOn = createdOn

	resp, err := s.remote.client.Send(ctx, req)
	if err != nil {
		return nil, moerr.AttachCause(ctx, err)
	}
	defer releaseResponse(resp)
	v := resp.GetWaitingList.WaitingList
	return v, nil
}

func (s *service) abortRemoteDeadlockTxn(
	txn pb.WaitTxn) (bool, error) {
	ctx, cancel := context.WithTimeoutCause(context.Background(), defaultRPCTimeout, moerr.CauseAbortRemoteDeadlockTxn)
	defer cancel()

	req := acquireRequest()
	defer releaseRequest(req)

	req.Method = pb.Method_AbortRemoteDeadlockTxn
	req.AbortRemoteDeadlockTxn.Txn = txn

	resp, err := s.remote.client.Send(ctx, req)
	if err != nil {
		return false, moerr.AttachCause(ctx, err)
	}
	defer releaseResponse(resp)
	v := resp.AbortRemoteDeadlockTxn.OK
	return v, nil
}

func (s *service) unlockTimeoutRemoteTxn(ctx context.Context) {
	wait := s.cfg.RemoteLockTimeout.Duration
	timer := time.NewTimer(wait)
	defer timer.Stop()

	timeout := defaultRemoteTxnTimeout
	txnTimer := time.NewTimer(timeout)
	defer txnTimer.Stop()

	var timeoutTxns [][]byte
	timeoutServices := make(map[string]struct{})
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			timeoutTxns = s.activeTxnHolder.getTimeoutRemoveTxn(
				timeoutServices,
				timeoutTxns,
				s.cfg.RemoteLockTimeout.Duration)
			if len(timeoutTxns) > 0 {
				s.logger.Warn("found orphans txns",
					bytesArrayField("txns", timeoutTxns))
				for _, txnID := range timeoutTxns {
					s.Unlock(ctx, txnID, timestamp.Timestamp{})
				}
			}

			timer.Reset(wait)
		case <-txnTimer.C:
			s.checkTxnTimeout(ctx)
			txnTimer.Reset(timeout)
		}
	}
}

func (s *service) checkTxnTimeout(ctx context.Context) {
	if s.isStatus(pb.Status_ServiceLockEnable) {
		return
	}
	txns := s.activeTxnHolder.getAllTxnID()
	for _, t := range txns {
		txn := s.activeTxnHolder.getActiveTxn(t, false, "")
		createOn := txn.remoteService
		if len(createOn) == 0 {
			if !s.isValidLocalTxn(t) {
				s.logger.Warn("found timeout txn",
					bytesArrayField("txn", [][]byte{t}))
				_ = s.Unlock(ctx, t, timestamp.Timestamp{})
			}
			continue
		}

		waitTxn := pb.WaitTxn{TxnID: t, CreatedOn: createOn}
		if !s.activeTxnHolder.isValidRemoteTxn(waitTxn) {
			s.logger.Warn("found timeout txn",
				bytesArrayField("txn", [][]byte{t}))
			_ = s.Unlock(ctx, t, timestamp.Timestamp{})
		}
	}
}

func (s *service) isValidLocalTxn(t []byte) bool {
	if s.cfg.TxnIterFunc == nil {
		return true
	}
	valid := false
	s.cfg.TxnIterFunc(func(txnID []byte) bool {
		if bytes.Equal(t, txnID) {
			valid = true
			return false
		}
		return true
	})
	if valid {
		return valid
	}

	cannotCommit := []pb.OrphanTxn{
		{
			Service: s.serviceID,
			Txn:     [][]byte{t},
		},
	}

	h, ok := s.activeTxnHolder.(*mapBasedTxnHolder)
	if !ok {
		return true
	}
	committing, err := h.notify(cannotCommit)
	if err != nil {
		// any error, we cannot make txn as a invalid txn
		return true
	}
	// the target txn is committing, valid
	return len(committing) != 0
}

func getLockTableBind(
	c Client,
	group uint32,
	tableID uint64,
	originTableID uint64,
	serviceID string,
	sharding pb.Sharding) (pb.LockTable, error) {
	ctx, cancel := context.WithTimeoutCause(context.Background(), defaultRPCTimeout, moerr.CauseGetLockTableBind)
	defer cancel()

	req := acquireRequest()
	defer releaseRequest(req)

	req.Method = pb.Method_GetBind
	req.GetBind.ServiceID = serviceID
	req.GetBind.Table = tableID
	req.GetBind.OriginTable = originTableID
	req.GetBind.Sharding = sharding
	req.GetBind.Group = group

	resp, err := c.Send(ctx, req)
	if err != nil {
		return pb.LockTable{}, moerr.AttachCause(ctx, err)
	}
	defer releaseResponse(resp)
	v := resp.GetBind.LockTable
	return v, nil
}

type who struct {
	ctx    context.Context
	cancel context.CancelFunc
	resp   *pb.Response
	cs     morpc.ClientSession
	txnID  []byte
}

func (s *service) handleFetchWhoWaitingMe(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case w := <-s.fetchWhoWaitingListC:
			txn := s.activeTxnHolder.getActiveTxn(
				w.txnID,
				false,
				"")
			if txn == nil {
				writeResponse(s.logger, w.cancel, w.resp, nil, w.cs)
				continue
			}
			txn.fetchWhoWaitingMe(
				s.serviceID,
				w.txnID,
				func(wt pb.WaitTxn, waiterAddress string) bool {
					wt.WaiterAddress = waiterAddress
					w.resp.GetWaitingList.WaitingList = append(w.resp.GetWaitingList.WaitingList, wt)
					return true
				},
				s.getLockTable)
			writeResponse(s.logger, w.cancel, w.resp, nil, w.cs)
		}
	}
}
