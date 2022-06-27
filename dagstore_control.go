package dagstore

import (
	"context"
	"fmt"

	"github.com/filecoin-project/dagstore/mount"

	"github.com/filecoin-project/dagstore/shard"
)

type OpType int

const (
	OpShardRegister OpType = iota
	OpShardInitialize
	OpShardMakeAvailable
	OpShardDestroy
	OpShardAcquire
	OpShardFail
	OpShardRelease
	OpShardRecover
	OpShardReserveTransient
	OpShardReleaseTransientReservation
)

var (
	defaultReservation = 134217728 // 128 Mib
)

func (o OpType) String() string {
	return [...]string{
		"OpShardRegister",
		"OpShardInitialize",
		"OpShardMakeAvailable",
		"OpShardDestroy",
		"OpShardAcquire",
		"OpShardFail",
		"OpShardRelease",
		"OpShardRecover",
		"OpShardReserveTransient",
		"OpShardReleaseTransientReservation"}[o]
}

// control runs the DAG store's event loop.
func (d *DAGStore) control() {
	defer d.wg.Done()

	// wFailure is a synthetic failure waiter that uses the DAGStore's
	// global context and the failure channel. Only safe to actually use if
	// d.failureCh != nil. wFailure is used to dispatch failure
	// notifications to the application.
	var wFailure = &waiter{ctx: d.ctx, outCh: d.failureCh}

	for {
		// consume the next task or GC request; if we're shutting down, this method will error.
		tsk, gc, err := d.consumeNext()
		if err != nil {
			if err == context.Canceled {
				log.Infow("dagstore closed")
			} else {
				log.Errorw("consuming next task failed; aborted event loop; dagstore unoperational", "error", err)
			}
			return
		}

		if gc != nil {
			// this was a manual GC request.
			d.manualGC(gc)
			continue
		}
		// perform GC if the transients directory has already gone above the watermark and automated gc is enabled.
		if d.automatedGCEnabled {
			maxTransientDirSize := d.maxTransientDirSize
			transientsGCWatermarkHigh := d.transientsGCWatermarkHigh
			transientsGCWatermarkLow := d.transientsGCWatermarkLow

			if float64(d.totalTransientDirSize) >= float64(maxTransientDirSize)*transientsGCWatermarkHigh {
				target := float64(maxTransientDirSize) * transientsGCWatermarkLow
				d.gcUptoTarget(target)
			}
		}

		s := tsk.shard
		log.Debugw("processing task", "op", tsk.op, "shard", tsk.shard.key, "error", tsk.err)

		s.lk.Lock()
		prevState := s.state

		switch tsk.op {
		case OpShardRegister:
			if s.state != ShardStateNew {
				// sanity check failed
				_ = d.failShard(s, d.internalCh, "%w: expected shard to be in 'new' state; was: %s", ErrShardInitializationFailed, s.state)
				break
			}

			// skip initialization if shard was registered with lazy init, and
			// respond immediately to waiter.
			if s.lazy {
				log.Debugw("shard registered with lazy initialization", "shard", s.key)
				// waiter will be nil if this was a restart and not a call to Register() call.
				if tsk.waiter != nil {
					res := &ShardResult{Key: s.key}
					d.dispatchResult(res, tsk.waiter)
				}
				break
			}

			// otherwise, park the registration channel and queue the init.
			s.wRegister = tsk.waiter
			_ = d.queueTask(&task{op: OpShardInitialize, shard: s, waiter: tsk.waiter}, d.internalCh)

		case OpShardInitialize:
			s.state = ShardStateInitializing

			// if we already have the index for this shard, there's nothing to do here.
			if istat, err := d.indices.StatFullIndex(s.key); err == nil && istat.Exists {
				log.Debugw("already have an index for shard being initialized, nothing to do", "shard", s.key)
				_ = d.queueTask(&task{op: OpShardMakeAvailable, shard: s}, d.internalCh)
				break
			}

			go d.initializeShard(tsk.ctx, s, s.mount)

		case OpShardMakeAvailable:
			// can arrive here after initializing a new shard,
			// or when recovering from a failure.

			s.state = ShardStateAvailable

			st, err := s.mount.Stat(d.ctx)
			if err != nil {
				log.Errorw("failed to stat transient", "shard", s.key, "error", err)
			} else {
				s.transientSize = st.Size
			}

			s.err = nil // nillify past errors

			// notify the registration waiter, if there is one.
			if s.wRegister != nil {
				res := &ShardResult{Key: s.key}
				d.dispatchResult(res, s.wRegister)
				s.wRegister = nil
			}

			// notify the recovery waiter, if there is one.
			if s.wRecover != nil {
				res := &ShardResult{Key: s.key}
				d.dispatchResult(res, s.wRecover)
				s.wRecover = nil
			}

			// trigger queued acquisition waiters.
			for _, w := range s.wAcquire {
				s.state = ShardStateServing

				// optimistically increment the refcount to acquire the shard. The go-routine will send an `OpShardRelease` message
				// to the event loop if it fails to acquire the shard.
				s.refs++
				go d.acquireAsync(w.ctx, w, s, s.mount)
			}
			s.wAcquire = s.wAcquire[:0]

		case OpShardAcquire:
			log.Debugw("got request to acquire shard", "shard", s.key, "current shard state", s.state)
			w := &waiter{ctx: tsk.ctx, outCh: tsk.outCh}

			// if the shard is errored, fail the acquire immediately.
			if s.state == ShardStateErrored {
				if s.recoverOnNextAcquire {
					// we are errored, but recovery was requested on the next acquire
					// we park the acquirer and trigger a recover.
					s.wAcquire = append(s.wAcquire, w)
					s.recoverOnNextAcquire = false
					// we use the global context instead of the acquire context
					// to avoid the first context cancellation interrupting the
					// recovery that may be blocking other acquirers with longer
					// contexts.
					_ = d.queueTask(&task{op: OpShardRecover, shard: s, waiter: &waiter{ctx: d.ctx}}, d.internalCh)
				} else {
					err := fmt.Errorf("shard is in errored state; err: %w", s.err)
					res := &ShardResult{Key: s.key, Error: err}
					d.dispatchResult(res, w)
				}
				break
			}

			if s.state != ShardStateAvailable && s.state != ShardStateServing {
				log.Debugw("shard isn't active yet, will queue acquire channel", "shard", s.key)
				// shard state isn't active yet; make this acquirer wait.
				s.wAcquire = append(s.wAcquire, w)

				// if the shard was registered with lazy init, and this is the
				// first acquire, queue the initialization.
				if s.state == ShardStateNew {
					log.Debugw("acquiring shard with lazy init enabled, will queue shard initialization", "shard", s.key)
					// Override the context with the background context.
					// We can't use the acquirer's context for initialization
					// because there can be multiple concurrent acquirers, and
					// if the first one cancels, the entire job would be cancelled.
					w := *tsk.waiter
					w.ctx = context.Background()
					_ = d.queueTask(&task{op: OpShardInitialize, shard: s, waiter: &w}, d.internalCh)
				}

				break
			}

			// mark as serving.
			s.state = ShardStateServing

			// optimistically increment the refcount to acquire the shard.
			// The goroutine will send an `OpShardRelease` task
			// to the event loop if it fails to acquire the shard.
			s.refs++
			go d.acquireAsync(tsk.ctx, w, s, s.mount)

		case OpShardRelease:
			if (s.state != ShardStateServing && s.state != ShardStateErrored) || s.refs <= 0 {
				log.Warn("ignored illegal request to release shard")
				break
			}

			// decrement refcount.
			s.refs--

			// reset state back to available, if we were the last
			// active acquirer.
			if s.refs == 0 {
				s.state = ShardStateAvailable
			}

		case OpShardFail:
			s.state = ShardStateErrored
			s.transientSize = 0
			s.err = tsk.err

			// notify the registration waiter, if there is one.
			if s.wRegister != nil {
				res := &ShardResult{
					Key:   s.key,
					Error: fmt.Errorf("failed to register shard: %w", tsk.err),
				}
				d.dispatchResult(res, s.wRegister)
				s.wRegister = nil
			}

			// notify the recovery waiter, if there is one.
			if s.wRecover != nil {
				res := &ShardResult{
					Key:   s.key,
					Error: fmt.Errorf("failed to recover shard: %w", tsk.err),
				}
				d.dispatchResult(res, s.wRecover)
				s.wRecover = nil
			}

			// fail waiting acquirers.
			// can't block the event loop, so launch a goroutine per acquirer.
			if len(s.wAcquire) > 0 {
				err := fmt.Errorf("failed to acquire shard: %w", tsk.err)
				res := &ShardResult{Key: s.key, Error: err}
				d.dispatchResult(res, s.wAcquire...)
				s.wAcquire = s.wAcquire[:0] // clear acquirers.
			}

			// Should we interrupt/disturb active acquirers? No.
			//
			// This part doesn't know which kind of error occurred.
			// It could be that the index has disappeared for new acquirers, but
			// active acquirers already have it.
			//
			// If this is a physical error (e.g. shard data was physically
			// deleted, or corrupted), we'll leave to the ShardAccessor (and the
			// ReadBlockstore) to fail at some point. At that stage, the caller
			// will call ShardAccessor#Close and eventually all active
			// references will be released, setting the shard in an errored
			// state with zero refcount.

			// Notify the application of the failure, if they provided a channel.
			if ch := d.failureCh; ch != nil {
				res := &ShardResult{Key: s.key, Error: s.err}
				d.dispatchFailuresCh <- &dispatch{res: res, w: wFailure}
			}

		case OpShardRecover:
			if s.state != ShardStateErrored {
				err := fmt.Errorf("refused to recover shard in state other than errored; current state: %d", s.state)
				res := &ShardResult{Key: s.key, Error: err}
				d.dispatchResult(res, tsk.waiter)
				break
			}

			// set the state to recovering.
			s.state = ShardStateRecovering
			// park the waiter; there can never be more than one because
			// subsequent calls to recover the same shard will be rejected
			// because the state is no longer ShardStateErrored.
			s.wRecover = tsk.waiter

			// attempt to delete the transient first; this can happen if the
			// transient has been removed by hand. DeleteTransient resets the
			// transient to "" always.
			freed, err := s.mount.DeleteTransient()
			if err != nil {
				log.Warnw("recovery: failed to delete transient", "shard", s.key, "error", err)
			}
			d.totalTransientDirSize -= freed

			// attempt to drop the index.
			dropped, err := d.indices.DropFullIndex(s.key)
			if err != nil {
				log.Warnw("recovery: failed to drop index for shard", "shard", s.key, "error", err)
			} else if !dropped {
				log.Debugw("recovery: no index dropped for shard", "shard", s.key)
			}

			// fetch again and reindex.
			go d.initializeShard(tsk.ctx, s, s.mount)

		case OpShardDestroy:
			if s.state == ShardStateServing || s.refs > 0 {
				err := fmt.Errorf("failed to destroy shard; active references: %d", s.refs)
				res := &ShardResult{Key: s.key, Error: err}
				d.dispatchResult(res, tsk.waiter)
				break
			}

			d.lk.Lock()
			delete(d.shards, s.key)
			d.lk.Unlock()
			// TODO are we guaranteed that there are no queued items for this shard?

		case OpShardReserveTransient:
			if s.state != ShardStateServing && s.state != ShardStateInitializing && s.state != ShardStateRecovering {
				// sanity check failed
				_ = d.failShard(s, d.internalCh, "%w: expected shard to be in 'serving' or `initialising` or `recovering` "+
					"state; was: %s", ErrShardIllegalReservationRequest, s.state)
				break
			}

			toReserve := tsk.reservationReq.want
			reservationSizeUnknown := toReserve == 0

			// increase the space allocated exponentially as more reservations are requested for a shard whose
			// transient size is unknown upfront.
			if reservationSizeUnknown {
				factor := int64(1 << tsk.reservationReq.nPrevReservations)
				toReserve = factor * int64(defaultReservation)
			}

			mkReservation := func() {
				d.totalTransientDirSize += toReserve
				tsk.reservationReq.response <- &reservationResp{reserved: toReserve}
			}
			// do we have enough space available ? if yes, allocate the reservation right away
			if d.totalTransientDirSize+toReserve <= d.maxTransientDirSize {
				mkReservation()
				break
			}

			// otherwise, perform a GC to make space for the reservation.
			d.gcUptoTarget(float64(d.maxTransientDirSize - toReserve))

			// if we have enough space available after the gc, allocate the reservation
			if d.totalTransientDirSize+toReserve <= d.maxTransientDirSize {
				mkReservation()
				break
			}

			// we weren't able to make space for the reservation request even after a GC attempt.
			// fail the reservation request.
			tsk.reservationReq.response <- &reservationResp{err: ErrNotEnoughSpaceInTransientsDir}

		case OpShardReleaseTransientReservation:
			if s.state != ShardStateServing && s.state != ShardStateInitializing && s.state != ShardStateRecovering {
				// sanity check failed
				_ = d.failShard(s, d.internalCh, "%w: expected shard to be in 'serving' or `initialising` or `recovering` "+
					"state; was: %s", ErrShardIllegalReservationRequest, s.state)
				break
			}
			d.totalTransientDirSize -= tsk.releaseReq.release

		default:
			panic(fmt.Sprintf("unrecognized shard operation: %d", tsk.op))
		}

		// update the GarbageCollector
		d.notifyGCStrategy(s.key, s, tsk.op)

		// persist the current shard state.
		if err := s.persist(d.ctx, d.config.Datastore); err != nil { // TODO maybe fail shard?
			log.Warnw("failed to persist shard", "shard", s.key, "error", err)
		}

		// send a notification if the user provided a notification channel.
		if d.traceCh != nil {
			log.Debugw("will write trace to the trace channel", "shard", s.key)
			n := Trace{
				Key: s.key,
				Op:  tsk.op,
				After: ShardInfo{
					ShardState: s.state,
					Error:      s.err,
					refs:       s.refs,
				},
				TransientDirSize: d.totalTransientDirSize,
			}
			d.traceCh <- n
			log.Debugw("finished writing trace to the trace channel", "shard", s.key)
		}

		log.Debugw("finished processing task", "op", tsk.op, "shard", tsk.shard.key, "prev_state", prevState, "curr_state", s.state, "error", tsk.err)

		s.lk.Unlock()
	}
}

