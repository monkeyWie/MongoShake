package collector

import (
	"mongoshake/collector/filter"
	"mongoshake/common"
	"mongoshake/oplog"
	"mongoshake/collector/configure"

	LOG "github.com/vinllen/log4go"
	"github.com/gugemichael/nimo4go"
	"github.com/vinllen/mgo/bson"
)

var (
	moveChunkFilter filter.MigrateFilter
	ddlFilter       filter.DDLFilter
	fakeOplog = &oplog.GenericOplog {
		Raw: nil,
		Parsed: &oplog.PartialLog { // initial fake oplog only used in comparison
			ParsedLog: oplog.ParsedLog{
				Timestamp: bson.MongoTimestamp(-2), // fake timestamp,
				Operation: "meaningless operstion",
			},
		},
	}

)

/*
 * as we mentioned in syncer.go, Batcher is used to batch oplog before sending in order to
 * improve performance.
 */
type Batcher struct {
	// related oplog syncer. not owned
	syncer *OplogSyncer

	// filter functionality by gid
	filterList filter.OplogFilterChain
	// oplog handler
	handler OplogHandler

	// current queue cursor
	nextQueue uint64
	// related tunnel workerGroup. not owned
	workerGroup []*Worker

	// the last oplog in the batch
	lastOplog *oplog.GenericOplog
	// first oplog in the next batch
	previousOplog *oplog.GenericOplog
	// the last filtered oplog in the batch
	lastFilterOplog *oplog.PartialLog

	// remainLogs store the logs that split by barrier and haven't been consumed yet.
	remainLogs []*oplog.GenericOplog
	// need flush barrier next generation
	needBarrier bool
}

func NewBatcher(syncer *OplogSyncer, filterList filter.OplogFilterChain,
	handler OplogHandler, workerGroup []*Worker) *Batcher {
	return &Batcher{
		syncer:        syncer,
		filterList:    filterList,
		handler:       handler,
		workerGroup:   workerGroup,
		previousOplog: fakeOplog, // initial fake oplog only used in comparison
		lastOplog:     fakeOplog,
	}
}

/*
 * return the last oplog, if the current batch is empty(first oplog in this batch is ddl),
 * just return the last oplog in the previous batch.
 * if just start, this is nil.
 */
func (batcher *Batcher) getLastOplog() (*oplog.PartialLog, *oplog.PartialLog) {
	return batcher.lastOplog.Parsed, batcher.lastFilterOplog
}

func (batcher *Batcher) filter(log *oplog.PartialLog) bool {
	// filter oplog such like Noop or Gid-filtered
	if batcher.filterList.IterateFilter(log) {
		LOG.Debug("Oplog is filtered. %v", log)
		if batcher.syncer.replMetric != nil {
			batcher.syncer.replMetric.AddFilter(1)
		}
		return true
	}

	if moveChunkFilter.Filter(log) {
		LOG.Crashf("move chunk oplog found[%v]", log)
		return false
	}

	// DDL is disable when timestamp <= fullSyncFinishPosition
	if ddlFilter.Filter(log) && utils.TimestampToInt64(log.Timestamp) <= batcher.syncer.fullSyncFinishPosition {
		LOG.Crashf("ddl oplog found[%v] when oplog timestamp[%v] less than fullSyncFinishPosition[%v]",
			log, log.Timestamp, batcher.syncer.fullSyncFinishPosition)
		return false
	}
	return false
}

func (batcher *Batcher) dispatchBatches(batchGroup [][]*oplog.GenericOplog) (work bool) {
	for i, batch := range batchGroup {
		// we still push logs even if length is zero. so without length check
		if batch != nil {
			work = true
			batcher.workerGroup[i].AllAcked(false)
		}
		batcher.workerGroup[i].Offer(batch)
	}
	return
}

// get a batch
func (batcher *Batcher) getBatch() []*oplog.GenericOplog {
	syncer := batcher.syncer
	var mergeBatch []*oplog.GenericOplog
	if len(batcher.remainLogs) == 0 {
		// remainLogs is empty.
		// first part of merge batch is from current logs queue.
		// It's allowed to be blocked !
		mergeBatch = <-syncer.logsQueue[batcher.currentQueue()]
		// move to next available logs queue
		batcher.moveToNextQueue()
		for len(mergeBatch) < conf.Options.AdaptiveBatchingMaxSize &&
			len(syncer.logsQueue[batcher.currentQueue()]) > 0 {
			// there has more pushed oplogs in next logs queue (read can't to be block)
			// Hence, we fetch them by the way. and merge together
			mergeBatch = append(mergeBatch, <-syncer.logsQueue[batcher.nextQueue]...)
			batcher.moveToNextQueue()
		}
	} else {
		// remainLogs isn't empty
		mergeBatch = batcher.remainLogs
		// we can't use "batcher.remainLogs = batcher.remainLogs[:0]" here
		batcher.remainLogs = make([]*oplog.GenericOplog, 0)
	}

	nimo.AssertTrue(len(mergeBatch) != 0, "logs queue batch logs has zero length")

	return mergeBatch
}

/**
 * return batched oplogs and barrier flag.
 * set barrier if find DDL.
 * i d i c u i
 *      | |
 */
