package dagstore

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/filecoin-project/dagstore/mount"
	"github.com/filecoin-project/dagstore/shard"
	"github.com/filecoin-project/dagstore/testdata"
	"github.com/ipfs/go-datastore"
	dsq "github.com/ipfs/go-datastore/query"
	dssync "github.com/ipfs/go-datastore/sync"
	logging "github.com/ipfs/go-log/v2"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

var carv2mnt = &mount.FSMount{FS: testdata.FS, Path: testdata.FSPathCarV2}

func init() {
	_ = logging.SetLogLevel("dagstore", "DEBUG")
}

func TestRegisterUsingExistingTransient(t *testing.T) {
	ds := datastore.NewMapDatastore()
	dagst, err := NewDAGStore(Config{
		MountRegistry: testRegistry(t),
		TransientsDir: t.TempDir(),
		Datastore:     ds,
	})
	require.NoError(t, err)

	ch := make(chan ShardResult, 1)
	k := shard.KeyFromString("foo")
	// even though the fs mount has an empty path, the existing transient will get us through registration.
	err = dagst.RegisterShard(context.Background(), k, &mount.FSMount{FS: testdata.FS, Path: ""}, ch, RegisterOpts{ExistingTransient: testdata.RootPathCarV2})
	require.NoError(t, err)

	res := <-ch
	require.NoError(t, res.Error)
	require.EqualValues(t, k, res.Key)
	require.Nil(t, res.Accessor)
	idx, err := dagst.indices.GetFullIndex(k)
	require.NoError(t, err)
	require.NotNil(t, idx)
}

func TestRegisterCarV1(t *testing.T) {
	ds := datastore.NewMapDatastore()
	dagst, err := NewDAGStore(Config{
		MountRegistry: testRegistry(t),
		TransientsDir: t.TempDir(),
		Datastore:     ds,
	})
	require.NoError(t, err)

	ch := make(chan ShardResult, 1)
	k := shard.KeyFromString("foo")
	err = dagst.RegisterShard(context.Background(), k, &mount.FSMount{FS: testdata.FS, Path: testdata.FSPathCarV1}, ch, RegisterOpts{})
	require.NoError(t, err)

	res := <-ch
	require.NoError(t, res.Error)
	require.EqualValues(t, k, res.Key)
	require.Nil(t, res.Accessor)

	info := dagst.AllShardsInfo()
	require.Len(t, info, 1)
	for _, ss := range info {
		require.Equal(t, ShardStateAvailable, ss.ShardState)
		require.NoError(t, ss.Error)
	}

	// verify index has been persisted
	istat, err := dagst.indices.StatFullIndex(k)
	require.NoError(t, err)
	require.True(t, istat.Exists)
}

func TestRegisterCarV2(t *testing.T) {
	dagst, err := NewDAGStore(Config{
		MountRegistry: testRegistry(t),
		TransientsDir: t.TempDir(),
		Datastore:     datastore.NewMapDatastore(),
	})
	require.NoError(t, err)

	ch := make(chan ShardResult, 1)
	k := shard.KeyFromString("foo")
	err = dagst.RegisterShard(context.Background(), k, carv2mnt, ch, RegisterOpts{})
	require.NoError(t, err)

	res := <-ch
	require.NoError(t, res.Error)
	require.EqualValues(t, k, res.Key)
	require.Nil(t, res.Accessor)

	info := dagst.AllShardsInfo()
	require.Len(t, info, 1)
	for _, ss := range info {
		require.Equal(t, ShardStateAvailable, ss.ShardState)
		require.NoError(t, ss.Error)
	}
	istat, err := dagst.indices.StatFullIndex(k)
	require.NoError(t, err)
	require.True(t, istat.Exists)
}

func TestRegisterConcurrentShards(t *testing.T) {
	run := func(t *testing.T, n int) {
		store := dssync.MutexWrap(datastore.NewMapDatastore())
		dagst, err := NewDAGStore(Config{
			MountRegistry: testRegistry(t),
			TransientsDir: t.TempDir(),
			Datastore:     store,
		})
		require.NoError(t, err)

		registerShards(t, dagst, n, carv2mnt)
	}

	t.Run("1", func(t *testing.T) { run(t, 1) })
	t.Run("2", func(t *testing.T) { run(t, 2) })
	t.Run("4", func(t *testing.T) { run(t, 4) })
	t.Run("8", func(t *testing.T) { run(t, 8) })
	t.Run("16", func(t *testing.T) { run(t, 16) })
	t.Run("32", func(t *testing.T) { run(t, 32) })
	t.Run("64", func(t *testing.T) { run(t, 64) })
	t.Run("128", func(t *testing.T) { run(t, 128) })
	t.Run("256", func(t *testing.T) { run(t, 256) })
}