func (d *DAGStore) notifyGCStrategy(key shard.Key, s *Shard, op OpType) {
	if op == OpShardDestroy {
		d.gcs.NotifyRemoved(s.key)
		return
	}

	// notify the garbage collector if shard was accessed
	if op == OpShardAcquire {
		d.gcs.NotifyAccessed(key)
	}

	// notify the garbage collector if shard is in a state where it can be reclaimed/gc'd.
	if nAcq := len(s.wAcquire); nAcq == 0 && (s.state == ShardStateAvailable || s.state == ShardStateErrored) {
		d.gcs.NotifyReclaimable(key)
	} else {
		d.gcs.NotifyNotReclaimable(key)
	}
}

func (d *DAGStore) consumeNext() (tsk *task, gc chan *GCResult, error error) {
	select {
	case tsk = <-d.internalCh: // drain internal first; these are tasks emitted from the event loop.
		return tsk, nil, nil
	case <-d.ctx.Done():
		return nil, nil, d.ctx.Err() // TODO drain and process before returning?
	default:
	}

	select {
	case tsk = <-d.externalCh:
		return tsk, nil, nil
	case tsk = <-d.completionCh:
		return tsk, nil, nil
	case gc := <-d.gcCh:
		return nil, gc, nil
	case <-d.ctx.Done():
		return nil, nil, d.ctx.Err() // TODO drain and process before returning?
	}
}

