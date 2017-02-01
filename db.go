// Package tsdb implements a time series storage for float64 sample data.
package tsdb

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sync/errgroup"

	"github.com/coreos/etcd/pkg/fileutil"
	"github.com/fabxc/tsdb/labels"
	"github.com/go-kit/kit/log"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
)

// DefaultOptions used for the DB. They are sane for setups using
// millisecond precision timestampdb.
var DefaultOptions = &Options{
	WALFlushInterval: 5 * time.Second,
	MinBlockDuration: 3 * 60 * 60 * 1000,  // 2 hours in milliseconds
	MaxBlockDuration: 48 * 60 * 60 * 1000, // 1 day in milliseconds
	GracePeriod:      2 * 60 * 60 * 1000,  // 2 hours in milliseconds
}

// Options of the DB storage.
type Options struct {
	// The interval at which the write ahead log is flushed to disc.
	WALFlushInterval time.Duration

	// The timestamp range of head blocks after which they get persisted.
	// It's the minimum duration of any persisted block.
	MinBlockDuration uint64

	// The maximum timestamp range of compacted blocks.
	MaxBlockDuration uint64

	// Time window between the highest timestamp and the minimum timestamp
	// that can still be appended.
	GracePeriod uint64
}

// Appender allows appending a batch of data. It must be completed with a
// call to Commit or Rollback and must not be reused afterwards.
type Appender interface {
	// Add adds a sample pair for the given series. A reference number is
	// returned which can be used to add further samples in the same or later
	// transactions.
	// Returned reference numbers are ephemeral and may be rejected in calls
	// to AddFast() at any point. Adding the sample via Add() returns a new
	// reference number.
	Add(l labels.Labels, t int64, v float64) (uint64, error)

	// Add adds a sample pair for the referenced series. It is generally faster
	// than adding a sample by providing its full label set.
	AddFast(ref uint64, t int64, v float64) error

	// Commit submits the collected samples and purges the batch.
	Commit() error

	// Rollback rolls back all modifications made in the appender so far.
	Rollback() error
}

const sep = '\xff'

// DB handles reads and writes of time series falling into
// a hashed partition of a seriedb.
type DB struct {
	dir     string
	logger  log.Logger
	metrics *dbMetrics
	opts    *Options

	mtx       sync.RWMutex
	persisted []*persistedBlock
	heads     []*headBlock
	headGen   uint8

	compactor *compactor

	compactc chan struct{}
	cutc     chan struct{}
	donec    chan struct{}
	stopc    chan struct{}
}

type dbMetrics struct {
	samplesAppended      prometheus.Counter
	compactionsTriggered prometheus.Counter
}

func newDBMetrics(r prometheus.Registerer) *dbMetrics {
	m := &dbMetrics{}

	m.samplesAppended = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tsdb_samples_appended_total",
		Help: "Total number of appended sampledb.",
	})
	m.compactionsTriggered = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tsdb_compactions_triggered_total",
		Help: "Total number of triggered compactions for the partition.",
	})

	if r != nil {
		r.MustRegister(
			m.samplesAppended,
			m.compactionsTriggered,
		)
	}
	return m
}

// Open returns a new DB in the given directory.
func Open(dir string, logger log.Logger, opts *Options) (db *DB, err error) {
	if !fileutil.Exist(dir) {
		if err := os.MkdirAll(dir, 0777); err != nil {
			return nil, err
		}
	}
	var r prometheus.Registerer
	// r := prometheus.DefaultRegisterer

	if opts == nil {
		opts = DefaultOptions
	}

	db = &DB{
		dir:      dir,
		logger:   logger,
		metrics:  newDBMetrics(r),
		opts:     opts,
		compactc: make(chan struct{}, 1),
		cutc:     make(chan struct{}, 1),
		donec:    make(chan struct{}),
		stopc:    make(chan struct{}),
	}
	db.compactor = newCompactor(r, &compactorOptions{
		maxBlockRange: opts.MaxBlockDuration,
	})

	if err := db.initBlocks(); err != nil {
		return nil, err
	}

	go db.run()

	return db, nil
}