func TestAcquireInexistentShard(t *testing.T) {
	dagst, err := NewDAGStore(Config{
		MountRegistry: testRegistry(t),
		TransientsDir: t.TempDir(),
		Datastore:     datastore.NewMapDatastore(),
	})
	require.NoError(t, err)

	ch := make(chan ShardResult, 1)
	k := shard.KeyFromString("foo")
	err = dagst.AcquireShard(context.Background(), k, ch, AcquireOpts{})
	require.Error(t, err)
}

func TestAcquireAfterRegisterWait(t *testing.T) {
	dagst, err := NewDAGStore(Config{
		MountRegistry: testRegistry(t),
		TransientsDir: t.TempDir(),
		Datastore:     datastore.NewMapDatastore(),
	})
	require.NoError(t, err)

	ch := make(chan ShardResult, 1)
	k := shard.KeyFromString("foo")
	err = dagst.RegisterShard(context.Background(), k, carv2mnt, ch, RegisterOpts{})
	require.NoError(t, err)

	res := <-ch
	require.NoError(t, res.Error)

	err = dagst.AcquireShard(context.Background(), k, ch, AcquireOpts{})
	require.NoError(t, err)

	res = <-ch
	require.NoError(t, res.Error)
	require.NotNil(t, res.Accessor)
	require.EqualValues(t, k, res.Accessor.Shard())
	err = res.Accessor.Close()
	require.NoError(t, err)
}

func TestConcurrentAcquires(t *testing.T) {
	dagst, err := NewDAGStore(Config{
		MountRegistry: testRegistry(t),
		TransientsDir: t.TempDir(),
	})
	require.NoError(t, err)

	ch := make(chan ShardResult, 1)
	k := shard.KeyFromString("foo")
	err = dagst.RegisterShard(context.Background(), k, carv2mnt, ch, RegisterOpts{})
	require.NoError(t, err)

	res := <-ch
	require.NoError(t, res.Error)

	// the test consists of acquiring then releasing.
	run := func(t *testing.T, n int) {
		accessors := acquireShard(t, dagst, k, n)
		releaseAll(t, dagst, k, accessors)
	}

	t.Run("1", func(t *testing.T) { run(t, 1) })
	t.Run("2", func(t *testing.T) { run(t, 2) })
	t.Run("4", func(t *testing.T) { run(t, 4) })
	t.Run("8", func(t *testing.T) { run(t, 8) })
	t.Run("16", func(t *testing.T) { run(t, 16) })
	t.Run("32", func(t *testing.T) { run(t, 32) })
	t.Run("64", func(t *testing.T) { run(t, 64) })
	t.Run("128", func(t *testing.T) { run(t, 128) })
	t.Run("256", func(t *testing.T) { run(t, 256) })

	info := dagst.AllShardsInfo()
	require.Len(t, info, 1)
	for _, ss := range info {
		require.Equal(t, ShardStateAvailable, ss.ShardState)
		require.NoError(t, ss.Error)
	}
}

func TestRestartRestoresState(t *testing.T) {
	indicesDir := t.TempDir()

	dir := t.TempDir()
	store := datastore.NewLogDatastore(dssync.MutexWrap(datastore.NewMapDatastore()), "trace")
	dagst, err := NewDAGStore(Config{
		MountRegistry: testRegistry(t),
		TransientsDir: dir,
		Datastore:     store,
		IndexDir:      indicesDir,
	})
	require.NoError(t, err)

	keys := registerShards(t, dagst, 100, carv2mnt)
	for _, k := range keys[0:20] { // acquire the first 20 keys.
		_ = acquireShard(t, dagst, k, 4)
	}

	res, err := store.Query(dsq.Query{})
	require.NoError(t, err)
	entries, err := res.Rest()
	require.NoError(t, err)
	require.Len(t, entries, 100) // we have 100 shards.

	// close the DAG store.
	err = dagst.Close()
	require.NoError(t, err)

	// create a new dagstore with the same datastore.
	dagst, err = NewDAGStore(Config{
		MountRegistry: testRegistry(t),
		TransientsDir: dir,
		Datastore:     store,
		IndexDir:      indicesDir,
	})
	require.NoError(t, err)
	info := dagst.AllShardsInfo()
	require.Len(t, info, 100)

	for k, ss := range info {
		require.Equal(t, ShardStateAvailable, ss.ShardState)
		require.NoError(t, ss.Error)
		require.Zero(t, ss.refs)

		// also ensure we have indices for all the shards.
		idx, err := dagst.indices.GetFullIndex(k)
		require.NoError(t, err)
		require.NotNil(t, idx)

		// ensure we can acquire the shard again
		_ = acquireShard(t, dagst, k, 10)

		// ensure we can't register the shard again
		err = dagst.RegisterShard(context.Background(), k, carv2mnt, nil, RegisterOpts{})
		require.Error(t, err)
		require.Contains(t, err.Error(), ErrShardExists.Error())
	}
}

