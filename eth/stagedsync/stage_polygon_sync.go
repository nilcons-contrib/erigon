// Copyright 2024 The Erigon Authors
// This file is part of Erigon.
//
// Erigon is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Erigon is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with Erigon. If not, see <http://www.gnu.org/licenses/>.

package stagedsync

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/erigontech/erigon-lib/chain"
	"github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/common/generics"
	"github.com/erigontech/erigon-lib/common/metrics"
	"github.com/erigontech/erigon-lib/downloader/snaptype"
	"github.com/erigontech/erigon-lib/gointerfaces/sentryproto"
	"github.com/erigontech/erigon-lib/kv"
	"github.com/erigontech/erigon-lib/log/v3"
	"github.com/erigontech/erigon/core/rawdb"
	"github.com/erigontech/erigon/core/rawdb/blockio"
	"github.com/erigontech/erigon/core/types"
	"github.com/erigontech/erigon/eth/stagedsync/stages"
	"github.com/erigontech/erigon/p2p/sentry"
	"github.com/erigontech/erigon/polygon/bor/borcfg"
	"github.com/erigontech/erigon/polygon/bridge"
	"github.com/erigontech/erigon/polygon/heimdall"
	"github.com/erigontech/erigon/polygon/p2p"
	polygonsync "github.com/erigontech/erigon/polygon/sync"
	"github.com/erigontech/erigon/rlp"
	"github.com/erigontech/erigon/turbo/services"
)

var errUpdateForkChoiceSuccess = errors.New("update fork choice success")

func NewPolygonSyncStageCfg(
	logger log.Logger,
	chainConfig *chain.Config,
	db kv.RwDB,
	heimdallClient heimdall.HeimdallClient,
	heimdallStore heimdall.Store,
	bridgeStore bridge.Store,
	sentry sentryproto.SentryClient,
	maxPeers int,
	statusDataProvider *sentry.StatusDataProvider,
	blockReader services.FullBlockReader,
	stopNode func() error,
	blockLimit uint,
) PolygonSyncStageCfg {
	// using a buffered channel to preserve order of tx actions,
	// do not expect to ever have more than 50 goroutines blocking on this channel
	// realistically 6: 4 scrapper goroutines in heimdall.Service, 1 in bridge.Service, 1 in sync.Sync,
	// but does not hurt to leave some extra buffer
	txActionStream := make(chan polygonSyncStageTxAction, 50)
	executionEngine := &polygonSyncStageExecutionEngine{
		blockReader:    blockReader,
		txActionStream: txActionStream,
		logger:         logger,
	}
	stageHeimdallStore := &polygonSyncStageHeimdallStore{
		checkpoints: &polygonSyncStageCheckpointStore{
			checkpointStore: heimdallStore.Checkpoints(),
			txActionStream:  txActionStream,
		},
		milestones: &polygonSyncStageMilestoneStore{
			milestoneStore: heimdallStore.Milestones(),
			txActionStream: txActionStream,
		},
		spans: &polygonSyncStageSpanStore{
			spanStore:      heimdallStore.Spans(),
			txActionStream: txActionStream,
		},
		spanBlockProducerSelections: &polygonSyncStageSbpsStore{
			spanStore:      heimdallStore.SpanBlockProducerSelections(),
			txActionStream: txActionStream,
		},
	}
	stageBridgeStore := &polygonSyncStageBridgeStore{
		eventStore:     bridgeStore,
		txActionStream: txActionStream,
	}
	borConfig := chainConfig.Bor.(*borcfg.BorConfig)
	heimdallService := heimdall.NewService(borConfig, heimdallClient, stageHeimdallStore, logger)
	bridgeService := bridge.NewBridge(stageBridgeStore, logger, borConfig, heimdallClient)
	p2pService := p2p.NewService(maxPeers, logger, sentry, statusDataProvider.GetStatusData)
	checkpointVerifier := polygonsync.VerifyCheckpointHeaders
	milestoneVerifier := polygonsync.VerifyMilestoneHeaders
	blocksVerifier := polygonsync.VerifyBlocks
	syncStore := polygonsync.NewStore(logger, executionEngine, bridgeService)
	blockDownloader := polygonsync.NewBlockDownloader(
		logger,
		p2pService,
		heimdallService,
		checkpointVerifier,
		milestoneVerifier,
		blocksVerifier,
		syncStore,
		blockLimit,
	)
	events := polygonsync.NewTipEvents(logger, p2pService, heimdallService)
	sync := polygonsync.NewSync(
		syncStore,
		executionEngine,
		milestoneVerifier,
		blocksVerifier,
		p2pService,
		blockDownloader,
		polygonsync.NewCanonicalChainBuilderFactory(chainConfig, borConfig, heimdallService),
		heimdallService,
		bridgeService,
		events.Events(),
		logger,
	)
	syncService := &polygonSyncStageService{
		logger:          logger,
		sync:            sync,
		syncStore:       syncStore,
		events:          events,
		p2p:             p2pService,
		executionEngine: executionEngine,
		heimdall:        heimdallService,
		bridge:          bridgeService,
		txActionStream:  txActionStream,
		stopNode:        stopNode,
	}
	return PolygonSyncStageCfg{
		db:          db,
		service:     syncService,
		blockReader: blockReader,
		blockWriter: blockio.NewBlockWriter(),
	}
}

type PolygonSyncStageCfg struct {
	db          kv.RwDB
	service     *polygonSyncStageService
	blockReader services.FullBlockReader
	blockWriter *blockio.BlockWriter
}