func (db *DB) run() {
	defer close(db.donec)

	// go func() {
	// 	for {
	// 		select {
	// 		case <-db.cutc:
	// 			db.mtx.Lock()
	// 			_, err := db.cut()
	// 			db.mtx.Unlock()

	// 			if err != nil {
	// 				db.logger.Log("msg", "cut failed", "err", err)
	// 			} else {
	// 				select {
	// 				case db.compactc <- struct{}{}:
	// 				default:
	// 				}
	// 			}
	// 			// Drain cut channel so we don't trigger immediately again.
	// 			select {
	// 			case <-db.cutc:
	// 			default:
	// 			}
	// 		case <-db.stopc:
	// 		}
	// 	}
	// }()

	for {
		select {
		case <-db.compactc:
			db.metrics.compactionsTriggered.Inc()

			var infos []compactionInfo
			for _, b := range db.compactable() {
				m := b.Meta()

				infos = append(infos, compactionInfo{
					generation: m.Compaction.Generation,
					mint:       m.MinTime,
					maxt:       m.MaxTime,
				})
			}

			i, j, ok := db.compactor.pick(infos)
			if !ok {
				continue
			}
			db.logger.Log("msg", "picked", "i", i, "j", j)
			for k := i; k < j; k++ {
				db.logger.Log("k", k, "generation", infos[k].generation)
			}

			if err := db.compact(i, j); err != nil {
				db.logger.Log("msg", "compaction failed", "err", err)
				continue
			}
			db.logger.Log("msg", "compaction completed")
			// Trigger another compaction in case there's more work to do.
			select {
			case db.compactc <- struct{}{}:
			default:
			}

		case <-db.stopc:
			return
		}
	}
}

func (db *DB) getBlock(i int) Block {
	if i < len(db.persisted) {
		return db.persisted[i]
	}
	return db.heads[i-len(db.persisted)]
}

// removeBlocks removes the blocks in range [i, j) from the list of persisted
// and head blocks. The blocks are not closed and their files not deleted.
func (db *DB) removeBlocks(i, j int) {
	for k := i; k < j; k++ {
		if i < len(db.persisted) {
			db.persisted = append(db.persisted[:i], db.persisted[i+1:]...)
		} else {
			l := i - len(db.persisted)
			db.heads = append(db.heads[:l], db.heads[l+1:]...)
		}
	}
}

func (db *DB) blocks() (bs []Block) {
	for _, b := range db.persisted {
		bs = append(bs, b)
	}
	for _, b := range db.heads {
		bs = append(bs, b)
	}
	return bs
}

// compact block in range [i, j) into a temporary directory and atomically
// swap the blocks out on successful completion.
func (db *DB) compact(i, j int) error {
	if j <= i {
		return errors.New("invalid compaction block range")
	}
	var blocks []Block
	for k := i; k < j; k++ {
		blocks = append(blocks, db.getBlock(k))
	}
	var (
		dir    = blocks[0].Dir()
		tmpdir = dir + ".tmp"
	)

	if err := db.compactor.compact(tmpdir, blocks...); err != nil {
		return err
	}

	pb, err := newPersistedBlock(tmpdir)
	if err != nil {
		return err
	}

	db.mtx.Lock()
	defer db.mtx.Unlock()

	if err := renameDir(tmpdir, dir); err != nil {
		return errors.Wrap(err, "rename dir")
	}
	pb.dir = dir

	db.removeBlocks(i, j)
	db.persisted = append(db.persisted, pb)

	for i, b := range blocks {
		if err := b.Close(); err != nil {
			return errors.Wrap(err, "close old block")
		}
		if i > 0 {
			if err := os.RemoveAll(b.Dir()); err != nil {
				return errors.Wrap(err, "removing old block")
			}
		}
	}
	return nil
}

func (db *DB) initBlocks() error {
	var (
		persisted []*persistedBlock
		heads     []*headBlock
	)

	dirs, err := blockDirs(db.dir)
	if err != nil {
		return err
	}

	for _, dir := range dirs {
		if fileutil.Exist(filepath.Join(dir, walFileName)) {
			h, err := openHeadBlock(dir, db.logger)
			if err != nil {
				return err
			}
			h.generation = db.headGen
			db.headGen++
			heads = append(heads, h)
			continue
		}
		b, err := newPersistedBlock(dir)
		if err != nil {
			return err
		}
		persisted = append(persisted, b)
	}

	db.persisted = persisted
	db.heads = heads

	// if len(heads) == 0 {
	// 	_, err = db.cut()
	// }
	return nil
}