func TestRestartResumesRegistration(t *testing.T) {
	dir := t.TempDir()
	store := datastore.NewLogDatastore(dssync.MutexWrap(datastore.NewMapDatastore()), "trace")
	r := testRegistry(t)

	err := r.Register("block", newBlockingMount(&mount.FSMount{FS: testdata.FS}))
	require.NoError(t, err)

	sink := tracer(128)
	dagst, err := NewDAGStore(Config{
		MountRegistry: r,
		TransientsDir: dir,
		Datastore:     store,
		TraceCh:       sink,
	})
	require.NoError(t, err)

	// start registering a shard -> registration will not complete as mount.Fetch will hang.
	k := shard.KeyFromString("test")
	ch := make(chan ShardResult, 1)
	block := newBlockingMount(carv2mnt)
	err = dagst.RegisterShard(context.Background(), k, block, ch, RegisterOpts{})
	require.NoError(t, err)

	// receive at most one trace in 1 second.
	traces := make([]Trace, 16)
	n, timedOut := sink.Read(traces, 1*time.Second)
	require.Equal(t, 1, n)
	require.True(t, timedOut)

	// no OpMakeAvailable trace; shard state is initializing.
	require.Equal(t, OpShardRegister, traces[0].Op)
	require.Equal(t, ShardStateInitializing, traces[0].After.ShardState)

	// corroborate we see the same through the API.
	info, err := dagst.GetShardInfo(k)
	require.NoError(t, err)
	require.EqualValues(t, ShardStateInitializing, info.ShardState)

	// close the dagstore and remove the transients.
	err = dagst.Close()
	require.NoError(t, err)
	// require.NoError(t, os.RemoveAll(dir))

	// start a new DAGStore and do not block the fetch this time -> registration should work.
	// create a new dagstore with the same datastore.
	//
	// Instantiate a new registry using a blocking mount that we control as
	// a template. Because UnblockCh is exported, it is a templated field, so
	// all mounts will await for tokens on that shared channel.
	r = testRegistry(t)
	bm := newBlockingMount(&mount.FSMount{FS: testdata.FS})
	err = r.Register("block", bm)
	require.NoError(t, err)

	// unblock the mount this time!
	bm.UnblockNext(1)

	dagst, err = NewDAGStore(Config{
		MountRegistry: r,
		TransientsDir: dir,
		Datastore:     store,
		TraceCh:       sink,
	})
	require.NoError(t, err)

	// this time we will receive two traces; OpRegister and OpMakeAvailable.
	n, timedOut = sink.Read(traces, 1*time.Second)
	require.Equal(t, 2, n)
	require.True(t, timedOut)

	// trace 1.
	require.Equal(t, OpShardRegister, traces[0].Op)
	require.Equal(t, ShardStateInitializing, traces[0].After.ShardState)

	// trace 2.
	require.Equal(t, OpShardMakeAvailable, traces[1].Op)
	require.Equal(t, ShardStateAvailable, traces[1].After.ShardState)

	// ensure we have indices.
	idx, err := dagst.indices.GetFullIndex(k)
	require.NoError(t, err)
	require.NotNil(t, idx)

	// now let's acquire the shard, and ensure we receive two more traces.
	// one for OpAcquire, one for OpRelease.
	accessors := acquireShard(t, dagst, k, 1)
	releaseAll(t, dagst, k, accessors)

	n, timedOut = sink.Read(traces, 1*time.Second)
	require.Equal(t, 2, n)
	require.True(t, timedOut)

	// trace 1.
	require.Equal(t, OpShardAcquire, traces[0].Op)
	require.Equal(t, ShardStateServing, traces[0].After.ShardState)

	// trace 2.
	require.Equal(t, OpShardRelease, traces[1].Op)
	require.Equal(t, ShardStateAvailable, traces[1].After.ShardState)
}

// TestBlockCallback tests that blocking a callback blocks the dispatcher
// but not the event loop.
func TestBlockCallback(t *testing.T) {
	t.Skip("TODO")
}