func ForwardPolygonSyncStage(
	ctx context.Context,
	tx kv.RwTx,
	stageState *StageState,
	unwinder Unwinder,
	cfg PolygonSyncStageCfg,
) error {
	useExternalTx := tx != nil
	if !useExternalTx {
		var err error
		tx, err = cfg.db.BeginRw(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}

	if err := cfg.service.Run(ctx, tx, stageState, unwinder); err != nil {
		return err
	}

	if !useExternalTx {
		return tx.Commit()
	}

	return nil
}

func UnwindPolygonSyncStage(ctx context.Context, tx kv.RwTx, u *UnwindState, cfg PolygonSyncStageCfg) error {
	u.UnwindPoint = max(u.UnwindPoint, cfg.blockReader.FrozenBlocks()) // protect from unwind behind files

	useExternalTx := tx != nil
	if !useExternalTx {
		var err error
		tx, err = cfg.db.BeginRw(ctx)
		if err != nil {
			return err
		}

		defer tx.Rollback()
	}

	// headers
	unwindBlock := u.Reason.Block != nil
	if err := rawdb.TruncateCanonicalHash(tx, u.UnwindPoint+1, unwindBlock); err != nil {
		return err
	}

	canonicalHash, err := cfg.blockReader.CanonicalHash(ctx, tx, u.UnwindPoint)
	if err != nil {
		return err
	}

	if err = rawdb.WriteHeadHeaderHash(tx, canonicalHash); err != nil {
		return err
	}

	// bodies
	if err = cfg.blockWriter.MakeBodiesNonCanonical(tx, u.UnwindPoint+1); err != nil {
		return err
	}

	// heimdall
	if err = UnwindHeimdall(tx, u, nil); err != nil {
		return err
	}

	if err = u.Done(tx); err != nil {
		return err
	}

	if err := stages.SaveStageProgress(tx, stages.Headers, u.UnwindPoint); err != nil {
		return err
	}

	if err := stages.SaveStageProgress(tx, stages.Bodies, u.UnwindPoint); err != nil {
		return err
	}

	if err := stages.SaveStageProgress(tx, stages.BlockHashes, u.UnwindPoint); err != nil {
		return err
	}

	if !useExternalTx {
		return tx.Commit()
	}

	return nil
}

func UnwindHeimdall(tx kv.RwTx, u *UnwindState, unwindTypes []string) error {
	if len(unwindTypes) == 0 || slices.Contains(unwindTypes, "events") {
		if err := UnwindEvents(tx, u.UnwindPoint); err != nil {
			return err
		}
	}

	if len(unwindTypes) == 0 || slices.Contains(unwindTypes, "spans") {
		if err := UnwindSpans(tx, u.UnwindPoint); err != nil {
			return err
		}

		if err := UnwindSpanBlockProducerSelections(tx, u.UnwindPoint); err != nil {
			return err
		}
	}

	if heimdall.CheckpointsEnabled() && (len(unwindTypes) == 0 || slices.Contains(unwindTypes, "checkpoints")) {
		if err := UnwindCheckpoints(tx, u.UnwindPoint); err != nil {
			return err
		}
	}

	if heimdall.MilestonesEnabled() && (len(unwindTypes) == 0 || slices.Contains(unwindTypes, "milestones")) {
		if err := UnwindMilestones(tx, u.UnwindPoint); err != nil {
			return err
		}
	}

	return nil
}

func UnwindEvents(tx kv.RwTx, unwindPoint uint64) error {
	cursor, err := tx.RwCursor(kv.BorEventNums)
	if err != nil {
		return err
	}
	defer cursor.Close()

	var blockNumBuf [8]byte
	binary.BigEndian.PutUint64(blockNumBuf[:], unwindPoint+1)

	_, _, err = cursor.Seek(blockNumBuf[:])
	if err != nil {
		return err
	}

	_, prevSprintLastIdBytes, err := cursor.Prev() // last event Id of previous sprint
	if err != nil {
		return err
	}

	var prevSprintLastId uint64
	if prevSprintLastIdBytes == nil {
		// we are unwinding the first entry, remove all items from BorEvents
		prevSprintLastId = 0
	} else {
		prevSprintLastId = binary.BigEndian.Uint64(prevSprintLastIdBytes)
	}

	eventId := make([]byte, 8) // first event Id for this sprint
	binary.BigEndian.PutUint64(eventId, prevSprintLastId+1)

	eventCursor, err := tx.RwCursor(kv.BorEvents)
	if err != nil {
		return err
	}
	defer eventCursor.Close()

	for eventId, _, err = eventCursor.Seek(eventId); err == nil && eventId != nil; eventId, _, err = eventCursor.Next() {
		if err = eventCursor.DeleteCurrent(); err != nil {
			return err
		}
	}
	if err != nil {
		return err
	}

	k, _, err := cursor.Next() // move cursor back to this sprint
	if err != nil {
		return err
	}

	for ; err == nil && k != nil; k, _, err = cursor.Next() {
		if err = cursor.DeleteCurrent(); err != nil {
			return err
		}
	}

	epbCursor, err := tx.RwCursor(kv.BorEventProcessedBlocks)
	if err != nil {
		return err
	}

	defer epbCursor.Close()
	for k, _, err = epbCursor.Seek(blockNumBuf[:]); err == nil && k != nil; k, _, err = epbCursor.Next() {
		if err = epbCursor.DeleteCurrent(); err != nil {
			return err
		}
	}
	return err
}

func UnwindSpans(tx kv.RwTx, unwindPoint uint64) error {
	cursor, err := tx.RwCursor(kv.BorSpans)
	if err != nil {
		return err
	}

	defer cursor.Close()
	lastSpanToKeep := heimdall.SpanIdAt(unwindPoint)
	var spanIdBytes [8]byte
	binary.BigEndian.PutUint64(spanIdBytes[:], uint64(lastSpanToKeep+1))
	var k []byte
	for k, _, err = cursor.Seek(spanIdBytes[:]); err == nil && k != nil; k, _, err = cursor.Next() {
		if err = cursor.DeleteCurrent(); err != nil {
			return err
		}
	}

	return err
}

func UnwindSpanBlockProducerSelections(tx kv.RwTx, unwindPoint uint64) error {
	producerCursor, err := tx.RwCursor(kv.BorProducerSelections)
	if err != nil {
		return err
	}
	defer producerCursor.Close()

	lastSpanToKeep := heimdall.SpanIdAt(unwindPoint)
	var spanIdBytes [8]byte
	binary.BigEndian.PutUint64(spanIdBytes[:], uint64(lastSpanToKeep+1))
	var k []byte
	for k, _, err = producerCursor.Seek(spanIdBytes[:]); err == nil && k != nil; k, _, err = producerCursor.Next() {
		if err = producerCursor.DeleteCurrent(); err != nil {
			return err
		}
	}

	return err
}

func UnwindCheckpoints(tx kv.RwTx, unwindPoint uint64) error {
	cursor, err := tx.RwCursor(kv.BorCheckpoints)
	if err != nil {
		return err
	}

	defer cursor.Close()
	lastCheckpointToKeep, err := heimdall.CheckpointIdAt(tx, unwindPoint)
	if errors.Is(err, heimdall.ErrCheckpointNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	var checkpointIdBytes [8]byte
	binary.BigEndian.PutUint64(checkpointIdBytes[:], uint64(lastCheckpointToKeep+1))
	var k []byte
	for k, _, err = cursor.Seek(checkpointIdBytes[:]); err == nil && k != nil; k, _, err = cursor.Next() {
		if err = cursor.DeleteCurrent(); err != nil {
			return err
		}
	}
	return err
}

func UnwindMilestones(tx kv.RwTx, unwindPoint uint64) error {
	cursor, err := tx.RwCursor(kv.BorMilestones)
	if err != nil {
		return err
	}

	defer cursor.Close()
	lastMilestoneToKeep, err := heimdall.MilestoneIdAt(tx, unwindPoint)
	if errors.Is(err, heimdall.ErrMilestoneNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	var milestoneIdBytes [8]byte
	binary.BigEndian.PutUint64(milestoneIdBytes[:], uint64(lastMilestoneToKeep+1))
	var k []byte
	for k, _, err = cursor.Seek(milestoneIdBytes[:]); err == nil && k != nil; k, _, err = cursor.Next() {
		if err = cursor.DeleteCurrent(); err != nil {
			return err
		}
	}
	return err
}

type polygonSyncStageTxAction struct {
	apply func(tx kv.RwTx) error
}

type polygonSyncStageService struct {
	logger          log.Logger
	sync            *polygonsync.Sync
	syncStore       polygonsync.Store
	events          *polygonsync.TipEvents
	p2p             p2p.Service
	executionEngine *polygonSyncStageExecutionEngine
	heimdall        heimdall.Service
	bridge          bridge.Service
	txActionStream  <-chan polygonSyncStageTxAction
	stopNode        func() error
	// internal
	appendLogPrefix func(string) string
	bgComponentsRun bool
	bgComponentsErr chan error
}

func (s *polygonSyncStageService) Run(ctx context.Context, tx kv.RwTx, stageState *StageState, unwinder Unwinder) error {
	s.appendLogPrefix = newAppendLogPrefix(stageState.LogPrefix())
	s.executionEngine.appendLogPrefix = s.appendLogPrefix
	s.executionEngine.stageState = stageState
	s.executionEngine.unwinder = unwinder
	s.logger.Info(s.appendLogPrefix("forward"), "progress", stageState.BlockNumber)

	s.runBgComponentsOnce(ctx)

	if err := s.executionEngine.processCachedForkChoiceIfNeeded(tx); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-s.bgComponentsErr:
			// call stop in separate goroutine to avoid deadlock due to "waitForStageLoopStop"
			go func() {
				s.logger.Error(s.appendLogPrefix("stopping node"), "err", err)
				err = s.stopNode()
				if err != nil {
					s.logger.Error(s.appendLogPrefix("could not stop node cleanly"), "err", err)
				}
			}()

			// use ErrStopped to exit the stage loop
			return fmt.Errorf("%w: %w", common.ErrStopped, err)
		case txAction := <-s.txActionStream:
			err := txAction.apply(tx)
			if errors.Is(err, errUpdateForkChoiceSuccess) {
				return nil
			}
			if errors.Is(err, context.Canceled) {
				// we return a different err and not context.Canceled because that will cancel the stage loop
				// instead we want the stage loop to return to this stage and re-processes
				return errors.New("txAction cancelled by requester")
			}
			if err != nil {
				return err
			}
		}
	}
}

func (s *polygonSyncStageService) runBgComponentsOnce(ctx context.Context) {
	if s.bgComponentsRun {
		return
	}

	s.logger.Info(s.appendLogPrefix("running background components"))
	s.bgComponentsRun = true
	s.bgComponentsErr = make(chan error)

	go func() {
		eg, ctx := errgroup.WithContext(ctx)

		eg.Go(func() error {
			return s.events.Run(ctx)
		})

		eg.Go(func() error {
			return s.heimdall.Run(ctx)
		})

		eg.Go(func() error {
			return s.bridge.Run(ctx)
		})

		eg.Go(func() error {
			return s.p2p.Run(ctx)
		})

		eg.Go(func() error {
			return s.syncStore.Run(ctx)
		})

		eg.Go(func() error {
			return s.sync.Run(ctx)
		})

		if err := eg.Wait(); err != nil {
			s.bgComponentsErr <- err
		}
	}()
}

type polygonSyncStageHeimdallStore struct {
	checkpoints                 *polygonSyncStageCheckpointStore
	milestones                  *polygonSyncStageMilestoneStore
	spans                       *polygonSyncStageSpanStore
	spanBlockProducerSelections *polygonSyncStageSbpsStore
}

func (s polygonSyncStageHeimdallStore) SpanBlockProducerSelections() heimdall.EntityStore[*heimdall.SpanBlockProducerSelection] {
	return s.spanBlockProducerSelections
}

func (s polygonSyncStageHeimdallStore) Checkpoints() heimdall.EntityStore[*heimdall.Checkpoint] {
	return s.checkpoints
}

func (s polygonSyncStageHeimdallStore) Milestones() heimdall.EntityStore[*heimdall.Milestone] {
	return s.milestones
}

func (s polygonSyncStageHeimdallStore) Spans() heimdall.EntityStore[*heimdall.Span] {
	return s.spans
}

func (s polygonSyncStageHeimdallStore) Prepare(_ context.Context) error {
	return nil
}

func (s polygonSyncStageHeimdallStore) Close() {
	// no-op
}

type polygonSyncStageCheckpointStore struct {
	checkpointStore heimdall.EntityStore[*heimdall.Checkpoint]
	txActionStream  chan<- polygonSyncStageTxAction
}

func (s polygonSyncStageCheckpointStore) SnapType() snaptype.Type {
	return s.checkpointStore.SnapType()
}

func (s polygonSyncStageCheckpointStore) LastEntityId(ctx context.Context) (uint64, bool, error) {
	type response struct {
		id  uint64
		ok  bool
		err error
	}

	r, err := awaitTxAction(ctx, s.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		id, ok, err := s.checkpointStore.(interface {
			WithTx(kv.Tx) heimdall.EntityStore[*heimdall.Checkpoint]
		}).WithTx(tx).LastEntityId(ctx)
		return respond(response{id: id, ok: ok, err: err})
	})
	if err != nil {
		return 0, false, err
	}

	return r.id, r.ok, r.err
}