// Close the partition.
func (db *DB) Close() error {
	close(db.stopc)
	<-db.donec

	var merr MultiError

	db.mtx.Lock()
	defer db.mtx.Unlock()

	for _, pb := range db.persisted {
		merr.Add(pb.Close())
	}
	for _, hb := range db.heads {
		merr.Add(hb.Close())
	}

	return merr.Err()
}

// Appender returns a new Appender on the database.
func (db *DB) Appender() Appender {
	db.mtx.RLock()
	a := &dbAppender{db: db}

	for _, b := range db.appendable() {
		a.heads = append(a.heads, b.Appender().(*headAppender))
	}
	return a
}

type dbAppender struct {
	db *DB
	// gen  uint8
	// head *headAppender
	maxGen uint8

	heads []*headAppender
}

func (a *dbAppender) Add(lset labels.Labels, t int64, v float64) (uint64, error) {
	h, err := a.appenderFor(t)
	if err != nil {
		fmt.Println("no appender")
		return 0, err
	}
	ref, err := h.Add(lset, t, v)
	if err != nil {
		return 0, err
	}
	return ref | (uint64(h.generation) << 40), nil
}

func (a *dbAppender) hashedAdd(hash uint64, lset labels.Labels, t int64, v float64) (uint64, error) {
	h, err := a.appenderFor(t)
	if err != nil {
		fmt.Println("no appender")
		return 0, err
	}
	ref, err := h.hashedAdd(hash, lset, t, v)
	if err != nil {
		return 0, err
	}
	return ref | (uint64(h.generation) << 40), nil
}

func (a *dbAppender) AddFast(ref uint64, t int64, v float64) error {
	// We store the head generation in the 4th byte and use it to reject
	// stale references.
	gen := uint8((ref << 16) >> 56)

	h, err := a.appenderFor(t)
	if err != nil {
		return err
	}
	// fmt.Println("check gen", h.generation, gen)
	if h.generation != gen {
		return ErrNotFound
	}

	return h.AddFast(ref, t, v)
}

// appenderFor gets the appender for the head containing timestamp t.
// If the head block doesn't exist yet, it gets created.
func (a *dbAppender) appenderFor(t int64) (*headAppender, error) {
	if len(a.heads) == 0 {
		if err := a.addNextHead(t); err != nil {
			return nil, err
		}
		return a.appenderFor(t)
	}
	for i := len(a.heads) - 1; i >= 0; i-- {
		h := a.heads[i]

		if t > h.meta.MaxTime {
			if err := a.addNextHead(t); err != nil {
				return nil, err
			}
			return a.appenderFor(t)
		}
		if t >= h.meta.MinTime {
			return h, nil
		}
	}

	return nil, ErrNotFound
}

func (a *dbAppender) addNextHead(t int64) error {
	a.db.mtx.RUnlock()
	a.db.mtx.Lock()

	// We switched locks, validate that adding a head for the timestamp
	// is still required.
	if len(a.db.heads) > 1 {
		h := a.db.heads[len(a.db.heads)-1]
		if t <= h.meta.MaxTime {
			a.heads = append(a.heads, h.Appender().(*headAppender))
			a.maxGen++
			a.db.mtx.Unlock()
			a.db.mtx.RLock()
			return nil
		}
	}

	h, err := a.db.cut(t)
	if err == nil {
		a.heads = append(a.heads, h.Appender().(*headAppender))
		a.maxGen++
	}

	a.db.mtx.Unlock()
	a.db.mtx.RLock()

	return err
}

func (a *dbAppender) Commit() error {
	var merr MultiError

	for _, h := range a.heads {
		merr.Add(h.Commit())
	}
	a.db.mtx.RUnlock()

	return merr.Err()
}

func (a *dbAppender) Rollback() error {
	var merr MultiError

	for _, h := range a.heads {
		merr.Add(h.Rollback())
	}
	a.db.mtx.RUnlock()

	return merr.Err()
}