var _ mount.TransientAllocator = (*transientAllocator)(nil)

// transientAllocator submits messages to the event loop to reserve and release space
// for transient downloads when the mount does not know the size of the transient to be downloaded upfront.
type transientAllocator struct {
	d *DAGStore
}

func (t *transientAllocator) Reserve(ctx context.Context, key shard.Key, nPrevReservations int64, toReserve int64) (reserved int64, err error) {
	t.d.lk.Lock()
	s, ok := t.d.shards[key]
	if !ok {
		t.d.lk.Unlock()
		return 0, ErrShardUnknown
	}
	t.d.lk.Unlock()

	out := make(chan *reservationResp, 1)
	tsk := &task{
		op:    OpShardReserveTransient,
		shard: s,
		reservationReq: &reservationReq{
			nPrevReservations: nPrevReservations,
			want:              toReserve,
			response:          out,
		},
	}

	if err := t.d.queueTask(tsk, t.d.completionCh); err != nil {
		return 0, fmt.Errorf("failed to send reservation request: %w", err)
	}

	select {
	case resp := <-out:
		return resp.reserved, resp.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (t *transientAllocator) Release(_ context.Context, key shard.Key, release int64) error {
	t.d.lk.Lock()
	s, ok := t.d.shards[key]
	if !ok {
		t.d.lk.Unlock()
		return ErrShardUnknown
	}
	t.d.lk.Unlock()

	tsk := &task{op: OpShardReleaseTransientReservation, shard: s,
		releaseReq: &releaseReq{release: release}}

	return t.d.queueTask(tsk, t.d.completionCh)
}