func (s polygonSyncStageCheckpointStore) LastFrozenEntityId() uint64 {
	return s.checkpointStore.LastFrozenEntityId()
}

func (s polygonSyncStageCheckpointStore) LastEntity(ctx context.Context) (*heimdall.Checkpoint, bool, error) {
	id, ok, err := s.LastEntityId(ctx)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}

	return s.Entity(ctx, id)
}

func (s polygonSyncStageCheckpointStore) Entity(ctx context.Context, id uint64) (*heimdall.Checkpoint, bool, error) {
	type response struct {
		v   *heimdall.Checkpoint
		err error
	}

	r, err := awaitTxAction(ctx, s.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		v, _, err := s.checkpointStore.(interface {
			WithTx(kv.Tx) heimdall.EntityStore[*heimdall.Checkpoint]
		}).
			WithTx(tx).Entity(ctx, id)
		return respond(response{v: v, err: err})
	})
	if err != nil {
		return nil, false, err
	}
	if r.err != nil {
		if errors.Is(r.err, heimdall.ErrCheckpointNotFound) {
			return nil, false, nil
		}

		return nil, false, r.err
	}

	return r.v, true, err
}

func (s polygonSyncStageCheckpointStore) PutEntity(ctx context.Context, id uint64, entity *heimdall.Checkpoint) error {
	type response struct {
		err error
	}

	r, err := awaitTxAction(ctx, s.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		err := s.checkpointStore.(interface {
			WithTx(kv.Tx) heimdall.EntityStore[*heimdall.Checkpoint]
		}).WithTx(tx).PutEntity(ctx, id, entity)
		return respond(response{err: err})
	})
	if err != nil {
		return err
	}

	return r.err
}