func (db *DB) appendable() []*headBlock {
	if len(db.heads) == 0 {
		return nil
	}
	var blocks []*headBlock
	maxHead := db.heads[len(db.heads)-1]

	k := len(db.heads) - 2
	for i := k; i >= 0; i-- {
		if db.heads[i].meta.MaxTime < maxHead.meta.MinTime-int64(db.opts.GracePeriod) {
			break
		}
		k--
	}
	for i := k + 1; i < len(db.heads); i++ {
		blocks = append(blocks, db.heads[i])
	}
	return blocks
}

func (db *DB) compactable() []Block {
	db.mtx.RLock()
	defer db.mtx.RUnlock()

	var blocks []Block
	for _, pb := range db.persisted {
		blocks = append(blocks, pb)
	}

	maxHead := db.heads[len(db.heads)-1]

	k := len(db.heads) - 2
	for i := k; i >= 0; i-- {
		if db.heads[i].meta.MaxTime < maxHead.meta.MinTime-int64(db.opts.GracePeriod) {
			break
		}
		k--
	}
	for i, hb := range db.heads[:len(db.heads)-1] {
		if i > k {
			break
		}
		blocks = append(blocks, hb)
	}

	return blocks
}

func intervalOverlap(amin, amax, bmin, bmax int64) bool {
	if bmin >= amin && bmin <= amax {
		return true
	}
	if amin >= bmin && amin <= bmax {
		return true
	}
	return false
}

func intervalContains(min, max, t int64) bool {
	return t >= min && t <= max
}

// blocksForInterval returns all blocks within the partition that may contain
// data for the given time range.
func (db *DB) blocksForInterval(mint, maxt int64) []Block {
	var bs []Block

	for _, b := range db.persisted {
		m := b.Meta()
		if intervalOverlap(mint, maxt, m.MinTime, m.MaxTime) {
			bs = append(bs, b)
		}
	}
	for _, b := range db.heads {
		m := b.Meta()
		if intervalOverlap(mint, maxt, m.MinTime, m.MaxTime) {
			bs = append(bs, b)
		}
	}

	return bs
}

// cut starts a new head block to append to. The completed head block
// will still be appendable for the configured grace period.
func (db *DB) cut(mint int64) (*headBlock, error) {
	maxt := mint + int64(db.opts.MinBlockDuration) - 1
	fmt.Println("cut", mint, maxt)

	dir, seq, err := nextBlockDir(db.dir)
	if err != nil {
		return nil, err
	}
	newHead, err := createHeadBlock(dir, seq, db.logger, mint, maxt)
	if err != nil {
		return nil, err
	}

	db.heads = append(db.heads, newHead)
	db.headGen++

	newHead.generation = db.headGen

	fmt.Println("headlen", len(db.heads))

	select {
	case db.compactc <- struct{}{}:
	default:
	}

	return newHead, nil
}

func isBlockDir(fi os.FileInfo) bool {
	if !fi.IsDir() {
		return false
	}
	if !strings.HasPrefix(fi.Name(), "b-") {
		return false
	}
	if _, err := strconv.ParseUint(fi.Name()[2:], 10, 32); err != nil {
		return false
	}
	return true
}

func blockDirs(dir string) ([]string, error) {
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var dirs []string

	for _, fi := range files {
		if isBlockDir(fi) {
			dirs = append(dirs, filepath.Join(dir, fi.Name()))
		}
	}
	return dirs, nil
}

func nextBlockDir(dir string) (string, int, error) {
	names, err := fileutil.ReadDir(dir)
	if err != nil {
		return "", 0, err
	}

	i := uint64(0)
	for _, n := range names {
		if !strings.HasPrefix(n, "b-") {
			continue
		}
		j, err := strconv.ParseUint(n[2:], 10, 32)
		if err != nil {
			continue
		}
		i = j
	}
	return filepath.Join(dir, fmt.Sprintf("b-%0.6d", i+1)), int(i + 1), nil
}

// PartitionedDB is a time series storage.
type PartitionedDB struct {
	logger log.Logger
	dir    string

	partitionPow uint
	Partitions   []*DB
}