func (batcher *Batcher) batchMore() ([][]*oplog.GenericOplog, bool, bool) {
	// picked raw oplogs and batching in sequence
	batchGroup := make([][]*oplog.GenericOplog, len(batcher.workerGroup))

	transactionOplogs := make([]*oplog.PartialLog, 0)
	barrier := false
Outer:
	for {
		// get a batch
		mergeBatch := batcher.getBatch()
		for i, genericLog := range mergeBatch {
			// filter oplog such like Noop or Gid-filtered
			// PAY ATTENTION: we can't handle the oplog in transaction has been filtered
			if batcher.filter(genericLog.Parsed) {
				// doesn't push to worker, set lastFilterOplog
				batcher.lastFilterOplog = genericLog.Parsed
				if batcher.flushBufferOplogs(&batchGroup, &transactionOplogs) {
					barrier = true
					batcher.remainLogs = mergeBatch[i + 1:]
					break Outer
				}
				batcher.previousOplog = fakeOplog
				continue
			}

			// current is ddl
			if ddlFilter.Filter(genericLog.Parsed) {
				// enable ddl?
				if !conf.Options.ReplayerDMLOnly {
					batcher.addIntoBatchGroup(&batchGroup, batcher.previousOplog)
					// store and handle in the next call
					if i == 0 {
						// first is DDL, add barrier after
						batcher.addIntoBatchGroup(&batchGroup, genericLog)
						batcher.remainLogs = mergeBatch[i + 1:]
					} else {
						// add barrier before, current oplog should handled on the next iteration
						batcher.remainLogs = mergeBatch[i:]
					}

					batcher.previousOplog = fakeOplog
					barrier = true
					// return batchGroup, true, false
					break Outer
				} else {
					// filter
					// doesn't push to worker, set lastFilterOplog
					batcher.lastFilterOplog = genericLog.Parsed
					if batcher.flushBufferOplogs(&batchGroup, &transactionOplogs) {
						barrier = true
						batcher.remainLogs = mergeBatch[i + 1:]
						break Outer
					}
					batcher.previousOplog = fakeOplog
					continue
				}
			}

			// need merge transaction?
			if genericLog.Parsed.Timestamp == batcher.previousOplog.Parsed.Timestamp {
				transactionOplogs = append(transactionOplogs, batcher.previousOplog.Parsed)
			} else if len(transactionOplogs) != 0 {
				transactionOplogs = append(transactionOplogs, batcher.previousOplog.Parsed)
				gathered := batcher.gatherTransaction(transactionOplogs)

				batcher.addIntoBatchGroup(&batchGroup, gathered)
				batcher.remainLogs = mergeBatch[i:]
				batcher.previousOplog = fakeOplog

				barrier = true
				// return batchGroup, true, false
				break Outer
			} else {
				batcher.addIntoBatchGroup(&batchGroup, batcher.previousOplog)
			}

			batcher.previousOplog = genericLog
		}

		// only gather more data when transactionOplogs isn't empty
		if len(transactionOplogs) == 0 {
			break
		}
	}

	// all oplogs are filtered?
	allEmpty := true
	// get the last oplog
	for _, ele := range batchGroup {
		if ele != nil && len(ele) > 0 {
			allEmpty = false
			rawLast := ele[len(ele) - 1]
			if rawLast.Parsed.Timestamp > batcher.lastOplog.Parsed.Timestamp {
				batcher.lastOplog = rawLast
			}
		}
	}

	return batchGroup, barrier, allEmpty
}

func (batcher *Batcher) addIntoBatchGroup(batchGroup *[][]*oplog.GenericOplog, genericLog *oplog.GenericOplog) {
	if genericLog == fakeOplog {
		return
	}

	batcher.handler.Handle(genericLog.Parsed)
	which := batcher.syncer.hasher.DistributeOplogByMod(genericLog.Parsed, len(batcher.workerGroup))
	(*batchGroup)[which] = append((*batchGroup)[which], genericLog)
}

func (batcher *Batcher) gatherTransaction(transactionOplogs []*oplog.PartialLog) *oplog.GenericOplog {
	// transaction oplogs should gather into an applyOps operation and add barrier here
	gathered, err := oplog.GatherApplyOps(transactionOplogs)
	if err != nil {
		LOG.Crashf("gather applyOps failed[%v]", err)
	}
	return gathered
}

// flush previous buffered oplog, true means should add barrier
func (batcher *Batcher) flushBufferOplogs(batchGroup *[][]*oplog.GenericOplog, transactionOplogs *[]*oplog.PartialLog) bool {
	if batcher.previousOplog == fakeOplog {
		return false
	}

	if len(*transactionOplogs) > 0 {
		if batcher.previousOplog == fakeOplog {
			LOG.Crashf("previous is fakeOplog when transaction oplogs is empty")
		}

		*transactionOplogs = append(*transactionOplogs, batcher.previousOplog.Parsed)
		gathered := batcher.gatherTransaction(*transactionOplogs)

		batcher.addIntoBatchGroup(batchGroup, gathered)
		batcher.previousOplog = fakeOplog
		return true
	}

	batcher.addIntoBatchGroup(batchGroup, batcher.previousOplog)
	batcher.previousOplog = fakeOplog
	return false
}

func (batcher *Batcher) moveToNextQueue() {
	batcher.nextQueue++
	batcher.nextQueue = batcher.nextQueue % uint64(len(batcher.syncer.logsQueue))
}

func (batcher *Batcher) currentQueue() uint64 {
	return batcher.nextQueue
}