func (s polygonSyncStageCheckpointStore) RangeFromBlockNum(ctx context.Context, blockNum uint64) ([]*heimdall.Checkpoint, error) {
	type response struct {
		result []*heimdall.Checkpoint
		err    error
	}

	r, err := awaitTxAction(ctx, s.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		r, err := s.checkpointStore.(interface {
			WithTx(kv.Tx) heimdall.EntityStore[*heimdall.Checkpoint]
		}).WithTx(tx).RangeFromBlockNum(ctx, blockNum)
		return respond(response{result: r, err: err})
	})
	if err != nil {
		return nil, err
	}

	return r.result, r.err
}

func (s *polygonSyncStageCheckpointStore) EntityIdFromBlockNum(ctx context.Context, blockNum uint64) (uint64, bool, error) {
	panic("polygonSyncStageCheckpointStore.EntityIdFromBlockNum not supported")
}

func (s polygonSyncStageCheckpointStore) Prepare(_ context.Context) error {
	return nil
}

func (s polygonSyncStageCheckpointStore) Close() {
	// no-op
}

type polygonSyncStageMilestoneStore struct {
	milestoneStore heimdall.EntityStore[*heimdall.Milestone]
	txActionStream chan<- polygonSyncStageTxAction
}

func (s polygonSyncStageMilestoneStore) SnapType() snaptype.Type {
	return s.milestoneStore.SnapType()
}

func (s polygonSyncStageMilestoneStore) LastEntityId(ctx context.Context) (uint64, bool, error) {
	type response struct {
		id  uint64
		ok  bool
		err error
	}

	r, err := awaitTxAction(ctx, s.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		id, ok, err := s.milestoneStore.(interface {
			WithTx(kv.Tx) heimdall.EntityStore[*heimdall.Milestone]
		}).WithTx(tx).LastEntityId(ctx)
		return respond(response{id: id, ok: ok, err: err})
	})
	if err != nil {
		return 0, false, err
	}

	return r.id, r.ok, r.err
}

func (s polygonSyncStageMilestoneStore) LastFrozenEntityId() uint64 {
	return s.milestoneStore.LastFrozenEntityId()
}

func (s polygonSyncStageMilestoneStore) LastEntity(ctx context.Context) (*heimdall.Milestone, bool, error) {
	id, ok, err := s.LastEntityId(ctx)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}

	return s.Entity(ctx, id)
}

func (s polygonSyncStageMilestoneStore) Entity(ctx context.Context, id uint64) (*heimdall.Milestone, bool, error) {
	type response struct {
		v   *heimdall.Milestone
		err error
	}

	r, err := awaitTxAction(ctx, s.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		v, _, err := s.milestoneStore.(interface {
			WithTx(kv.Tx) heimdall.EntityStore[*heimdall.Milestone]
		}).
			WithTx(tx).Entity(ctx, id)
		return respond(response{v: v, err: err})
	})
	if err != nil {
		return nil, false, err
	}
	if r.err != nil {
		if errors.Is(r.err, heimdall.ErrMilestoneNotFound) {
			return nil, false, nil
		}

		return nil, false, r.err
	}

	return r.v, true, err
}

func (s polygonSyncStageMilestoneStore) PutEntity(ctx context.Context, id uint64, entity *heimdall.Milestone) error {
	type response struct {
		err error
	}

	r, err := awaitTxAction(ctx, s.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		err := s.milestoneStore.(interface {
			WithTx(kv.Tx) heimdall.EntityStore[*heimdall.Milestone]
		}).WithTx(tx).PutEntity(ctx, id, entity)
		return respond(response{err: err})
	})
	if err != nil {
		return err
	}

	return r.err
}

func (s polygonSyncStageMilestoneStore) RangeFromBlockNum(ctx context.Context, blockNum uint64) ([]*heimdall.Milestone, error) {
	type response struct {
		result []*heimdall.Milestone
		err    error
	}

	r, err := awaitTxAction(ctx, s.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		r, err := s.milestoneStore.(interface {
			WithTx(kv.Tx) heimdall.EntityStore[*heimdall.Milestone]
		}).WithTx(tx).RangeFromBlockNum(ctx, blockNum)
		return respond(response{result: r, err: err})
	})
	if err != nil {
		return nil, err
	}

	return r.result, r.err
}

func (s *polygonSyncStageMilestoneStore) EntityIdFromBlockNum(ctx context.Context, blockNum uint64) (uint64, bool, error) {
	panic("polygonSyncStageMilestoneStore.EntityIdFromBlockNum not supported")
}

func (s polygonSyncStageMilestoneStore) Prepare(_ context.Context) error {
	return nil
}

func (s polygonSyncStageMilestoneStore) Close() {
	// no-op
}