func isPowTwo(x int) bool {
	return x > 0 && (x&(x-1)) == 0
}

// OpenPartitioned or create a new DB.
func OpenPartitioned(dir string, n int, l log.Logger, opts *Options) (*PartitionedDB, error) {
	if !isPowTwo(n) {
		return nil, errors.Errorf("%d is not a power of two", n)
	}
	if opts == nil {
		opts = DefaultOptions
	}
	if l == nil {
		l = log.NewLogfmtLogger(os.Stdout)
		l = log.NewContext(l).With("ts", log.DefaultTimestampUTC, "caller", log.DefaultCaller)
	}

	if err := os.MkdirAll(dir, 0777); err != nil {
		return nil, err
	}
	c := &PartitionedDB{
		logger:       l,
		dir:          dir,
		partitionPow: uint(math.Log2(float64(n))),
	}

	// Initialize vertical partitiondb.
	// TODO(fabxc): validate partition number to be power of 2, which is required
	// for the bitshift-modulo when finding the right partition.
	for i := 0; i < n; i++ {
		l := log.NewContext(l).With("partition", i)
		d := partitionDir(dir, i)

		s, err := Open(d, l, opts)
		if err != nil {
			return nil, fmt.Errorf("initializing partition %q failed: %s", d, err)
		}

		c.Partitions = append(c.Partitions, s)
	}

	return c, nil
}

func partitionDir(base string, i int) string {
	return filepath.Join(base, fmt.Sprintf("p-%0.4d", i))
}

// Close the database.
func (db *PartitionedDB) Close() error {
	var g errgroup.Group

	for _, partition := range db.Partitions {
		g.Go(partition.Close)
	}

	return g.Wait()
}

// Appender returns a new appender against the database.
func (db *PartitionedDB) Appender() Appender {
	app := &partitionedAppender{db: db}

	for _, p := range db.Partitions {
		app.partitions = append(app.partitions, p.Appender().(*dbAppender))
	}
	return app
}

type partitionedAppender struct {
	db         *PartitionedDB
	partitions []*dbAppender
}

func (a *partitionedAppender) Add(lset labels.Labels, t int64, v float64) (uint64, error) {
	h := lset.Hash()
	p := h >> (64 - a.db.partitionPow)

	ref, err := a.partitions[p].hashedAdd(h, lset, t, v)
	if err != nil {
		return 0, err
	}
	return ref | (p << 48), nil
}

func (a *partitionedAppender) AddFast(ref uint64, t int64, v float64) error {
	p := uint8((ref << 8) >> 56)
	return a.partitions[p].AddFast(ref, t, v)
}

func (a *partitionedAppender) Commit() error {
	var merr MultiError

	for _, p := range a.partitions {
		merr.Add(p.Commit())
	}
	return merr.Err()
}

func (a *partitionedAppender) Rollback() error {
	var merr MultiError

	for _, p := range a.partitions {
		merr.Add(p.Rollback())
	}
	return merr.Err()
}

// The MultiError type implements the error interface, and contains the
// Errors used to construct it.
type MultiError []error

// Returns a concatenated string of the contained errors
func (es MultiError) Error() string {
	var buf bytes.Buffer

	if len(es) > 1 {
		fmt.Fprintf(&buf, "%d errors: ", len(es))
	}

	for i, err := range es {
		if i != 0 {
			buf.WriteString("; ")
		}
		buf.WriteString(err.Error())
	}

	return buf.String()
}

// Add adds the error to the error list if it is not nil.
func (es *MultiError) Add(err error) {
	if err == nil {
		return
	}
	if merr, ok := err.(MultiError); ok {
		*es = append(*es, merr...)
	} else {
		*es = append(*es, err)
	}
}

// Err returns the error list as an error or nil if it is empty.
func (es MultiError) Err() error {
	if len(es) == 0 {
		return nil
	}
	return es
}

func yoloString(b []byte) string {
	sh := (*reflect.SliceHeader)(unsafe.Pointer(&b))

	h := reflect.StringHeader{
		Data: sh.Data,
		Len:  sh.Len,
	}
	return *((*string)(unsafe.Pointer(&h)))
}