// registerShards registers n shards concurrently, using the CARv2 mount.
func registerShards(t *testing.T, dagst *DAGStore, n int, mnt mount.Mount) (ret []shard.Key) {
	grp, _ := errgroup.WithContext(context.Background())
	for i := 0; i < n; i++ {
		k := shard.KeyFromString(fmt.Sprintf("shard-%d", i))
		grp.Go(func() error {
			ch := make(chan ShardResult, 1)
			err := dagst.RegisterShard(context.Background(), k, mnt, ch, RegisterOpts{})
			if err != nil {
				return err
			}
			res := <-ch
			return res.Error
		})
		ret = append(ret, k)
	}

	require.NoError(t, grp.Wait())

	info := dagst.AllShardsInfo()
	require.Len(t, info, n)
	for k, ss := range info {
		require.Equal(t, ShardStateAvailable, ss.ShardState)
		require.NoError(t, ss.Error)
		istat, err := dagst.indices.StatFullIndex(k)
		require.NoError(t, err)
		require.True(t, istat.Exists)
	}

	return ret
}

// acquireShard acquires the shard known by key `k` concurrently `n` times.
func acquireShard(t *testing.T, dagst *DAGStore, k shard.Key, n int) []*ShardAccessor {
	accessors := make([]*ShardAccessor, n)

	// acquire
	grp, _ := errgroup.WithContext(context.Background())
	for i := 0; i < n; i++ {
		i := i
		grp.Go(func() error {
			ch := make(chan ShardResult, 1)
			err := dagst.AcquireShard(context.Background(), k, ch, AcquireOpts{})
			if err != nil {
				return err
			}

			res := <-ch
			if res.Error != nil {
				return res.Error
			}

			bs, err := res.Accessor.Blockstore()
			if err != nil {
				return err
			}

			state, err := dagst.GetShardInfo(k)
			if err != nil {
				return err
			} else if state.ShardState != ShardStateServing {
				return fmt.Errorf("expected state ShardStateServing; was: %d", state.ShardState)
			}

			if _, err := bs.Get(testdata.RootCID); err != nil {
				return err
			}

			accessors[i] = res.Accessor
			return nil
		})
	}

	require.NoError(t, grp.Wait())

	// check shard state.
	info, err := dagst.GetShardInfo(k)
	require.NoError(t, err)
	require.Equal(t, ShardStateServing, info.ShardState)
	require.NoError(t, info.Error)
	// refs should be equal to number of acquirers since we've not closed any acquirer/released any shard.
	require.EqualValues(t, n, info.refs)

	return accessors
}

// releaseAll releases all accessors for a given shard.
func releaseAll(t *testing.T, dagst *DAGStore, k shard.Key, accs []*ShardAccessor) {
	grp, _ := errgroup.WithContext(context.Background())
	for _, acc := range accs {
		// close all accessors.
		grp.Go(acc.Close)
	}

	require.NoError(t, grp.Wait())
	require.Eventually(t, func() bool {
		info, err := dagst.GetShardInfo(k)
		return err == nil && info.ShardState == ShardStateAvailable && info.refs == 0
	}, 5*time.Second, 100*time.Millisecond)

	// // refs should be zero now since shard accessors have been closed and transient file should be cleaned up.
	// require.Zero(t, info.refs)
	// _, err = os.Stat(abs)
	// require.Error(t, err)
	//
}

func testRegistry(t *testing.T) *mount.Registry {
	r := mount.NewRegistry()
	err := r.Register("fs", &mount.FSMount{FS: testdata.FS})
	require.NoError(t, err)
	return r
}

type Tracer chan Trace

func tracer(buf int) Tracer {
	return make(chan Trace, buf)
}

// Read drains as many traces as len(out), at most. It returns how many
// traces were copied into the slice, and updates the internal read
// counter.
func (m Tracer) Read(dst []Trace, timeout time.Duration) (n int, timedOut bool) {
	for i := range dst {
		select {
		case dst[i] = <-m:
		case <-time.After(timeout):
			return i, true
		}
	}
	return len(dst), false
}

// blockingMount is a mount that proxies to another mount, but it blocks by
// default, unless unblock tokens are added via UnblockNext.
type blockingMount struct {
	mount.Mount
	UnblockCh chan struct{} // exported so that it is a templated field for mounts that were restored after a restart.
}

func newBlockingMount(mnt mount.Mount) *blockingMount {
	return &blockingMount{Mount: mnt, UnblockCh: make(chan struct{})}
}

// UnblockNext allows as many calls to Fetch() as n to proceed.
func (b *blockingMount) UnblockNext(n int) {
	go func() {
		for i := 0; i < n; i++ {
			b.UnblockCh <- struct{}{}
		}
	}()
}

func (b *blockingMount) Fetch(ctx context.Context) (mount.Reader, error) {
	<-b.UnblockCh
	return b.Mount.Fetch(ctx)
}