type polygonSyncStageSpanStore struct {
	spanStore      heimdall.EntityStore[*heimdall.Span]
	txActionStream chan<- polygonSyncStageTxAction
}

func (s polygonSyncStageSpanStore) SnapType() snaptype.Type {
	return s.spanStore.SnapType()
}

func (s polygonSyncStageSpanStore) LastEntityId(ctx context.Context) (id uint64, ok bool, err error) {
	type response struct {
		id  uint64
		ok  bool
		err error
	}

	r, err := awaitTxAction(ctx, s.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		id, ok, err := s.spanStore.(interface {
			WithTx(kv.Tx) heimdall.EntityStore[*heimdall.Span]
		}).WithTx(tx).LastEntityId(ctx)
		return respond(response{id: id, ok: ok, err: err})
	})
	if err != nil {
		return 0, false, err
	}

	return r.id, r.ok, r.err
}

func (s polygonSyncStageSpanStore) LastFrozenEntityId() (id uint64) {
	return s.spanStore.LastFrozenEntityId()
}

func (s polygonSyncStageSpanStore) LastEntity(ctx context.Context) (*heimdall.Span, bool, error) {
	id, ok, err := s.LastEntityId(ctx)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}

	return s.Entity(ctx, id)
}

func (s polygonSyncStageSpanStore) Entity(ctx context.Context, id uint64) (*heimdall.Span, bool, error) {
	type response struct {
		v   *heimdall.Span
		err error
	}

	r, err := awaitTxAction(ctx, s.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		v, _, err := s.spanStore.(interface {
			WithTx(kv.Tx) heimdall.EntityStore[*heimdall.Span]
		}).
			WithTx(tx).Entity(ctx, id)
		return respond(response{v: v, err: err})
	})
	if err != nil {
		return nil, false, err
	}
	if r.err != nil {
		if errors.Is(r.err, heimdall.ErrSpanNotFound) {
			return nil, false, nil
		}

		return nil, false, r.err
	}

	return r.v, true, err
}

func (s polygonSyncStageSpanStore) PutEntity(ctx context.Context, id uint64, entity *heimdall.Span) error {
	type response struct {
		err error
	}

	r, err := awaitTxAction(ctx, s.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		err := s.spanStore.(interface {
			WithTx(kv.Tx) heimdall.EntityStore[*heimdall.Span]
		}).WithTx(tx).PutEntity(ctx, id, entity)
		return respond(response{err: err})
	})
	if err != nil {
		return err
	}

	return r.err
}

func (s polygonSyncStageSpanStore) RangeFromBlockNum(ctx context.Context, blockNum uint64) ([]*heimdall.Span, error) {
	type response struct {
		result []*heimdall.Span
		err    error
	}

	r, err := awaitTxAction(ctx, s.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		r, err := s.spanStore.(interface {
			WithTx(kv.Tx) heimdall.EntityStore[*heimdall.Span]
		}).WithTx(tx).RangeFromBlockNum(ctx, blockNum)
		return respond(response{result: r, err: err})
	})
	if err != nil {
		return nil, err
	}

	return r.result, r.err
}

func (s *polygonSyncStageSpanStore) EntityIdFromBlockNum(ctx context.Context, blockNum uint64) (uint64, bool, error) {
	panic("polygonSyncStageSpanStore.EntityIdFromBlockNum not supported")
}

func (s polygonSyncStageSpanStore) Prepare(_ context.Context) error {
	return nil
}

func (s polygonSyncStageSpanStore) Close() {
	// no-op
}

// polygonSyncStageSbpsStore is the store for heimdall.SpanBlockProducerSelection
type polygonSyncStageSbpsStore struct {
	spanStore      heimdall.EntityStore[*heimdall.SpanBlockProducerSelection]
	txActionStream chan<- polygonSyncStageTxAction
}

func (s polygonSyncStageSbpsStore) LastEntityId(ctx context.Context) (uint64, bool, error) {
	type response struct {
		id  uint64
		ok  bool
		err error
	}

	r, err := awaitTxAction(ctx, s.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		id, ok, err := s.spanStore.(interface {
			WithTx(kv.Tx) heimdall.EntityStore[*heimdall.SpanBlockProducerSelection]
		}).WithTx(tx).LastEntityId(ctx)
		return respond(response{id: id, ok: ok, err: err})
	})
	if err != nil {
		return 0, false, err
	}

	return r.id, r.ok, r.err
}

func (s polygonSyncStageSbpsStore) SnapType() snaptype.Type {
	return nil
}

func (s polygonSyncStageSbpsStore) LastFrozenEntityId() uint64 {
	return s.spanStore.LastFrozenEntityId()
}

func (s polygonSyncStageSbpsStore) LastEntity(ctx context.Context) (*heimdall.SpanBlockProducerSelection, bool, error) {
	id, ok, err := s.LastEntityId(ctx)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}

	return s.Entity(ctx, id)
}

func (s polygonSyncStageSbpsStore) Entity(ctx context.Context, id uint64) (*heimdall.SpanBlockProducerSelection, bool, error) {
	type response struct {
		v   *heimdall.SpanBlockProducerSelection
		err error
	}

	r, err := awaitTxAction(ctx, s.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		v, _, err := s.spanStore.(interface {
			WithTx(kv.Tx) heimdall.EntityStore[*heimdall.SpanBlockProducerSelection]
		}).
			WithTx(tx).Entity(ctx, id)
		return respond(response{v: v, err: err})
	})
	if err != nil {
		return nil, false, err
	}
	if r.err != nil {
		if errors.Is(r.err, heimdall.ErrSpanNotFound) {
			return nil, false, nil
		}

		return nil, false, r.err
	}

	return r.v, true, err
}

func (s polygonSyncStageSbpsStore) PutEntity(ctx context.Context, id uint64, entity *heimdall.SpanBlockProducerSelection) error {
	type response struct {
		err error
	}

	r, err := awaitTxAction(ctx, s.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		err := s.spanStore.(interface {
			WithTx(kv.Tx) heimdall.EntityStore[*heimdall.SpanBlockProducerSelection]
		}).WithTx(tx).PutEntity(ctx, id, entity)
		return respond(response{err: err})
	})
	if err != nil {
		return err
	}

	return r.err
}

func (s polygonSyncStageSbpsStore) RangeFromBlockNum(ctx context.Context, blockNum uint64) ([]*heimdall.SpanBlockProducerSelection, error) {
	type response struct {
		result []*heimdall.SpanBlockProducerSelection
		err    error
	}

	r, err := awaitTxAction(ctx, s.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		r, err := s.spanStore.(interface {
			WithTx(kv.Tx) heimdall.EntityStore[*heimdall.SpanBlockProducerSelection]
		}).WithTx(tx).RangeFromBlockNum(ctx, blockNum)
		return respond(response{result: r, err: err})
	})
	if err != nil {
		return nil, err
	}

	return r.result, r.err
}

func (s *polygonSyncStageSbpsStore) EntityIdFromBlockNum(ctx context.Context, blockNum uint64) (uint64, bool, error) {
	panic("polygonSyncStageSbpsStore.EntityIdFromBlockNum not supported")
}

func (s polygonSyncStageSbpsStore) Prepare(_ context.Context) error {
	return nil
}

func (s polygonSyncStageSbpsStore) Close() {
	// no-op
}

type polygonSyncStageBridgeStore struct {
	eventStore     bridge.Store
	txActionStream chan<- polygonSyncStageTxAction
}

func (s polygonSyncStageBridgeStore) LastEventId(ctx context.Context) (uint64, error) {
	type response struct {
		id  uint64
		err error
	}

	r, err := awaitTxAction(ctx, s.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		id, err := s.eventStore.(interface{ WithTx(kv.Tx) bridge.Store }).WithTx(tx).LastEventId(ctx)
		return respond(response{id: id, err: err})
	})
	if err != nil {
		return 0, err
	}

	return r.id, r.err
}

func (s polygonSyncStageBridgeStore) PutEvents(ctx context.Context, events []*heimdall.EventRecordWithTime) error {
	type response struct {
		err error
	}

	r, err := awaitTxAction(ctx, s.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		return respond(response{err: s.eventStore.(interface{ WithTx(kv.Tx) bridge.Store }).WithTx(tx).PutEvents(ctx, events)})
	})
	if err != nil {
		return err
	}

	return r.err
}

func (s polygonSyncStageBridgeStore) LastProcessedEventId(ctx context.Context) (uint64, error) {
	type response struct {
		id  uint64
		err error
	}

	r, err := awaitTxAction(ctx, s.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		id, err := s.eventStore.(interface{ WithTx(kv.Tx) bridge.Store }).WithTx(tx).LastProcessedEventId(ctx)
		return respond(response{id: id, err: err})
	})
	if err != nil {
		return 0, err
	}
	if r.err != nil {
		return 0, r.err
	}
	if r.id == 0 {
		return s.eventStore.LastFrozenEventId(), nil
	}

	return r.id, nil
}

func (s polygonSyncStageBridgeStore) LastProcessedBlockInfo(ctx context.Context) (bridge.ProcessedBlockInfo, bool, error) {
	type response struct {
		info bridge.ProcessedBlockInfo
		ok   bool
		err  error
	}

	r, err := awaitTxAction(ctx, s.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		info, ok, err := s.eventStore.(interface{ WithTx(kv.Tx) bridge.Store }).WithTx(tx).LastProcessedBlockInfo(ctx)
		return respond(response{info: info, ok: ok, err: err})
	})
	if err != nil {
		return bridge.ProcessedBlockInfo{}, false, err
	}

	return r.info, r.ok, r.err
}

func (s polygonSyncStageBridgeStore) PutProcessedBlockInfo(ctx context.Context, info bridge.ProcessedBlockInfo) error {
	type response struct {
		err error
	}

	r, err := awaitTxAction(ctx, s.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		return respond(response{err: s.eventStore.(interface{ WithTx(kv.Tx) bridge.Store }).
			WithTx(tx).PutProcessedBlockInfo(ctx, info)})
	})
	if err != nil {
		return err
	}

	return r.err
}

func (s polygonSyncStageBridgeStore) LastFrozenEventId() uint64 {
	return s.eventStore.LastFrozenEventId()
}

func (s polygonSyncStageBridgeStore) LastFrozenEventBlockNum() uint64 {
	return s.eventStore.LastFrozenEventBlockNum()
}

func (s polygonSyncStageBridgeStore) LastEventIdWithinWindow(ctx context.Context, fromId uint64, toTime time.Time) (uint64, error) {
	type response struct {
		id  uint64
		err error
	}

	r, err := awaitTxAction(ctx, s.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		id, err := s.eventStore.(interface{ WithTx(kv.Tx) bridge.Store }).WithTx(tx).LastEventIdWithinWindow(ctx, fromId, toTime)
		return respond(response{id: id, err: err})
	})
	if err != nil {
		return 0, err
	}
	if r.err != nil {
		return 0, err
	}

	return r.id, nil
}

func (s polygonSyncStageBridgeStore) PutBlockNumToEventId(ctx context.Context, blockNumToEventId map[uint64]uint64) error {
	type response struct {
		err error
	}

	r, err := awaitTxAction(ctx, s.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		return respond(response{err: s.eventStore.(interface{ WithTx(kv.Tx) bridge.Store }).
			WithTx(tx).PutBlockNumToEventId(ctx, blockNumToEventId)})
	})
	if err != nil {
		return err
	}

	return r.err
}

func (s polygonSyncStageBridgeStore) BorStartEventId(ctx context.Context, hash common.Hash, blockHeight uint64) (uint64, error) {
	panic("polygonSyncStageBridgeStore.BorStartEventId not supported")
}

func (s polygonSyncStageBridgeStore) EventsByBlock(ctx context.Context, hash common.Hash, blockNum uint64) ([]rlp.RawValue, error) {
	panic("polygonSyncStageBridgeStore.EventsByBlock not supported")

}

func (s polygonSyncStageBridgeStore) Events(context.Context, uint64, uint64) ([][]byte, error) {
	// used for accessing events in execution
	// astrid stage integration intends to use the bridge only for scrapping
	// not for reading which remains the same in execution (via BlockReader)
	// astrid standalone mode introduces its own reader
	panic("polygonSyncStageBridgeStore.Events not supported")
}

func (s polygonSyncStageBridgeStore) BlockEventIdsRange(context.Context, uint64) (uint64, uint64, error) {
	// used for accessing events in execution
	// astrid stage integration intends to use the bridge only for scrapping
	// not for reading which remains the same in execution (via BlockReader)
	// astrid standalone mode introduces its own reader
	panic("polygonSyncStageBridgeStore.BlockEventIdsRange not supported")
}

func (s polygonSyncStageBridgeStore) EventLookup(context.Context, common.Hash) (uint64, bool, error) {
	// used in RPCs
	// astrid stage integration intends to use the bridge only for scrapping,
	// not for reading which remains the same in RPCs (via BlockReader)
	// astrid standalone mode introduces its own reader
	panic("polygonSyncStageBridgeStore.EventTxnToBlockNum not supported")
}

func (s polygonSyncStageBridgeStore) EventsByIdFromSnapshot(from uint64, to time.Time, limit int) ([]*heimdall.EventRecordWithTime, bool, error) {
	panic("polygonSyncStageBridgeStore.EventsByIdFromSnapshot not supported")
}

func (s polygonSyncStageBridgeStore) PutEventTxnToBlockNum(context.Context, map[common.Hash]uint64) error {
	// this is a no-op for the astrid stage integration mode because the BorTxLookup table is populated
	// in stage_txlookup.go as part of borTxnLookupTransform
	return nil
}

func (s polygonSyncStageBridgeStore) PruneEventIds(context.Context, uint64) error {
	// at time of writing, pruning for Astrid stage loop integration is handled via the stage loop mechanisms
	panic("polygonSyncStageBridgeStore.PruneEventIds not supported")
}

func (s polygonSyncStageBridgeStore) Prepare(context.Context) error {
	// no-op
	return nil
}

func (s polygonSyncStageBridgeStore) Close() {
	// no-op
}

func newPolygonSyncStageForkChoice(newNodes []chainNode) *polygonSyncStageForkChoice {
	if len(newNodes) == 0 {
		panic("unexpected newNodes to be 0")
	}

	return &polygonSyncStageForkChoice{newNodes: newNodes}
}

type polygonSyncStageForkChoice struct {
	// note newNodes contains tip first and its new ancestors after it (oldest is last)
	// we assume len(newNodes) is never 0, guarded by panic in newPolygonSyncStageForkChoice
	newNodes []chainNode
}

func (fc polygonSyncStageForkChoice) tipBlockNum() uint64 {
	return fc.newNodes[0].number
}

func (fc polygonSyncStageForkChoice) tipBlockHash() common.Hash {
	return fc.newNodes[0].hash
}

func (fc polygonSyncStageForkChoice) oldestNewAncestorBlockNum() uint64 {
	return fc.newNodes[len(fc.newNodes)-1].number
}

func (fc polygonSyncStageForkChoice) numNodes() int {
	return len(fc.newNodes)
}

type chainNode struct {
	hash   common.Hash
	number uint64
}

type polygonSyncStageExecutionEngine struct {
	blockReader    services.FullBlockReader
	txActionStream chan<- polygonSyncStageTxAction
	logger         log.Logger
	// internal
	appendLogPrefix  func(string) string
	stageState       *StageState
	unwinder         Unwinder
	cachedForkChoice *polygonSyncStageForkChoice
}

func (e *polygonSyncStageExecutionEngine) GetHeader(ctx context.Context, blockNum uint64) (*types.Header, error) {
	type response struct {
		header *types.Header
		err    error
	}

	r, err := awaitTxAction(ctx, e.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		header, err := e.blockReader.HeaderByNumber(ctx, tx, blockNum)
		return respond(response{header: header, err: err})
	})
	if err != nil {
		return nil, err
	}

	return r.header, r.err
}

func (e *polygonSyncStageExecutionEngine) InsertBlocks(ctx context.Context, blocks []*types.Block) error {
	type response struct {
		err error
	}

	r, err := awaitTxAction(ctx, e.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		return respond(response{err: e.insertBlocks(tx, blocks)})
	})
	if err != nil {
		return err
	}

	return r.err
}

func (e *polygonSyncStageExecutionEngine) insertBlocks(tx kv.RwTx, blocks []*types.Block) error {
	for _, block := range blocks {
		height := block.NumberU64()
		header := block.Header()
		body := block.Body()

		e.logger.Debug(e.appendLogPrefix("inserting block"), "blockNum", height, "blockHash", header.Hash())

		metrics.UpdateBlockConsumerHeaderDownloadDelay(header.Time, height, e.logger)
		metrics.UpdateBlockConsumerBodyDownloadDelay(header.Time, height, e.logger)

		var parentTd *big.Int
		var err error
		if height > 0 {
			// Parent's total difficulty
			parentHeight := height - 1
			parentTd, err = rawdb.ReadTd(tx, header.ParentHash, parentHeight)
			if err != nil || parentTd == nil {
				return fmt.Errorf(
					"parent's total difficulty not found with hash %x and height %d: %v",
					header.ParentHash,
					parentHeight,
					err,
				)
			}
		} else {
			parentTd = big.NewInt(0)
		}

		td := parentTd.Add(parentTd, header.Difficulty)
		if err := rawdb.WriteHeader(tx, header); err != nil {
			return err
		}

		if err := rawdb.WriteTd(tx, header.Hash(), height, td); err != nil {
			return err
		}

		if _, err := rawdb.WriteRawBodyIfNotExists(tx, header.Hash(), height, body.RawBody()); err != nil {
			return err
		}
	}

	return nil
}

func (e *polygonSyncStageExecutionEngine) UpdateForkChoice(ctx context.Context, tip *types.Header, _ *types.Header) error {
	type response struct {
		err error
	}

	r, err := awaitTxAction(ctx, e.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		err := e.updateForkChoice(ctx, tx, tip)
		if responseErr := respond(response{err: err}); responseErr != nil {
			return responseErr
		}
		if err == nil {
			return errUpdateForkChoiceSuccess
		}
		return nil
	})
	if err != nil {
		return err
	}

	return r.err
}

func (e *polygonSyncStageExecutionEngine) updateForkChoice(ctx context.Context, tx kv.RwTx, tip *types.Header) error {
	tipBlockNum := tip.Number.Uint64()
	tipHash := tip.Hash()

	e.logger.Info(
		e.appendLogPrefix("update fork choice"),
		"block", tipBlockNum,
		"age", common.PrettyAge(time.Unix(int64(tip.Time), 0)),
		"hash", tipHash,
	)

	newNodes, badNodes, err := e.connectTip(ctx, tx, tip)
	if err != nil {
		return err
	}

	if len(badNodes) > 0 {
		badNode := badNodes[len(badNodes)-1]
		unwindNumber := badNode.number - 1
		badHash := badNode.hash
		e.cachedForkChoice = newPolygonSyncStageForkChoice(newNodes)

		e.logger.Info(
			e.appendLogPrefix("new fork - unwinding and caching fork choice"),
			"unwindNumber", unwindNumber,
			"badHash", badHash,
			"cachedTipNumber", e.cachedForkChoice.tipBlockNum(),
			"cachedTipHash", e.cachedForkChoice.tipBlockHash(),
			"cachedNewNodes", e.cachedForkChoice.numNodes(),
		)

		return e.unwinder.UnwindTo(unwindNumber, ForkReset(badHash), tx)
	}

	if len(newNodes) == 0 {
		return nil
	}

	return e.updateForkChoiceForward(tx, newPolygonSyncStageForkChoice(newNodes))
}

func (e *polygonSyncStageExecutionEngine) connectTip(
	ctx context.Context,
	tx kv.RwTx,
	tip *types.Header,
) (newNodes []chainNode, badNodes []chainNode, err error) {
	blockNum := tip.Number.Uint64()
	blockHash := tip.Hash()

	e.logger.Debug(e.appendLogPrefix("connecting tip"), "blockNum", blockNum, "blockHash", blockHash)

	if blockNum == 0 {
		return nil, nil, nil
	}

	var emptyHash common.Hash
	var ch common.Hash
	for {
		ch, err = e.blockReader.CanonicalHash(ctx, tx, blockNum)
		if err != nil {
			return nil, nil, fmt.Errorf("connectTip reading canonical hash for %d: %w", blockNum, err)
		}
		if ch == blockHash {
			break
		}

		h, err := e.blockReader.Header(ctx, tx, blockHash, blockNum)
		if err != nil {
			return nil, nil, err
		}
		if h == nil {
			return nil, nil, fmt.Errorf("connectTip header is nil. blockNum %d, blockHash %x", blockNum, blockHash)
		}

		newNodes = append(newNodes, chainNode{
			hash:   blockHash,
			number: blockNum,
		})

		if ch != emptyHash {
			badNodes = append(badNodes, chainNode{
				hash:   ch,
				number: blockNum,
			})
		}

		blockHash = h.ParentHash
		blockNum--
	}

	return newNodes, badNodes, nil
}

func (e *polygonSyncStageExecutionEngine) updateForkChoiceForward(tx kv.RwTx, fc *polygonSyncStageForkChoice) error {
	tipBlockNum := fc.tipBlockNum()

	for i := fc.numNodes() - 1; i >= 0; i-- {
		newNode := fc.newNodes[i]
		if err := rawdb.WriteCanonicalHash(tx, newNode.hash, newNode.number); err != nil {
			return err
		}
	}

	if err := rawdb.AppendCanonicalTxNums(tx, fc.oldestNewAncestorBlockNum()); err != nil {
		return err
	}

	if err := rawdb.WriteHeadHeaderHash(tx, fc.tipBlockHash()); err != nil {
		return err
	}

	if err := e.stageState.Update(tx, tipBlockNum); err != nil {
		return err
	}

	if err := stages.SaveStageProgress(tx, stages.Headers, tipBlockNum); err != nil {
		return err
	}

	if err := stages.SaveStageProgress(tx, stages.BlockHashes, tipBlockNum); err != nil {
		return err
	}

	if err := stages.SaveStageProgress(tx, stages.Bodies, tipBlockNum); err != nil {
		return err
	}

	return nil
}

func (e *polygonSyncStageExecutionEngine) processCachedForkChoiceIfNeeded(tx kv.RwTx) error {
	if e.cachedForkChoice == nil {
		return nil
	}

	e.logger.Info(
		e.appendLogPrefix("new fork - processing cached fork choice after unwind"),
		"cachedTipNumber", e.cachedForkChoice.tipBlockNum(),
		"cachedTipHash", e.cachedForkChoice.tipBlockHash(),
		"cachedNewNodes", e.cachedForkChoice.numNodes(),
	)

	if err := e.updateForkChoiceForward(tx, e.cachedForkChoice); err != nil {
		return err
	}

	e.cachedForkChoice = nil
	return nil
}

func (e *polygonSyncStageExecutionEngine) CurrentHeader(ctx context.Context) (*types.Header, error) {
	type response struct {
		result *types.Header
		err    error
	}

	r, err := awaitTxAction(ctx, e.txActionStream, func(tx kv.RwTx, respond func(r response) error) error {
		r, err := e.currentHeader(ctx, tx)
		return respond(response{result: r, err: err})
	})
	if err != nil {
		return nil, err
	}

	return r.result, r.err
}

func (e *polygonSyncStageExecutionEngine) currentHeader(ctx context.Context, tx kv.Tx) (*types.Header, error) {
	stageBlockNum, err := stages.GetStageProgress(tx, stages.PolygonSync)
	if err != nil {
		return nil, err
	}

	snapshotBlockNum := e.blockReader.FrozenBlocks()
	if stageBlockNum < snapshotBlockNum {
		return e.blockReader.HeaderByNumber(ctx, tx, snapshotBlockNum)
	}

	hash := rawdb.ReadHeadHeaderHash(tx)
	header := rawdb.ReadHeader(tx, hash, stageBlockNum)
	if header == nil {
		return nil, errors.New("header not found")
	}

	return header, nil
}

func awaitTxAction[T any](
	ctx context.Context,
	txActionStream chan<- polygonSyncStageTxAction,
	cb func(tx kv.RwTx, respond func(response T) error) error,
) (T, error) {
	responseStream := make(chan T)
	respondFunc := func(response T) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case responseStream <- response:
			return nil
		}
	}
	txAction := polygonSyncStageTxAction{
		apply: func(tx kv.RwTx) error {
			return cb(tx, respondFunc)
		},
	}

	select {
	case <-ctx.Done():
		return generics.Zero[T](), ctx.Err()
	case txActionStream <- txAction:
		// no-op
	}

	select {
	case <-ctx.Done():
		return generics.Zero[T](), ctx.Err()
	case resp := <-responseStream:
		return resp, nil
	}
}

func newAppendLogPrefix(logPrefix string) func(msg string) string {
	return func(msg string) string {
		return fmt.Sprintf("[%s] %s", logPrefix, msg)
	}
}
